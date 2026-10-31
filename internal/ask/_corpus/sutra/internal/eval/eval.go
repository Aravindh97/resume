// Package eval measures entity resolution against labelled data with
// pairwise precision, recall and F1: a pair of records is a true match when
// both come from the same real company.
package eval

import (
	"fmt"
	"io"
	"sort"
	"strings"

	"github.com/Aravindh97/sutra/internal/normalize"
	"github.com/Aravindh97/sutra/internal/ontology"
	"github.com/Aravindh97/sutra/internal/resolve"
	"github.com/Aravindh97/sutra/internal/synth"
)

type Metrics struct {
	Name                  string
	Precision, Recall, F1 float64
	TP, FP, FN            int
	Entities, Review      int
}

func pairs(clusters [][]string) map[[2]string]bool {
	out := map[[2]string]bool{}
	for _, c := range clusters {
		s := append([]string(nil), c...)
		sort.Strings(s)
		for i := range s {
			for j := i + 1; j < len(s); j++ {
				out[[2]string{s[i], s[j]}] = true
			}
		}
	}
	return out
}

func score(name string, pred, truth [][]string) Metrics {
	p, t := pairs(pred), pairs(truth)
	m := Metrics{Name: name, Entities: len(pred)}
	for k := range p {
		if t[k] {
			m.TP++
		} else {
			m.FP++
		}
	}
	for k := range t {
		if !p[k] {
			m.FN++
		}
	}
	if m.TP+m.FP > 0 {
		m.Precision = float64(m.TP) / float64(m.TP+m.FP)
	}
	if m.TP+m.FN > 0 {
		m.Recall = float64(m.TP) / float64(m.TP+m.FN)
	}
	if m.Precision+m.Recall > 0 {
		m.F1 = 2 * m.Precision * m.Recall / (m.Precision + m.Recall)
	}
	return m
}

// Records maps a synthetic dataset through the ontology, returning records
// and the truth label per record key.
func Records(o *ontology.Ontology, d synth.Dataset) ([]ontology.Record, map[string]string, error) {
	var recs []ontology.Record
	truth := map[string]string{}
	add := func(src string, rows []synth.Row) error {
		s := o.Sources[src]
		for _, row := range rows {
			r, err := s.MapRow(row.Fields)
			if err != nil {
				return err
			}
			recs = append(recs, r)
			truth[r.Key()] = row.TruthID
		}
		return nil
	}
	if err := add("erp_vendors", d.ERP); err != nil {
		return nil, nil, err
	}
	return recs, truth, add("crm_accounts", d.CRM)
}

func group(recs []ontology.Record, key func(ontology.Record) string) [][]string {
	m := map[string][]string{}
	var singles [][]string
	for _, r := range recs {
		k := key(r)
		if k == "" {
			singles = append(singles, []string{r.Key()})
			continue
		}
		m[k] = append(m[k], r.Key())
	}
	for _, v := range m {
		singles = append(singles, v)
	}
	return singles
}

type Report struct {
	Records, Companies int
	Rows               []Metrics
	Sweep              []Metrics
	Errors             []string // sample false merges and misses
}

func Run(o *ontology.Ontology, d synth.Dataset, cfg resolve.Config) (*Report, error) {
	recs, truthOf, err := Records(o, d)
	if err != nil {
		return nil, err
	}
	truth := group(recs, func(r ontology.Record) string { return truthOf[r.Key()] })
	prio := map[string]int{}
	for _, s := range o.Sources {
		prio[s.Name] = s.Priority
	}
	rep := &Report{Records: len(recs), Companies: len(truth)}

	rep.Rows = append(rep.Rows, score("exact name", group(recs, func(r ontology.Record) string {
		return strings.ToLower(strings.TrimSpace(r.Attrs["name"]))
	}), truth))
	rep.Rows = append(rep.Rows, score("GSTIN only", group(recs, func(r ontology.Record) string {
		g, ok := normalize.GSTIN(r.Attrs["gstin"])
		if !ok {
			return ""
		}
		return g
	}), truth))

	run := func(name string, c resolve.Config) Metrics {
		res := resolve.Resolve(recs, prio, c)
		var cl [][]string
		for _, e := range res.Entities {
			cl = append(cl, e.Members)
		}
		m := score(name, cl, truth)
		m.Review = len(res.Review)
		return m
	}
	rep.Rows = append(rep.Rows, run("sutra", cfg))
	for _, th := range []float64{0.70, 0.75, 0.80, 0.85, 0.90, 0.95} {
		c := cfg
		c.Match = th
		rep.Sweep = append(rep.Sweep, run(fmt.Sprintf("match>=%.2f", th), c))
	}

	// Explain errors at the configured threshold.
	res := resolve.Resolve(recs, prio, cfg)
	byKey := map[string]ontology.Record{}
	for _, r := range recs {
		byKey[r.Key()] = r
	}
	clusterOf := map[string]string{}
	for _, e := range res.Entities {
		for _, m := range e.Members {
			clusterOf[m] = e.ID
		}
	}
	for _, e := range res.Entities {
		for i := range e.Members {
			for j := i + 1; j < len(e.Members); j++ {
				a, b := e.Members[i], e.Members[j]
				if truthOf[a] != truthOf[b] && len(rep.Errors) < 8 {
					rep.Errors = append(rep.Errors, fmt.Sprintf("FALSE MERGE %q (%s) + %q (%s): %s", byKey[a].Attrs["name"],
						byKey[a].Attrs["city"], byKey[b].Attrs["name"], byKey[b].Attrs["city"], resolve.Score(byKey[a], byKey[b]).Reason))
				}
			}
		}
	}
	for _, t := range truth {
		for i := range t {
			for j := i + 1; j < len(t); j++ {
				a, b := t[i], t[j]
				if clusterOf[a] != clusterOf[b] && len(rep.Errors) < 16 {
					p := resolve.Score(byKey[a], byKey[b])
					rep.Errors = append(rep.Errors, fmt.Sprintf("MISSED %q vs %q: score %.2f (%s)", byKey[a].Attrs["name"],
						byKey[b].Attrs["name"], p.Score, p.Reason))
				}
			}
		}
	}
	return rep, nil
}

func Print(w io.Writer, r *Report, verbose bool) {
	fmt.Fprintf(w, "records=%d companies=%d\n\n", r.Records, r.Companies)
	line := func(m Metrics) {
		fmt.Fprintf(w, "%-14s %9.3f %7.3f %6.3f %5d %5d %5d %9d %7d\n", m.Name, m.Precision, m.Recall, m.F1, m.TP, m.FP, m.FN, m.Entities, m.Review)
	}
	hdr := fmt.Sprintf("%-14s %9s %7s %6s %5s %5s %5s %9s %7s\n", "method", "precision", "recall", "f1", "tp", "fp", "fn", "entities", "review")
	fmt.Fprint(w, hdr)
	for _, m := range r.Rows {
		line(m)
	}
	fmt.Fprint(w, "\nthreshold sweep\n"+hdr)
	for _, m := range r.Sweep {
		line(m)
	}
	if verbose {
		fmt.Fprintln(w)
		for _, e := range r.Errors {
			fmt.Fprintln(w, e)
		}
	}
}
