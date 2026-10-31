//go:build integration

package store_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Aravindh97/smriti/internal/embed"
	"github.com/Aravindh97/smriti/internal/extract"
	"github.com/Aravindh97/smriti/internal/mcp"
	"github.com/Aravindh97/smriti/internal/memory"
	"github.com/Aravindh97/smriti/internal/store"
)

func open(t *testing.T) (*store.Store, string) {
	t.Helper()
	url := os.Getenv("DATABASE_URL")
	if url == "" {
		t.Skip("DATABASE_URL not set")
	}
	s, err := store.Open(context.Background(), url, embed.NewHashing(384))
	if err != nil {
		t.Fatal(err)
	}
	org := fmt.Sprintf("test-%d", time.Now().UnixNano())
	t.Cleanup(s.DB.Close)
	t.Cleanup(func() {
		ctx := context.Background()
		s.DB.Exec(ctx, `UPDATE memories SET superseded_by=NULL WHERE org_id=$1`, org)
		s.DB.Exec(ctx, `DELETE FROM memories WHERE org_id=$1`, org)
		s.DB.Exec(ctx, `DELETE FROM memory_writes WHERE org_id=$1`, org)
	})
	return s, org
}

func cand(scope memory.Scope, team, subj, content, src string, at time.Time) memory.Candidate {
	return memory.Candidate{Scope: scope, Team: team, Subject: subj, Content: content, Confidence: 0.9, SourceRef: src, ObservedAt: at}
}

var t0 = time.Date(2026, 1, 1, 9, 0, 0, 0, time.UTC)

func TestConcurrentWritersLeaveOneCurrentValue(t *testing.T) {
	s, org := open(t)
	ctx := context.Background()
	p := memory.Principal{OrgID: org, UserID: "anita"}
	var wg sync.WaitGroup
	errs := make(chan error, 20)
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, err := s.Write(ctx, p, []memory.Candidate{cand(memory.User, "", "manager",
				fmt.Sprintf("manager is person%d", i), fmt.Sprintf("c%d/0", i), t0.Add(time.Duration(i)*time.Minute))})
			errs <- err
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	var current, total int
	s.DB.QueryRow(ctx, `SELECT count(*) FILTER (WHERE valid_to IS NULL), count(*) FROM memories WHERE org_id=$1`, org).Scan(&current, &total)
	if current != 1 {
		t.Fatalf("current rows = %d (total %d)", current, total)
	}
}

func TestAccessScopes(t *testing.T) {
	s, org := open(t)
	ctx := context.Background()
	anita := memory.Principal{OrgID: org, UserID: "anita", Teams: []string{"design"}}
	rahul := memory.Principal{OrgID: org, UserID: "rahul", Teams: []string{"design"}}
	joseph := memory.Principal{OrgID: org, UserID: "joseph", Teams: []string{"ops"}}
	hr := memory.Principal{OrgID: org, UserID: "meena", OrgWriter: true}
	other := memory.Principal{OrgID: org + "-other", UserID: "anita", Teams: []string{"design"}}

	s.Write(ctx, anita, []memory.Candidate{cand(memory.User, "", "manager", "Anita's manager is Arjun", "a/1", t0),
		cand(memory.Team, "design", "tool for mockups", "Team uses Penpot for mockups", "a/2", t0)})
	res, _ := s.Write(ctx, joseph, []memory.Candidate{cand(memory.Team, "design", "tool", "Team uses Paint", "j/1", t0),
		cand(memory.Org, "", "payroll", "Payroll is the 15th", "j/2", t0)})
	if res[0].Action != store.Denied || res[1].Action != store.Denied {
		t.Fatalf("joseph wrote outside his scopes: %+v", res)
	}
	s.Write(ctx, hr, []memory.Candidate{cand(memory.Org, "", "payroll", "Payroll is the last working day", "m/1", t0)})

	see := func(p memory.Principal, q string) string {
		r, err := s.Recall(ctx, p, q, store.RecallOptions{K: 10})
		if err != nil {
			t.Fatal(err)
		}
		var out []string
		for _, x := range r {
			out = append(out, x.Memory.Content)
		}
		return strings.Join(out, " | ")
	}
	if got := see(rahul, "mockups tool manager payroll"); !strings.Contains(got, "Penpot") || strings.Contains(got, "Arjun") || !strings.Contains(got, "last working day") {
		t.Errorf("rahul sees: %s", got)
	}
	if got := see(joseph, "mockups tool manager payroll"); strings.Contains(got, "Penpot") || strings.Contains(got, "Arjun") {
		t.Errorf("joseph sees: %s", got)
	}
	if got := see(other, "mockups manager payroll"); got != "" {
		t.Errorf("another org sees: %s", got)
	}
}

