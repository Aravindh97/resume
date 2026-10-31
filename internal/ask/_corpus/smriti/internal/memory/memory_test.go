package memory

import (
	"testing"
	"time"
)

func cur(subject, content string, from time.Time) Memory {
	return Memory{ID: subject, Subject: subject, Content: content, ValidFrom: from, LastConfirmed: from}
}

func TestDecide(t *testing.T) {
	p := DefaultPolicy()
	jan := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	mar := jan.AddDate(0, 2, 0)
	cases := []struct {
		name     string
		cand     Candidate
		existing []Existing
		want     Action
	}{
		{"new fact", Candidate{Subject: "manager", Content: "manager is Priya", Confidence: 0.9}, nil, Add},
		{"low confidence", Candidate{Subject: "manager", Content: "x", Confidence: 0.2}, nil, Ignore},
		{"same value with filler word", Candidate{Subject: "Manager", Content: "User's manager is Arjun Mehta now", Confidence: 0.9},
			[]Existing{{cur("manager", "User's manager is Arjun Mehta", jan), 0.97}}, Confirm},
		// Near-identical embeddings, different value: must supersede.
		{"value changed despite high similarity", Candidate{Subject: "preference voice calls", Content: "Prefers English for voice calls", Confidence: 0.8},
			[]Existing{{cur("preference_voice_calls", "Prefers Tamil for voice calls", jan), 0.96}}, Supersede},
		{"older statement does not overwrite newer", Candidate{Subject: "manager", Content: "manager is Priya", Confidence: 0.9, ObservedAt: jan},
			[]Existing{{cur("manager", "manager is Arjun", mar), 0.8}}, Ignore},
		{"near duplicate under another subject", Candidate{Subject: "reporting manager", Content: "manager is Arjun", Confidence: 0.9},
			[]Existing{{cur("manager", "manager is Arjun", jan), 0.95}}, Confirm},
		{"unrelated memory below threshold", Candidate{Subject: "region", Content: "region is Kerala", Confidence: 0.9},
			[]Existing{{cur("manager", "manager is Arjun", jan), 0.3}}, Add},
	}
	for _, c := range cases {
		if got := p.Decide(c.cand, c.existing).Action; got != c.want {
			t.Errorf("%s: got %s, want %s", c.name, got, c.want)
		}
	}
}

func TestPrincipalOwner(t *testing.T) {
	p := Principal{OrgID: "o", UserID: "u", Teams: []string{"design"}}
	if o, err := p.Owner(Team, "design"); err != nil || o != "design" {
		t.Fatal(o, err)
	}
	if _, err := p.Owner(Team, "sales"); err == nil {
		t.Fatal("wrote to a team the user is not in")
	}
	if _, err := p.Owner(Org, ""); err == nil {
		t.Fatal("wrote org memory without OrgWriter")
	}
	p.OrgWriter = true
	if o, err := p.Owner(Org, ""); err != nil || o != "o" {
		t.Fatal(o, err)
	}
}

func TestScorePrefersRecentAndConfirmed(t *testing.T) {
	w := DefaultWeights()
	now := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	old := Memory{LastConfirmed: now.AddDate(0, -6, 0), Confirmations: 1}
	fresh := Memory{LastConfirmed: now.AddDate(0, 0, -1), Confirmations: 1}
	if w.Score(0.6, fresh, now) <= w.Score(0.6, old, now) {
		t.Error("recency not rewarded")
	}
	sure := Memory{LastConfirmed: old.LastConfirmed, Confirmations: 8}
	if w.Score(0.6, sure, now) <= w.Score(0.6, old, now) {
		t.Error("confirmations not rewarded")
	}
	if w.Score(0.9, old, now) <= w.Score(0.5, fresh, now) {
		t.Error("relevance should dominate")
	}
}

func TestNormSubject(t *testing.T) {
	for in, want := range map[string]string{"Preferred  Name!": "preferred_name", " tool for lead-tracking ": "tool_for_lead_tracking"} {
		if got := NormSubject(in); got != want {
			t.Errorf("%q -> %q, want %q", in, got, want)
		}
	}
}
