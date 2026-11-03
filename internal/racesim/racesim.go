// Package racesim runs real goroutines against a shared resume record to show
// what a data race does and how sync.Mutex and channel ownership fix it.
//
// Each field has two valid values (before/after an update, e.g. a city
// change). Writers copy one value into the field byte by byte, yielding to
// the scheduler between bytes, the way a long write can be interrupted. A
// view counter is incremented with read, yield, write.
//
//   - Unsafe: no synchronisation. Writes interleave, so a field can end up
//     as a mix of both values (a torn write) and counter increments are lost.
//   - Mutex: each field write and increment holds a sync.Mutex.
//   - Channel: a single owner goroutine applies all writes it receives on a
//     channel; nothing else touches the record.
//
// It compiles to WebAssembly for the browser demo and runs natively in tests.
// The Unsafe mode is a deliberate data race; `go test -race` would flag it,
// which is the point, so its test is excluded from race builds.
package racesim

import (
	"runtime"
	"sync"
)

type Mode string

const (
	Unsafe  Mode = "none"
	Mutex   Mode = "mutex"
	Channel Mode = "chan"
)

type Field struct {
	Name     string
	Versions [2]string
}

var DefaultFields = []Field{
	{"role", [2]string{"Full-Stack Engineer (Go)", "Full-Stack Engineer, Go!"}},
	{"focus", [2]string{"Retrieval, Memory, Pipelines", "Pipelines, Retrieval, Memory"}},
	{"stack", [2]string{"Go, Temporal, OpenSearch, Postgres", "Go + Temporal + OpenSearch + PG"}},
	{"based", [2]string{"Tiruppur", "Bengaluru"}},
	{"evals", [2]string{"hit@5 0.977, stale 0.000", "MRR 0.925, F1 0.96-0.996"}},
}

type Record struct {
	mu     sync.Mutex
	fields []Field
	vals   [][]byte
	views  int
}

func NewRecord(fields []Field) *Record {
	r := &Record{fields: fields}
	for _, f := range fields {
		r.vals = append(r.vals, []byte(f.Versions[0]))
	}
	return r
}

type write struct {
	field, version int
	done           chan struct{}
}

// copyBytes writes v into dst one byte at a time, yielding between bytes.
func copyBytes(dst *[]byte, v string) {
	if len(*dst) != len(v) {
		*dst = make([]byte, len(v))
	}
	b := *dst
	for i := 0; i < len(v); i++ {
		b[i] = v[i]
		runtime.Gosched()
	}
}

// Step runs `writers` goroutines, each doing one field write and one view
// increment, and waits for all of them.
func (r *Record) Step(mode Mode, writers int, pick func(i int) (field, version int)) {
	var wg sync.WaitGroup
	var ch chan write
	var owner sync.WaitGroup
	if mode == Channel {
		ch = make(chan write)
		owner.Add(1)
		go func() {
			defer owner.Done()
			for w := range ch {
				copyBytes(&r.vals[w.field], r.fields[w.field].Versions[w.version])
				r.views++
				close(w.done)
			}
		}()
	}
	for i := 0; i < writers; i++ {
		f, v := pick(i)
		i := i
		wg.Add(1)
		go func() {
			defer wg.Done()
			switch mode {
			case Mutex:
				r.mu.Lock()
				copyBytes(&r.vals[f], r.fields[f].Versions[v])
				r.views++
				r.mu.Unlock()
			case Channel:
				d := make(chan struct{})
				ch <- write{f, v, d}
				<-d
			default:
				// Lengths differ between versions; allocate once so concurrent
				// writers share one buffer, as two writers to one field would.
				buf := r.vals[f]
				if len(buf) != len(r.fields[f].Versions[v]) {
					buf = make([]byte, len(r.fields[f].Versions[v]))
					r.vals[f] = buf
				}
				val := r.fields[f].Versions[v]
				// Writers run at uneven speeds, as preempted threads do, so
				// different writers finish different bytes last.
				for j := 0; j < len(val) && j < len(buf); j++ {
					buf[j] = val[j]
					for k := 0; k <= (i*31+j*17)%3; k++ {
						runtime.Gosched()
					}
				}
				n := r.views
				runtime.Gosched()
				r.views = n + 1
			}
		}()
	}
	wg.Wait()
	if ch != nil {
		close(ch)
		owner.Wait()
	}
}

type FieldState struct {
	Name  string `json:"name"`
	Value string `json:"value"`
	Want  string `json:"want"` // the valid version closest to Value
	Torn  bool   `json:"torn"`
}

type Snapshot struct {
	Fields []FieldState `json:"fields"`
	Views  int          `json:"views"`
}

func (r *Record) Snapshot() Snapshot {
	r.mu.Lock()
	defer r.mu.Unlock()
	s := Snapshot{Views: r.views}
	for i, f := range r.fields {
		v := string(r.vals[i])
		want := f.Versions[0]
		if diff(v, f.Versions[1]) < diff(v, want) {
			want = f.Versions[1]
		}
		s.Fields = append(s.Fields, FieldState{Name: f.Name, Value: v, Want: want, Torn: v != f.Versions[0] && v != f.Versions[1]})
	}
	return s
}

// Reset restores every field to its first version.
func (r *Record) Reset() {
	r.mu.Lock()
	defer r.mu.Unlock()
	for i, f := range r.fields {
		r.vals[i] = []byte(f.Versions[0])
	}
	r.views = 0
}

func diff(a, b string) int {
	d := 0
	for i := 0; i < max(len(a), len(b)); i++ {
		if i >= len(a) || i >= len(b) || a[i] != b[i] {
			d++
		}
	}
	return d
}
