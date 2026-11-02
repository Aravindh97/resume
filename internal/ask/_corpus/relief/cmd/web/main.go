//go:build js && wasm

// Command web runs relief in the browser: three devices with their own
// replicas, links you can cut, and the randomised partition test.
package main

import (
	"encoding/json"
	"fmt"
	"net/url"
	"strconv"
	"sync"
	"time"

	"github.com/Aravindh97/relief/internal/crdt"
	"github.com/Aravindh97/relief/internal/sim"
)

var (
	mu    sync.Mutex
	names = []string{"camp2-phone", "camp4-tablet", "truck-laptop"}
	docs  map[string]*crdt.Doc
	links map[string]bool // "a|b" -> connected
	log   []string
)

func key(a, b string) string {
	if a > b {
		a, b = b, a
	}
	return a + "|" + b
}

func reset() {
	docs, links, log = map[string]*crdt.Doc{}, map[string]bool{}, nil
	for _, n := range names {
		docs[n] = crdt.New(n, nil)
	}
	for i := range names {
		for j := i + 1; j < len(names); j++ {
			links[key(names[i], names[j])] = true
		}
	}
	d := docs["camp2-phone"]
	d.AddStock("rice_kg", 200)
	d.AddStock("tarpaulin", 40)
	d.OpenNeed("insulin camp-2")
	d.SetField("camp-2.status", "flooded")
	gossip()
	log = append(log, "start: camp 2 recorded 200 kg rice, 40 tarpaulins, an insulin request; everyone synced")
}

func gossip() int {
	moved := 0
	for round := 0; round < 10; round++ {
		n := 0
		for i := range names {
			for j := i + 1; j < len(names); j++ {
				if links[key(names[i], names[j])] {
					m, _ := crdt.Sync(docs[names[i]], docs[names[j]])
					n += m
				}
			}
		}
		moved += n
		if n == 0 {
			break
		}
	}
	return moved
}

type deviceView struct {
	Name      string     `json:"name"`
	State     crdt.State `json:"state"`
	Hash      string     `json:"hash"`
	Ops       int        `json:"ops"`
	VV        crdt.VV    `json:"vv"`
	Conflicts []string   `json:"conflicts"`
}

func view() map[string]any {
	var ds []deviceView
	for _, n := range names {
		d := docs[n]
		ds = append(ds, deviceView{n, d.State(), d.Hash(), d.OpCount(), d.VV(), d.Conflicts()})
	}
	ls := map[string]bool{}
	for k, v := range links {
		ls[k] = v
	}
	same := true
	for _, d := range ds[1:] {
		if d.Hash != ds[0].Hash {
			same = false
		}
	}
	tail := log
	if len(tail) > 12 {
		tail = tail[len(tail)-12:]
	}
	return map[string]any{"devices": ds, "links": ls, "converged": same, "log": tail}
}

func main() {
	reset()
	serve(map[string]handler{
		"GET state": func(url.Values, []byte) resp {
			mu.Lock()
			defer mu.Unlock()
			return ok(view())
		},
		"POST reset": func(url.Values, []byte) resp {
			mu.Lock()
			defer mu.Unlock()
			reset()
			return ok(view())
		},
		"POST op": func(_ url.Values, b []byte) resp {
			var in struct{ Device, Kind, Key, Value string }
			if json.Unmarshal(b, &in) != nil || in.Key == "" {
				return fail(400, "device, kind and key are required")
			}
			mu.Lock()
			defer mu.Unlock()
			d, found := docs[in.Device]
			if !found {
				return fail(404, "no such device")
			}
			switch in.Kind {
			case "stock":
				n, err := strconv.ParseInt(in.Value, 10, 64)
				if err != nil || n == 0 {
					return fail(400, "quantity must be a non-zero whole number, like 50 or -20")
				}
				d.AddStock(in.Key, n)
				log = append(log, fmt.Sprintf("%s: %s %+d", in.Device, in.Key, n))
			case "open":
				d.OpenNeed(in.Key)
				log = append(log, fmt.Sprintf("%s: opened need %q", in.Device, in.Key))
			case "done":
				d.CloseNeed(in.Key)
				log = append(log, fmt.Sprintf("%s: closed need %q (the requests it has seen)", in.Device, in.Key))
			case "set":
				d.SetField(in.Key, in.Value)
				log = append(log, fmt.Sprintf("%s: set %s = %s", in.Device, in.Key, in.Value))
			default:
				return fail(400, "unknown kind")
			}
			// connected devices sync straight away, like phones on one hotspot
			if n := gossip(); n > 0 {
				log = append(log, fmt.Sprintf("  synced %d ops to connected devices", n))
			}
			return ok(view())
		},
		"POST link": func(_ url.Values, b []byte) resp {
			var in struct {
				A, B string
				Up   bool
			}
			if json.Unmarshal(b, &in) != nil {
				return fail(400, "bad request")
			}
			mu.Lock()
			defer mu.Unlock()
			k := key(in.A, in.B)
			if _, found := links[k]; !found {
				return fail(404, "no such link")
			}
			links[k] = in.Up
			if in.Up {
				n := gossip()
				log = append(log, fmt.Sprintf("link %s ↔ %s restored: %d ops synced", in.A, in.B, n))
			} else {
				log = append(log, fmt.Sprintf("link %s ↔ %s cut", in.A, in.B))
			}
			return ok(view())
		},
		"POST sim": func(q url.Values, _ []byte) resp {
			runs, _ := strconv.Atoi(q.Get("runs"))
			runs = min(max(runs, 1), 20000)
			t := time.Now()
			var conv, viol, baseLost int
			var sent, full int
			var first string
			for i := 0; i < runs; i++ {
				r := sim.Run(sim.Config{Devices: 3 + i%6, Steps: 100 + i%400, Seed: uint64(i)})
				if r.Converged {
					conv++
				}
				if len(r.Violations) > 0 {
					viol++
					if first == "" {
						first = fmt.Sprintf("seed %d: %v", i, r.Violations)
					}
				}
				if r.BaselineLost > 0 {
					baseLost++
				}
				sent += r.DeltaBytes
				full += r.FullBytes
			}
			return ok(map[string]any{"runs": runs, "converged": conv, "violations": viol, "first_violation": first,
				"baseline_lost": baseLost, "bytes_pct": 100 * float64(sent) / float64(max(full, 1)), "took_ms": time.Since(t).Milliseconds()})
		},
	})
}
