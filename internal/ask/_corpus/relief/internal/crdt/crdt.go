// Package crdt is the replicated document behind relief sync.
//
// Design: an operation-based CRDT whose state is a set of operations.
//
//   - Every change is an Op stamped with a Dot: (device, seq), where seq
//     counts that device's changes 1, 2, 3, ... Dots are unique.
//
//   - A device's knowledge is a version vector: for each device, the highest
//     seq it holds. Because ops from a device are always delivered in seq
//     order, "I have seq <= vv[d]" describes exactly which ops it has.
//
//   - Sync: send your version vector, receive every op the peer has beyond
//     it. Nothing is sent twice, and a third device can relay ops it got
//     from someone else (two camps sync through a passing truck).
//
//   - The visible state is computed from the op set with rules that do not
//     depend on order, so two devices with the same ops always show the same
//     state:
//
//     stock      counter per item: sum of all +/- changes (nothing is lost
//     when two devices change the same item offline)
//     needs      add-wins set: a need is open if some "add" exists that no
//     "done" had seen (a new request made offline is not erased by
//     someone closing an older one)
//     fields     last-writer-wins by hybrid logical clock (camp status,
//     contact): the latest edit wins even if device clocks are off
package crdt

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"
)

type Dot struct {
	Device string `json:"d"`
	Seq    uint64 `json:"s"`
}

func (d Dot) String() string { return fmt.Sprintf("%s:%d", d.Device, d.Seq) }

type Kind string

const (
	Stock   Kind = "stock" // Key=item, Delta=+/- quantity
	NeedAdd Kind = "need+" // Key=need
	NeedDel Kind = "need-" // Key=need, Seen=dots of the adds it closes
	Set     Kind = "set"   // Key=field, Value, HLC
)

type Op struct {
	Dot   Dot    `json:"dot"`
	Kind  Kind   `json:"k"`
	Key   string `json:"key"`
	Delta int64  `json:"n,omitempty"`
	Value string `json:"v,omitempty"`
	HLC   HLC    `json:"t,omitempty"`
	Seen  []Dot  `json:"seen,omitempty"`
}

type VV map[string]uint64

func (v VV) Copy() VV {
	c := make(VV, len(v))
	for k, x := range v {
		c[k] = x
	}
	return c
}

type lww struct {
	Value string
	HLC   HLC
	Dev   string
}

func (a lww) less(b lww) bool {
	if a.HLC != b.HLC {
		return a.HLC.Less(b.HLC)
	}
	return a.Dev < b.Dev
}

// Doc is one device's replica.
type Doc struct {
	Device string
	clock  *Clock
	vv     VV
	ops    map[string][]Op // device -> ops in seq order (index i holds seq i+1)

	// materialised view, updated as ops arrive (all updates commute)
	stock  map[string]int64
	adds   map[string]map[Dot]bool // need -> live add dots
	closed map[Dot]bool            // add dots closed by some NeedDel
	fields map[string]lww
}

func New(device string, clock *Clock) *Doc {
	if clock == nil {
		clock = NewClock(nil)
	}
	return &Doc{Device: device, clock: clock, vv: VV{}, ops: map[string][]Op{},
		stock: map[string]int64{}, adds: map[string]map[Dot]bool{}, closed: map[Dot]bool{}, fields: map[string]lww{}}
}

func (d *Doc) VV() VV { return d.vv.Copy() }

func (d *Doc) next() Dot {
	d.vv[d.Device]++
	return Dot{d.Device, d.vv[d.Device]}
}

func (d *Doc) local(op Op) Op {
	op.Dot = d.next()
	d.ops[d.Device] = append(d.ops[d.Device], op)
	d.apply(op)
	return op
}

// AddStock records items received (+n) or handed out (-n).
func (d *Doc) AddStock(item string, n int64) Op { return d.local(Op{Kind: Stock, Key: item, Delta: n}) }

// OpenNeed records a request ("insulin for 3 people at camp 4").
func (d *Doc) OpenNeed(need string) Op { return d.local(Op{Kind: NeedAdd, Key: need}) }

// CloseNeed marks a need met. It closes only the requests this device has
// seen; a request someone else made offline stays open.
func (d *Doc) CloseNeed(need string) Op {
	var seen []Dot
	for dot := range d.adds[need] {
		seen = append(seen, dot)
	}
	sortDots(seen)
	return d.local(Op{Kind: NeedDel, Key: need, Seen: seen})
}

func (d *Doc) SetField(key, value string) Op {
	return d.local(Op{Kind: Set, Key: key, Value: value, HLC: d.clock.Now()})
}

func (d *Doc) apply(op Op) {
	switch op.Kind {
	case Stock:
		d.stock[op.Key] += op.Delta
	case NeedAdd:
		if d.closed[op.Dot] {
			return // a "done" that saw this add arrived first
		}
		if d.adds[op.Key] == nil {
			d.adds[op.Key] = map[Dot]bool{}
		}
		d.adds[op.Key][op.Dot] = true
	case NeedDel:
		for _, s := range op.Seen {
			d.closed[s] = true
			delete(d.adds[op.Key], s)
		}
		if len(d.adds[op.Key]) == 0 {
			delete(d.adds, op.Key)
		}
	case Set:
		cur, ok := d.fields[op.Key]
		nw := lww{op.Value, op.HLC, op.Dot.Device}
		if !ok || cur.less(nw) {
			d.fields[op.Key] = nw
		}
		d.clock.Observe(op.HLC)
	}
}

