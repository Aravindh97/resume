//go:build js && wasm

// Command web runs smriti in the browser: chat as one of the scenario's
// users, watch facts get added, confirmed, superseded or denied, recall what
// an agent would see (now or as of a past date), inspect history, forget,
// replay everything, and run the eval against the raw-history baseline. The
// store is in memory with the same rules as the pgvector store.
package main

import (
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"net/url"
	"sort"
	"sync"
	"time"

	"github.com/Aravindh97/smriti/internal/embed"
	"github.com/Aravindh97/smriti/internal/eval"
	"github.com/Aravindh97/smriti/internal/extract"
	"github.com/Aravindh97/smriti/internal/memory"
	"github.com/Aravindh97/smriti/internal/store"
)

//go:embed embed/scenario.json
var scenarioJSON []byte

var (
	mu         sync.Mutex
	sc         eval.Scenario
	st         *store.Mem
	principals map[string]memory.Principal
	clock      time.Time
	said       int
)

const org = "kaveri"

func newStore() *store.Mem { return store.NewMem(embed.NewHashing(384)) }

func load() error {
	if err := json.Unmarshal(scenarioJSON, &sc); err != nil {
		return err
	}
	st = newStore()
	clock = sc.Now
	said = 0
	st.SetClock(func() time.Time { return clock })
	principals = map[string]memory.Principal{}
	writers := map[string]bool{}
	for _, w := range sc.OrgWriters {
		writers[w] = true
	}
	for _, u := range sc.Users {
		principals[u.ID] = memory.Principal{OrgID: org, UserID: u.ID, Teams: u.Teams, OrgWriter: writers[u.ID]}
	}
	_, err := replay()
	return err
}

// replay feeds every scenario session through the extractor and the store,
// oldest first, and counts rows that changed.
func replay() (map[string]int, error) {
	ctx := context.Background()
	sessions := append(sc.Sessions[:0:0], sc.Sessions...)
	sort.SliceStable(sessions, func(i, j int) bool { return sessions[i].At.Before(sessions[j].At) })
	counts := map[string]int{}
	var ex extract.Rules
	for _, s := range sessions {
		c := memory.Conversation{ID: s.ID}
		for j, t := range s.Messages {
			c.Messages = append(c.Messages, memory.Message{ID: fmt.Sprint(j), Role: "user", Text: t, At: s.At.Add(time.Duration(j) * time.Minute)})
		}
		cands, err := ex.Extract(ctx, c, extract.Context{Team: s.Team})
		if err != nil {
			return nil, err
		}
		res, err := st.Write(ctx, principals[s.User], cands)
		if err != nil {
			return nil, err
		}
		for _, r := range res {
			counts[string(r.Action)]++
		}
	}
	return counts, nil
}

func principal(q url.Values, name string) (memory.Principal, bool) {
	p, ok := principals[name]
	return p, ok
}

type view struct {
	ID, Scope, Owner, Subject, Content, Source string
	Confirmations                              int
	From                                       string
	To                                         string `json:",omitempty"`
	Current                                    bool
}

func toView(m memory.Memory) view {
	v := view{ID: m.ID, Scope: string(m.Scope), Owner: m.OwnerID, Subject: m.Subject, Content: m.Content, Source: m.SourceRef,
		Confirmations: m.Confirmations, From: m.ValidFrom.Format("2006-01-02 15:04"), Current: m.ValidTo == nil}
	if m.ValidTo != nil {
		v.To = m.ValidTo.Format("2006-01-02 15:04")
	}
	return v
}

// visible lists every row the principal may read, current first.
func visible(p memory.Principal) []view {
	var out []view
	for _, m := range st.All(org) {
		ok := m.Scope == memory.Org || m.Scope == memory.User && m.OwnerID == p.UserID
		for _, t := range p.Teams {
			ok = ok || m.Scope == memory.Team && m.OwnerID == t
		}
		if ok {
			out = append(out, toView(m))
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Current != out[j].Current {
			return out[i].Current
		}
		if out[i].Scope != out[j].Scope {
			return out[i].Scope > out[j].Scope // user, team, org
		}
		return out[i].Subject < out[j].Subject
	})
	return out
}