func TestBitemporalSupersedeAndAsOf(t *testing.T) {
	s, org := open(t)
	ctx := context.Background()
	p := memory.Principal{OrgID: org, UserID: "anita"}
	mar := t0.AddDate(0, 2, 0)
	s.Write(ctx, p, []memory.Candidate{cand(memory.User, "", "manager", "manager is Priya", "s1/0", t0)})
	r, _ := s.Write(ctx, p, []memory.Candidate{cand(memory.User, "", "manager", "manager is Arjun", "s2/0", mar)})
	if r[0].Action != memory.Supersede || r[0].Replaced == "" {
		t.Fatalf("%+v", r)
	}
	// A late replay of an even older statement must not win.
	r, _ = s.Write(ctx, p, []memory.Candidate{cand(memory.User, "", "manager", "manager is Ravi", "s0/0", t0.AddDate(0, -1, 0))})
	if r[0].Action != memory.Ignore {
		t.Fatalf("late older statement: %+v", r)
	}
	now, _ := s.Recall(ctx, p, "who is my manager", store.RecallOptions{K: 1})
	feb := t0.AddDate(0, 1, 0)
	then, _ := s.Recall(ctx, p, "who is my manager", store.RecallOptions{K: 1, AsOf: &feb})
	if now[0].Memory.Content != "manager is Arjun" || then[0].Memory.Content != "manager is Priya" {
		t.Fatalf("now=%q then=%q", now[0].Memory.Content, then[0].Memory.Content)
	}
	h, _ := s.History(ctx, p, "manager")
	if len(h) != 2 || h[0].SupersededBy != h[1].ID || !h[0].ValidTo.Equal(mar) {
		t.Fatalf("history: %+v", h)
	}
	// Replaying a processed message is a no-op.
	r, _ = s.Write(ctx, p, []memory.Candidate{cand(memory.User, "", "manager", "manager is Arjun", "s2/0", mar)})
	if r[0].Action != store.Duplicate {
		t.Fatalf("replay: %+v", r)
	}
	n, err := s.Forget(ctx, p, h[1].ID)
	if err != nil || n != 2 {
		t.Fatalf("forget: %d %v", n, err)
	}
	if h, _ = s.History(ctx, p, "manager"); len(h) != 0 {
		t.Fatalf("history after forget: %+v", h)
	}
}

func TestExpireAndConsolidate(t *testing.T) {
	s, org := open(t)
	ctx := context.Background()
	p := memory.Principal{OrgID: org, UserID: "karthik"}
	c := cand(memory.User, "", "availability", "travelling to Dubai", "k/1", t0)
	c.TTL = 48 * time.Hour
	s.Now = func() time.Time { return t0.Add(time.Hour) }
	s.Write(ctx, p, []memory.Candidate{c})
	// Insert a duplicate under a different subject directly, bypassing the
	// write-time check, as an older version of the service might have.
	s.Write(ctx, p, []memory.Candidate{cand(memory.User, "", "region", "region is Kerala", "k/2", t0)})
	s.DB.Exec(ctx, `INSERT INTO memories (org_id, scope, owner_id, subject, content, embedding, confidence, source_ref, valid_from, last_confirmed)
		SELECT org_id, scope, owner_id, 'sales_region', content, embedding, confidence, 'k/3', valid_from, last_confirmed
		FROM memories WHERE org_id=$1 AND subject='region'`, org)

	if r, _ := s.Recall(ctx, p, "am I travelling", store.RecallOptions{K: 5}); !strings.Contains(fmt.Sprint(r), "Dubai") {
		t.Fatal("availability should be visible before expiry")
	}
	s.Now = func() time.Time { return t0.Add(72 * time.Hour) }
	if n, err := s.ExpireDue(ctx); err != nil || n != 1 {
		t.Fatalf("expired %d %v", n, err)
	}
	if n, err := s.Consolidate(ctx, 0.95, 100); err != nil || n != 1 {
		t.Fatalf("merged %d %v", n, err)
	}
	var current int
	s.DB.QueryRow(ctx, `SELECT count(*) FROM memories WHERE org_id=$1 AND valid_to IS NULL`, org).Scan(&current)
	if current != 1 {
		t.Fatalf("current = %d", current)
	}
}

func TestMCPServer(t *testing.T) {
	s, org := open(t)
	srv := &mcp.Server{Store: s, Extractor: extract.Rules{}, Principal: memory.Principal{OrgID: org, UserID: "anita"}}
	in := strings.Join([]string{
		`{"jsonrpc":"2.0","id":1,"method":"initialize"}`,
		`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"remember","arguments":{"text":"My manager is Arjun Mehta."}}}`,
		`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"recall","arguments":{"query":"who is my manager"}}}`,
		`{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"recall","arguments":{"query":"x","user_id":"rahul"}}}`,
	}, "\n")
	var out bytes.Buffer
	if err := srv.Serve(context.Background(), strings.NewReader(in), &out); err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	if len(lines) != 4 || !strings.Contains(lines[2], "Arjun Mehta") {
		t.Fatalf("%s", out.String())
	}
	var last map[string]any
	json.Unmarshal([]byte(lines[3]), &last)
	if last["result"].(map[string]any)["isError"] != true {
		t.Fatalf("unknown argument user_id must be rejected: %s", lines[3])
	}
}
