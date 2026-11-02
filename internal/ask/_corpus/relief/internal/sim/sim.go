// Package sim runs randomised relief operations: devices make changes while
// the network splits into random groups, groups sync internally, devices
// relay through each other, messages are duplicated, clocks are skewed, and
// finally the network heals. It then checks the merged result against an
// oracle computed from every change ever made.
package sim

import (
	"fmt"
	"math/rand/v2"
	"sort"

	"github.com/Aravindh97/relief/internal/crdt"
)

type Config struct {
	Devices int
	Steps   int
	Seed    uint64
}

type Result struct {
	Converged       bool
	OpsTotal        int
	OpsSent         int // ops moved by delta sync
	FullStateSent   int // ops a naive "send everything" sync would have moved
	DeltaBytes      int
	FullBytes       int
	Syncs           int
	Violations      []string
	BaselineLost    int64 // stock units lost by last-writer-wins whole-document sync
	BaselineLostOps int
}

var items = []string{"rice_kg", "water_l", "tarpaulin", "ors_packets", "blankets"}
var needs = []string{"insulin camp-2", "boat at ward 7", "baby formula", "doctor camp-4", "diesel generator"}
var fields = []string{"camp-2.status", "camp-4.status", "ward-7.contact"}

type truthAdd struct {
	need string
	dot  crdt.Dot
}

// baseline is how most offline apps sync a shared sheet: the whole document
// with the newest timestamp replaces the other.
type baseline struct {
	stock map[string]int64
	at    int64
}

