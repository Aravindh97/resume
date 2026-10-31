// Package eval replays a multi-session scenario into Smriti and asks
// questions whose answers changed over time.
//
// For each question it reports whether the top memory has the current value
// (correct), the outdated value (stale), or content the user must not see
// (leak). The baseline retrieves the single most similar raw message from the
// user's own chat history with the same embedder, which is what an agent
// without a memory layer would do.
package eval

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/Aravindh97/smriti/internal/embed"
	"github.com/Aravindh97/smriti/internal/extract"
	"github.com/Aravindh97/smriti/internal/memory"
	"github.com/Aravindh97/smriti/internal/store"
)

type Scenario struct {
	Org        string    `json:"org"`
	Now        time.Time `json:"now"`
	OrgWriters []string  `json:"org_writers"`
	Users      []struct {
		ID    string   `json:"id"`
		Teams []string `json:"teams"`
	} `json:"users"`
	Sessions []struct {
		ID       string    `json:"id"`
		User     string    `json:"user"`
		Team     string    `json:"team"`
		At       time.Time `json:"at"`
		Messages []string  `json:"messages"`
	} `json:"sessions"`
	Questions []struct {
		ID     string     `json:"id"`
		User   string     `json:"user"`
		Query  string     `json:"query"`
		AsOf   *time.Time `json:"as_of"`
		Expect string     `json:"expect"`
		Stale  string     `json:"stale"`
		Forbid string     `json:"forbid"`
	} `json:"questions"`
}

func Load(path string) (*Scenario, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var s Scenario
	return &s, json.Unmarshal(b, &s)
}

type Row struct {
	ID, User, Query        string
	Smriti, Baseline       string // top answer text
	SmritiOK, BaselineOK   bool
	SmritiStale, BaseStale bool
	Leak                   bool
}

type Report struct {
	Questions                    int
	SmritiCorrect, BaseCorrect   int
	SmritiStale, BaseStale       int
	Leaks                        int
	SmritiTokens, BaselineTokens float64 // avg context tokens per question
	Writes                       map[string]int
	ReplayChanged                int // rows changed by replaying every session again
	Rows                         []Row
}

func contains(s, sub string) bool {
	return sub != "" && strings.Contains(strings.ToLower(s), strings.ToLower(sub))
}

func tokens(s string) float64 { return float64(len(s)) / 4 }