// Missing returns, in seq order per device, every op this replica has that
// a peer with version vector peer does not.
func (d *Doc) Missing(peer VV) []Op {
	var out []Op
	devs := make([]string, 0, len(d.ops))
	for dev := range d.ops {
		devs = append(devs, dev)
	}
	sort.Strings(devs)
	for _, dev := range devs {
		have := peer[dev]
		ops := d.ops[dev]
		if have < uint64(len(ops)) {
			out = append(out, ops[have:]...)
		}
	}
	return out
}

// Receive applies ops from a peer. Ops already held are ignored, so
// receiving the same batch twice is harmless. Ops must arrive in seq order
// per device with no gaps (Missing guarantees this); a gap is an error and
// nothing past it is applied.
func (d *Doc) Receive(ops []Op) (applied int, err error) {
	for _, op := range ops {
		have := d.vv[op.Dot.Device]
		switch {
		case op.Dot.Seq <= have:
			continue // duplicate
		case op.Dot.Seq != have+1:
			return applied, fmt.Errorf("gap: have %s:%d, got %s", op.Dot.Device, have, op.Dot)
		}
		d.vv[op.Dot.Device] = op.Dot.Seq
		d.ops[op.Dot.Device] = append(d.ops[op.Dot.Device], op)
		d.apply(op)
		applied++
	}
	return applied, nil
}

// Sync exchanges ops both ways between two replicas in memory and returns
// how many ops moved. Over a network this is two requests carrying a
// version vector and a batch of ops.
func Sync(a, b *Doc) (int, error) {
	ab := a.Missing(b.VV())
	ba := b.Missing(a.VV())
	n1, err := b.Receive(ab)
	if err != nil {
		return 0, err
	}
	n2, err := a.Receive(ba)
	return n1 + n2, err
}

// State is the visible document.
type State struct {
	Stock  map[string]int64  `json:"stock"`
	Needs  []string          `json:"open_needs"`
	Fields map[string]string `json:"fields"`
}

func (d *Doc) State() State {
	s := State{Stock: map[string]int64{}, Fields: map[string]string{}}
	for k, v := range d.stock {
		s.Stock[k] = v
	}
	for k := range d.adds {
		s.Needs = append(s.Needs, k)
	}
	sort.Strings(s.Needs)
	for k, v := range d.fields {
		s.Fields[k] = v.Value
	}
	return s
}

// Hash is a fingerprint of the visible state, for convergence checks.
func (d *Doc) Hash() string {
	s := d.State()
	h := sha256.New()
	keys := func(m map[string]int64) []string {
		out := make([]string, 0, len(m))
		for k := range m {
			out = append(out, k)
		}
		sort.Strings(out)
		return out
	}
	for _, k := range keys(s.Stock) {
		fmt.Fprintf(h, "s|%s|%d\n", k, s.Stock[k])
	}
	fmt.Fprintf(h, "n|%s\n", strings.Join(s.Needs, "\x00"))
	fk := make([]string, 0, len(s.Fields))
	for k := range s.Fields {
		fk = append(fk, k)
	}
	sort.Strings(fk)
	for _, k := range fk {
		fmt.Fprintf(h, "f|%s|%s\n", k, s.Fields[k])
	}
	return hex.EncodeToString(h.Sum(nil))[:16]
}

// Conflicts lists what a human must look at after merging: stock that went
// negative because several devices handed out the same supplies offline.
// A CRDT cannot prevent this without coordination; it can make sure nobody
// misses it.
func (d *Doc) Conflicts() []string {
	var out []string
	for item, n := range d.stock {
		if n < 0 {
			out = append(out, fmt.Sprintf("%s oversubscribed by %d", item, -n))
		}
	}
	sort.Strings(out)
	return out
}

// OpCount is the number of ops held.
func (d *Doc) OpCount() int {
	n := 0
	for _, o := range d.ops {
		n += len(o)
	}
	return n
}

// AllOps returns every op held, for snapshots.
func (d *Doc) AllOps() []Op { return d.Missing(VV{}) }

func sortDots(ds []Dot) {
	sort.Slice(ds, func(i, j int) bool {
		if ds[i].Device != ds[j].Device {
			return ds[i].Device < ds[j].Device
		}
		return ds[i].Seq < ds[j].Seq
	})
}

// EncodedSize estimates wire bytes for a batch (used for sync metrics).
func EncodedSize(ops []Op) int {
	n := 0
	var buf [binary.MaxVarintLen64]byte
	for _, o := range ops {
		n += len(o.Dot.Device) + binary.PutUvarint(buf[:], o.Dot.Seq) + len(o.Kind) + len(o.Key) + len(o.Value) + 8
		if o.Kind == Set {
			n += 12
		}
		for _, s := range o.Seen {
			n += len(s.Device) + binary.PutUvarint(buf[:], s.Seq)
		}
	}
	return n
}
