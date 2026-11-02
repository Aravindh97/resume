//go:build js && wasm

// Command web runs Sutra's entity resolution in the browser on labelled
// synthetic ERP and CRM data.
package main

import (
	_ "embed"
	"encoding/json"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Aravindh97/sutra/internal/eval"
	"github.com/Aravindh97/sutra/internal/normalize"
	"github.com/Aravindh97/sutra/internal/ontology"
	"github.com/Aravindh97/sutra/internal/resolve"
	"github.com/Aravindh97/sutra/internal/synth"
)

//go:embed embed/ontology.yaml
var ontologyYAML []byte

type session struct {
	recs  []ontology.Record
	truth map[string]string
	byKey map[string]ontology.Record
	res   resolve.Result
	prio  map[string]int
	seed  uint64
	n     int
	took  time.Duration
}

var (
	mu  sync.Mutex
	cur *session
	ont *ontology.Ontology
)

func build(seed uint64, n int) (*session, error) {
	d := synth.Generate(seed, n)
	recs, truth, err := eval.Records(ont, d)
	if err != nil {
		return nil, err
	}
	s := &session{recs: recs, truth: truth, byKey: map[string]ontology.Record{}, prio: map[string]int{}, seed: seed, n: n}
	for _, r := range recs {
		s.byKey[r.Key()] = r
	}
	for _, src := range ont.Sources {
		s.prio[src.Name] = src.Priority
	}
	t := time.Now()
	s.res = resolve.Resolve(recs, s.prio, resolve.DefaultConfig())
	s.took = time.Since(t)
	return s, nil
}

func entityRow(e resolve.Entity, s *session) map[string]any {
	srcs := map[string]int{}
	truths := map[string]bool{}
	for _, m := range e.Members {
		srcs[strings.SplitN(m, ":", 2)[0]]++
		truths[s.truth[m]] = true
	}
	return map[string]any{"id": e.ID, "name": e.Golden["name"], "gstin": e.Golden["gstin"], "city": e.Golden["city"],
		"members": len(e.Members), "sources": srcs, "correct": len(truths) == 1}
}

func main() {
	var err error
	ont, err = ontology.Parse(ontologyYAML)
	serve(map[string]handler{
		"POST generate": func(q url.Values, _ []byte) resp {
			if err != nil {
				return fail(500, err.Error())
			}
			seed, _ := strconv.ParseUint(q.Get("seed"), 10, 64)
			n, _ := strconv.Atoi(q.Get("n"))
			n = min(max(n, 20), 2000)
			s, e := build(seed, n)
			if e != nil {
				return fail(500, e.Error())
			}
			mu.Lock()
			cur = s
			mu.Unlock()
			multi := 0
			for _, en := range s.res.Entities {
				if len(en.Members) > 1 {
					multi++
				}
			}
			return ok(map[string]any{"records": len(s.recs), "entities": len(s.res.Entities), "merged": multi, "review": len(s.res.Review),
				"pairs_scored": s.res.Compared, "all_pairs": len(s.recs) * (len(s.recs) - 1) / 2, "took_ms": s.took.Milliseconds()})
		},
		"GET entities": func(q url.Values, _ []byte) resp {
			mu.Lock()
			s := cur
			mu.Unlock()
			if s == nil {
				return fail(400, "generate data first")
			}
			term := strings.ToLower(strings.TrimSpace(q.Get("q")))
			var rows []map[string]any
			for _, e := range s.res.Entities {
				if len(e.Members) < 2 && term == "" {
					continue // the list shows merged entities unless searching
				}
				hay := strings.ToLower(e.Golden["name"] + " " + e.Golden["gstin"] + " " + e.Golden["city"])
				for _, m := range e.Members {
					hay += " " + strings.ToLower(s.byKey[m].Attrs["name"])
				}
				if term != "" && !strings.Contains(hay, term) {
					continue
				}
				rows = append(rows, entityRow(e, s))
			}
			sort.Slice(rows, func(i, j int) bool { return rows[i]["members"].(int) > rows[j]["members"].(int) })
			if len(rows) > 15 {
				rows = rows[:15]
			}
			return ok(rows)
		},
		"GET entity": func(q url.Values, _ []byte) resp {
			mu.Lock()
			s := cur
			mu.Unlock()
			if s == nil {
				return fail(400, "generate data first")
			}
			for _, e := range s.res.Entities {
				if e.ID != q.Get("id") {
					continue
				}
				var members []map[string]any
				for _, m := range e.Members {
					r := s.byKey[m]
					members = append(members, map[string]any{"key": m, "source": r.Source, "id": r.ID, "attrs": r.Attrs, "true_company": s.truth[m]})
				}
				return ok(map[string]any{"entity": entityRow(e, s), "golden": e.Golden, "lineage": e.Lineage, "evidence": e.Reasons, "members": members})
			}
			return fail(404, "no such entity")
		},
		"GET review": func(url.Values, []byte) resp {
			mu.Lock()
			s := cur
			mu.Unlock()
			if s == nil {
				return fail(400, "generate data first")
			}
			var rows []map[string]any
			for i, p := range s.res.Review {
				if i == 40 {
					break
				}
				a, b := s.byKey[p.A], s.byKey[p.B]
				rows = append(rows, map[string]any{"a": a.Attrs["name"] + " · " + a.Attrs["city"], "b": b.Attrs["name"] + " · " + b.Attrs["city"],
					"score": p.Score, "reason": p.Reason, "truly_same": s.truth[p.A] == s.truth[p.B]})
			}
			return ok(map[string]any{"total": len(s.res.Review), "rows": rows})
		},
		"POST eval": func(q url.Values, _ []byte) resp {
			seed, _ := strconv.ParseUint(q.Get("seed"), 10, 64)
			n, _ := strconv.Atoi(q.Get("n"))
			n = min(max(n, 20), 2000)
			r, e := eval.Run(ont, synth.Generate(seed, n), resolve.DefaultConfig())
			if e != nil {
				return fail(500, e.Error())
			}
			return ok(map[string]any{"records": r.Records, "companies": r.Companies, "rows": r.Rows, "sweep": r.Sweep})
		},
		"POST gstin": func(_ url.Values, b []byte) resp {
			var in struct{ Value string }
			json.Unmarshal(b, &in)
			g, valid := normalize.GSTIN(in.Value)
			out := map[string]any{"normalised": g, "valid": valid}
			if len(g) >= 14 {
				out["expected_check_char"] = string(normalize.GSTINChecksum(g[:14]))
				out["pan"] = normalize.PANFromGSTIN(g)
				out["state_code"] = g[:2]
			}
			return ok(out)
		},
	})
}