func Run(ctx context.Context, st *store.Store, sc *Scenario, runID string) (*Report, error) {
	org := sc.Org + "-" + runID
	st.Now = func() time.Time { return sc.Now }
	principals := map[string]memory.Principal{}
	writers := map[string]bool{}
	for _, w := range sc.OrgWriters {
		writers[w] = true
	}
	for _, u := range sc.Users {
		principals[u.ID] = memory.Principal{OrgID: org, UserID: u.ID, Teams: u.Teams, OrgWriter: writers[u.ID]}
	}
	type msg struct {
		user, text string
		at         time.Time
	}
	var history []msg
	sessions := sc.Sessions
	sort.SliceStable(sessions, func(i, j int) bool { return sessions[i].At.Before(sessions[j].At) })
	convs := make([]memory.Conversation, len(sessions))
	for i, s := range sessions {
		c := memory.Conversation{ID: s.ID}
		for j, t := range s.Messages {
			at := s.At.Add(time.Duration(j) * time.Minute)
			c.Messages = append(c.Messages, memory.Message{ID: fmt.Sprint(j), Role: "user", Text: t, At: at})
			history = append(history, msg{s.User, t, at})
		}
		convs[i] = c
	}

	rep := &Report{Writes: map[string]int{}}
	var ex extract.Rules
	write := func(i int) ([]store.WriteResult, error) {
		s := sessions[i]
		cands, err := ex.Extract(ctx, convs[i], extract.Context{Team: s.Team})
		if err != nil {
			return nil, err
		}
		return st.Write(ctx, principals[s.User], cands)
	}
	for i := range sessions {
		res, err := write(i)
		if err != nil {
			return nil, fmt.Errorf("session %s: %w", sessions[i].ID, err)
		}
		for _, r := range res {
			rep.Writes[string(r.Action)]++
		}
	}
	// Replay everything in reverse order: nothing may change.
	for i := len(sessions) - 1; i >= 0; i-- {
		res, err := write(i)
		if err != nil {
			return nil, err
		}
		for _, r := range res {
			switch r.Action {
			case memory.Add, memory.Confirm, memory.Supersede:
				rep.ReplayChanged++
			}
		}
	}

	emb := st.Embedder
	for _, q := range sc.Questions {
		p := principals[q.User]
		row := Row{ID: q.ID, User: q.User, Query: q.Query}
		got, err := st.Recall(ctx, p, q.Query, store.RecallOptions{K: 3, AsOf: q.AsOf})
		if err != nil {
			return nil, err
		}
		var ctxTok float64
		for _, r := range got {
			ctxTok += tokens(r.Memory.Content)
			if contains(r.Memory.Content, q.Forbid) || !visibleTo(r.Memory, p) {
				row.Leak = true
			}
		}
		if len(got) > 0 {
			row.Smriti = got[0].Memory.Content
		}
		row.SmritiOK = q.Expect == "" && !row.Leak || contains(row.Smriti, q.Expect)
		row.SmritiStale = contains(row.Smriti, q.Stale)

		// Baseline: most similar raw message from this user's own history.
		at := sc.Now
		if q.AsOf != nil {
			at = *q.AsOf
		}
		qv, _ := emb.Embed(ctx, []string{q.Query})
		best, bestSim := "", float32(-2)
		var full float64
		for _, h := range history {
			if h.user != q.User || h.at.After(at) {
				continue
			}
			full += tokens(h.text)
			hv, _ := emb.Embed(ctx, []string{h.text})
			if s := embed.Cosine(qv[0], hv[0]); s > bestSim {
				best, bestSim = h.text, s
			}
		}
		row.Baseline = best
		row.BaselineOK = q.Expect == "" && !contains(best, q.Forbid) || contains(best, q.Expect) && !contains(best, q.Stale)
		row.BaseStale = contains(best, q.Stale)

		rep.Questions++
		rep.SmritiTokens += ctxTok
		rep.BaselineTokens += full
		if row.SmritiOK && !row.SmritiStale {
			rep.SmritiCorrect++
		}
		if row.SmritiStale {
			rep.SmritiStale++
		}
		if row.BaselineOK {
			rep.BaseCorrect++
		}
		if row.BaseStale {
			rep.BaseStale++
		}
		if row.Leak {
			rep.Leaks++
		}
		rep.Rows = append(rep.Rows, row)
	}
	n := float64(max(rep.Questions, 1))
	rep.SmritiTokens /= n
	rep.BaselineTokens /= n
	return rep, nil
}

func visibleTo(m memory.Memory, p memory.Principal) bool {
	switch m.Scope {
	case memory.User:
		return m.OwnerID == p.UserID
	case memory.Team:
		for _, t := range p.Teams {
			if t == m.OwnerID {
				return true
			}
		}
		return false
	}
	return m.OrgID == p.OrgID
}

func Print(w io.Writer, r *Report, verbose bool) {
	n := float64(max(r.Questions, 1))
	fmt.Fprintf(w, "writes: %v   replay of all sessions changed %d rows\n\n", r.Writes, r.ReplayChanged)
	fmt.Fprintf(w, "%-22s %9s %7s %6s %14s\n", "system", "correct", "stale", "leaks", "tokens/query")
	fmt.Fprintf(w, "%-22s %9.3f %7.3f %6d %14.1f\n", "smriti (top-1 memory)", float64(r.SmritiCorrect)/n, float64(r.SmritiStale)/n, r.Leaks, r.SmritiTokens)
	fmt.Fprintf(w, "%-22s %9.3f %7.3f %6s %14.1f\n", "raw history (top-1)", float64(r.BaseCorrect)/n, float64(r.BaseStale)/n, "-", r.BaselineTokens)
	if !verbose {
		return
	}
	fmt.Fprintln(w)
	for _, row := range r.Rows {
		mark := func(ok, stale bool) string {
			switch {
			case stale:
				return "STALE"
			case ok:
				return "ok"
			}
			return "MISS"
		}
		fmt.Fprintf(w, "%s %-8s %-42q smriti=%-5s %-48.48q base=%-5s %.48q\n", row.ID, row.User, row.Query,
			mark(row.SmritiOK, row.SmritiStale), row.Smriti, mark(row.BaselineOK, row.BaseStale), row.Baseline)
	}
}
