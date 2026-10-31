// Package eval measures retrieval quality on a labelled question set.
//
// A question is answered correctly at rank r if the chunk at r comes from the
// expected document and contains the evidence string. Labels are tied to
// evidence text, not chunk ids, so they stay valid when chunking changes.
package eval

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/Aravindh97/granth/internal/search"
)

type Question struct {
	ID       string `json:"id"`
	Query    string `json:"query"`
	Doc      string `json:"doc"`      // file name of the expected source
	Evidence string `json:"evidence"` // text the right chunk must contain
}

func Load(path string) ([]Question, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var qs []Question
	sc := bufio.NewScanner(f)
	for n := 1; sc.Scan(); n++ {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		var q Question
		if err := json.Unmarshal([]byte(line), &q); err != nil {
			return nil, fmt.Errorf("%s:%d: %w", path, n, err)
		}
		qs = append(qs, q)
	}
	return qs, sc.Err()
}

type Miss struct {
	Question Question
	Top      []search.Result
}

type Report struct {
	Mode           search.Mode
	N              int
	HitAt1, HitAt5 float64
	MRR            float64 // MRR@10
	CitationAt1    float64 // top result cites the right document
	P50, P95       time.Duration
	Misses         []Miss // questions with no correct chunk in the top 5
}

func Run(ctx context.Context, s *search.Searcher, qs []Question, mode search.Mode) (*Report, error) {
	r := &Report{Mode: mode, N: len(qs)}
	var lat []time.Duration
	for _, q := range qs {
		t := time.Now()
		resp, err := s.Search(ctx, search.Request{Query: q.Query, K: 10, Mode: mode})
		if err != nil {
			return nil, fmt.Errorf("%s: %w", q.ID, err)
		}
		lat = append(lat, time.Since(t))
		rank := 0
		for i, res := range resp.Results {
			if correct(q, res) {
				rank = i + 1
				break
			}
		}
		if rank == 1 {
			r.HitAt1++
		}
		if rank >= 1 && rank <= 5 {
			r.HitAt5++
		} else {
			top := resp.Results
			if len(top) > 3 {
				top = top[:3]
			}
			r.Misses = append(r.Misses, Miss{q, top})
		}
		if rank > 0 {
			r.MRR += 1 / float64(rank)
		}
		if len(resp.Results) > 0 && filepath.Base(resp.Results[0].Citation.SourceURI) == q.Doc {
			r.CitationAt1++
		}
	}
	n := float64(max(1, len(qs)))
	r.HitAt1, r.HitAt5, r.MRR, r.CitationAt1 = r.HitAt1/n, r.HitAt5/n, r.MRR/n, r.CitationAt1/n
	sort.Slice(lat, func(i, j int) bool { return lat[i] < lat[j] })
	if len(lat) > 0 {
		r.P50, r.P95 = lat[len(lat)/2], lat[min(len(lat)-1, len(lat)*95/100)]
	}
	return r, nil
}

func correct(q Question, r search.Result) bool {
	return filepath.Base(r.Citation.SourceURI) == q.Doc && strings.Contains(r.Snippet, q.Evidence)
}

// Print writes a summary table and, if verbose, a trace for every miss.
func Print(w io.Writer, reports []*Report, verbose bool) {
	fmt.Fprintf(w, "%-8s %4s %7s %7s %7s %8s %9s %9s\n", "mode", "n", "hit@1", "hit@5", "mrr@10", "cite@1", "p50", "p95")
	for _, r := range reports {
		fmt.Fprintf(w, "%-8s %4d %7.3f %7.3f %7.3f %8.3f %9s %9s\n", r.Mode, r.N, r.HitAt1, r.HitAt5, r.MRR,
			r.CitationAt1, r.P50.Round(time.Microsecond), r.P95.Round(time.Microsecond))
	}
	if !verbose {
		return
	}
	for _, r := range reports {
		for _, m := range r.Misses {
			fmt.Fprintf(w, "\n[%s] MISS %s %q (want %s: %q)\n", r.Mode, m.Question.ID, m.Question.Query, m.Question.Doc, m.Question.Evidence)
			for i, t := range m.Top {
				fmt.Fprintf(w, "   #%d %s lex=%d vec=%d fused=%.4f  %.70q\n", i+1, filepath.Base(t.Citation.SourceURI),
					t.Trace.LexicalRank, t.Trace.VectorRank, t.Trace.Fused, strings.ReplaceAll(t.Snippet, "\n", " "))
			}
		}
	}
}
