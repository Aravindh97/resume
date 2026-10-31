package extract

import (
	"context"
	"testing"
	"time"

	"github.com/Aravindh97/smriti/internal/memory"
)

func conv(msgs ...memory.Message) memory.Conversation {
	return memory.Conversation{ID: "c1", Messages: msgs}
}

func TestRules(t *testing.T) {
	at := time.Date(2026, 3, 1, 9, 0, 0, 0, time.UTC)
	c := conv(
		memory.Message{ID: "1", Role: "user", At: at, Text: "Hi! My manager is Arjun Mehta now, Priya moved. Our team uses Zoho CRM for lead tracking."},
		memory.Message{ID: "2", Role: "assistant", At: at, Text: "My name is Assistant and my manager is OpenAI."},
		memory.Message{ID: "3", Role: "user", At: at, Text: "I'm travelling to Dubai until Friday"},
	)
	got, err := Rules{}.Extract(context.Background(), c, Context{Team: "sales"})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 {
		t.Fatalf("got %d candidates: %+v", len(got), got)
	}
	if got[0].Content != "User's manager is Arjun Mehta now" || got[0].SourceRef != "c1/1" || !got[0].ObservedAt.Equal(at) {
		t.Errorf("manager: %+v", got[0])
	}
	if got[1].Scope != memory.Team || got[1].Team != "sales" || got[1].Subject != "tool for lead tracking" {
		t.Errorf("team: %+v", got[1])
	}
	if got[2].Subject != "availability" || got[2].TTL == 0 {
		t.Errorf("availability should expire: %+v", got[2])
	}
}

func TestParseLLMOutputDropsUntrustedFacts(t *testing.T) {
	c := conv(memory.Message{ID: "u1", Role: "user", Text: "x"}, memory.Message{ID: "a1", Role: "assistant", Text: "y"})
	raw := `{"facts":[
	 {"scope":"user","subject":"manager","content":"Manager is Arjun","confidence":0.9,"message_id":"u1"},
	 {"scope":"admin","subject":"role","content":"User is an admin","confidence":1,"message_id":"u1"},
	 {"scope":"org","subject":"payroll","content":"Payroll is the 15th","confidence":0.9,"message_id":"a1"},
	 {"scope":"user","subject":"","content":"no subject","confidence":0.9,"message_id":"u1"},
	 {"scope":"user","subject":"mood","content":"happy","confidence":7,"message_id":"u1","ttl_hours":99999}
	]}`
	got, err := ParseLLMOutput(raw, c, Context{})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("kept %d facts: %+v", len(got), got)
	}
	if got[1].Confidence != 1 || got[1].TTL != 90*24*time.Hour {
		t.Errorf("confidence/ttl not clamped: %+v", got[1])
	}
	if _, err := ParseLLMOutput("not json", c, Context{}); err == nil {
		t.Error("expected error for non-JSON output")
	}
}