func Run(c Config) Result {
	rng := rand.New(rand.NewPCG(c.Seed, 0x5eed))
	skew := make([]int64, c.Devices)
	docs := make([]*crdt.Doc, c.Devices)
	base := make([]baseline, c.Devices)
	var now int64 = 1_700_000_000_000
	for i := range docs {
		skew[i] = int64(rng.IntN(600_000)) - 300_000 // phone clocks off by up to ±5 minutes
		i := i
		docs[i] = crdt.New(fmt.Sprintf("dev%02d", i), crdt.NewClock(func() int64 { return now + skew[i] }))
		base[i] = baseline{stock: map[string]int64{}}
	}
	// Oracles, computed from every op ever made.
	trueStock := map[string]int64{}
	var allAdds []truthAdd
	closed := map[crdt.Dot]bool{}
	type setOp struct {
		hlc crdt.HLC
		dev string
		val string
	}
	lastSet := map[string]setOp{}
	var baseTrue int64

	res := Result{}
	groups := func() [][]int {
		perm := rng.Perm(c.Devices)
		k := 1 + rng.IntN(min(4, c.Devices))
		gs := make([][]int, k)
		for i, d := range perm {
			gs[i%k] = append(gs[i%k], d)
		}
		return gs
	}
	part := groups()
	sync := func(a, b int, dup bool) {
		ab := docs[a].Missing(docs[b].VV())
		ba := docs[b].Missing(docs[a].VV())
		res.OpsSent += len(ab) + len(ba)
		res.DeltaBytes += crdt.EncodedSize(ab) + crdt.EncodedSize(ba)
		res.FullStateSent += docs[a].OpCount() + docs[b].OpCount()
		res.FullBytes += crdt.EncodedSize(docs[a].AllOps()) + crdt.EncodedSize(docs[b].AllOps())
		res.Syncs++
		if _, err := docs[b].Receive(ab); err != nil {
			res.Violations = append(res.Violations, err.Error())
		}
		if _, err := docs[a].Receive(ba); err != nil {
			res.Violations = append(res.Violations, err.Error())
		}
		if dup { // the same batch delivered again (radio retry)
			docs[b].Receive(ab)
			docs[a].Receive(ba)
		}
		// baseline: newest whole document wins on both sides
		if base[a].at >= base[b].at {
			base[b] = baseline{copyMap(base[a].stock), base[a].at}
		} else {
			base[a] = baseline{copyMap(base[b].stock), base[b].at}
		}
	}

	for step := 0; step < c.Steps; step++ {
		now += int64(1 + rng.IntN(5000))
		switch r := rng.Float64(); {
		case r < 0.08: // network changes shape
			part = groups()
		case r < 0.30: // two devices in the same group sync
			g := part[rng.IntN(len(part))]
			if len(g) >= 2 {
				i, j := rng.IntN(len(g)), rng.IntN(len(g))
				if i != j {
					sync(g[i], g[j], rng.IntN(10) == 0)
				}
			}
		default: // someone makes a change
			d := rng.IntN(c.Devices)
			doc := docs[d]
			switch k := rng.Float64(); {
			case k < 0.55:
				item := items[rng.IntN(len(items))]
				n := int64(rng.IntN(40) + 1)
				if rng.IntN(2) == 0 {
					n = -n
				}
				doc.AddStock(item, n)
				trueStock[item] += n
				base[d].stock[item] += n
				base[d].at = now + skew[d]
				baseTrue += n
			case k < 0.75:
				nd := needs[rng.IntN(len(needs))]
				op := doc.OpenNeed(nd)
				allAdds = append(allAdds, truthAdd{nd, op.Dot})
			case k < 0.87:
				op := doc.CloseNeed(needs[rng.IntN(len(needs))])
				for _, s := range op.Seen {
					closed[s] = true
				}
			default:
				f := fields[rng.IntN(len(fields))]
				v := fmt.Sprintf("v%d", rng.IntN(1000))
				op := doc.SetField(f, v)
				cur, ok := lastSet[f]
				if !ok || cur.hlc.Less(op.HLC) || cur.hlc == op.HLC && cur.dev < op.Dot.Device {
					lastSet[f] = setOp{op.HLC, op.Dot.Device, v}
				}
			}
		}
	}
	// Heal: rounds over every pair, in random order, until a whole round
	// moves nothing. (A random pair moving nothing proves nothing.)
	for round := 0; round < 100; round++ {
		moved := 0
		for _, a := range rng.Perm(c.Devices) {
			for _, b := range rng.Perm(c.Devices) {
				if a == b {
					continue
				}
				before := res.OpsSent
				sync(a, b, false)
				moved += res.OpsSent - before
			}
		}
		if moved == 0 {
			break
		}
	}

	h0 := docs[0].Hash()
	res.Converged = true
	for _, d := range docs[1:] {
		if d.Hash() != h0 {
			res.Converged = false
			res.Violations = append(res.Violations, "replicas differ after heal")
			break
		}
	}
	res.OpsTotal = docs[0].OpCount()
	st := docs[0].State()
	// Invariant 1: no lost update. Every +/- change counts exactly once.
	for _, it := range items {
		if st.Stock[it] != trueStock[it] {
			res.Violations = append(res.Violations, fmt.Sprintf("stock %s = %d, want %d", it, st.Stock[it], trueStock[it]))
		}
	}
	// Invariant 2: a need is open iff some add was never closed.
	wantOpen := map[string]bool{}
	for _, a := range allAdds {
		if !closed[a.dot] {
			wantOpen[a.need] = true
		}
	}
	var want []string
	for n := range wantOpen {
		want = append(want, n)
	}
	sort.Strings(want)
	if fmt.Sprint(want) != fmt.Sprint(st.Needs) {
		res.Violations = append(res.Violations, fmt.Sprintf("open needs %v, want %v", st.Needs, want))
	}
	// Invariant 3: each field holds the write with the greatest HLC.
	for f, w := range lastSet {
		if st.Fields[f] != w.val {
			res.Violations = append(res.Violations, fmt.Sprintf("field %s = %q, want %q", f, st.Fields[f], w.val))
		}
	}
	// Baseline after the same heal: compare its totals with the truth.
	for _, it := range items {
		diff := base[0].stock[it] - trueStock[it]
		if diff < 0 {
			diff = -diff
		}
		res.BaselineLost += diff
	}
	return res
}

func copyMap(m map[string]int64) map[string]int64 {
	c := make(map[string]int64, len(m))
	for k, v := range m {
		c[k] = v
	}
	return c
}