func main() {
	if err := load(); err != nil {
		panic(err)
	}
	ctx := context.Background()
	serve(map[string]handler{
		"GET users": func(url.Values, []byte) resp {
			mu.Lock()
			defer mu.Unlock()
			var out []map[string]any
			for _, u := range sc.Users {
				out = append(out, map[string]any{"id": u.ID, "teams": u.Teams, "org_writer": principals[u.ID].OrgWriter})
			}
			return ok(map[string]any{"users": out, "now": clock.Format("2006-01-02 15:04"), "sessions": len(sc.Sessions)})
		},
		"GET memories": func(q url.Values, _ []byte) resp {
			mu.Lock()
			defer mu.Unlock()
			p, found := principal(q, q.Get("user"))
			if !found {
				return fail(404, "no such user")
			}
			return ok(visible(p))
		},
		"POST say": func(_ url.Values, b []byte) resp {
			var in struct{ User, Text string }
			if json.Unmarshal(b, &in) != nil || len(in.Text) == 0 || len(in.Text) > 2000 {
				return fail(400, "say something (up to 2000 characters)")
			}
			mu.Lock()
			defer mu.Unlock()
			p, found := principals[in.User]
			if !found {
				return fail(404, "no such user")
			}
			said++
			clock = clock.Add(time.Hour)
			team := ""
			if len(p.Teams) > 0 {
				team = p.Teams[0]
			}
			conv := memory.Conversation{ID: fmt.Sprintf("live-%d", said), Messages: []memory.Message{{ID: "0", Role: "user", Text: in.Text, At: clock}}}
			cands, err := extract.Rules{}.Extract(ctx, conv, extract.Context{Team: team})
			if err != nil {
				return fail(500, err.Error())
			}
			res, err := st.Write(ctx, p, cands)
			if err != nil {
				return fail(500, err.Error())
			}
			// Send the same message again, as a retried webhook would.
			again, _ := st.Write(ctx, p, cands)
			dupes := 0
			for _, r := range again {
				if r.Action == store.Duplicate {
					dupes++
				}
			}
			return ok(map[string]any{"results": res, "retry_duplicates": dupes, "now": clock.Format("2006-01-02 15:04"), "memories": visible(p)})
		},
		"POST recall": func(_ url.Values, b []byte) resp {
			var in struct{ User, Query, AsOf string }
			if json.Unmarshal(b, &in) != nil || in.Query == "" {
				return fail(400, "query required")
			}
			mu.Lock()
			defer mu.Unlock()
			p, found := principals[in.User]
			if !found {
				return fail(404, "no such user")
			}
			o := store.RecallOptions{K: 3}
			if in.AsOf != "" {
				t, err := time.Parse("2006-01-02", in.AsOf)
				if err != nil {
					return fail(400, "as-of must be YYYY-MM-DD")
				}
				t = t.Add(24*time.Hour - time.Second)
				o.AsOf = &t
			}
			got, err := st.Recall(ctx, p, in.Query, o)
			if err != nil {
				return fail(500, err.Error())
			}
			var out []map[string]any
			for _, r := range got {
				out = append(out, map[string]any{"memory": toView(r.Memory), "similarity": r.Similarity, "score": r.Score})
			}
			return ok(out)
		},
		"GET history": func(q url.Values, _ []byte) resp {
			mu.Lock()
			defer mu.Unlock()
			p, found := principals[q.Get("user")]
			if !found {
				return fail(404, "no such user")
			}
			h, _ := st.History(ctx, p, q.Get("subject"))
			var out []view
			for _, m := range h {
				out = append(out, toView(m))
			}
			return ok(out)
		},
		"POST forget": func(_ url.Values, b []byte) resp {
			var in struct{ User, ID string }
			json.Unmarshal(b, &in)
			mu.Lock()
			defer mu.Unlock()
			p, found := principals[in.User]
			if !found {
				return fail(404, "no such user")
			}
			n, err := st.Forget(ctx, p, in.ID)
			if err != nil {
				return fail(403, "only your own user memories can be forgotten")
			}
			return ok(map[string]any{"deleted_versions": n, "memories": visible(p)})
		},
		"POST replay": func(url.Values, []byte) resp {
			mu.Lock()
			defer mu.Unlock()
			c, err := replay()
			if err != nil {
				return fail(500, err.Error())
			}
			changed := c[string(memory.Add)] + c[string(memory.Confirm)] + c[string(memory.Supersede)]
			return ok(map[string]any{"actions": c, "changed": changed})
		},
		"POST reset": func(url.Values, []byte) resp {
			mu.Lock()
			defer mu.Unlock()
			if err := load(); err != nil {
				return fail(500, err.Error())
			}
			return ok("reset")
		},
		"POST eval": func(url.Values, []byte) resp {
			var s eval.Scenario
			json.Unmarshal(scenarioJSON, &s)
			rep, err := eval.Run(ctx, newStore(), &s, "web")
			if err != nil {
				return fail(500, err.Error())
			}
			return ok(rep)
		},
	})
}
