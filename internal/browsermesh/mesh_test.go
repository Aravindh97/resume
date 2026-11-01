package browsermesh

import (
	"context"
	"testing"
	"time"

	"github.com/Aravindh97/resume/internal/raft"
)

func leader(t *testing.T, m *Mesh) string {
	t.Helper()
	for i := 0; i < 300; i++ {
		for _, s := range m.Status() {
			if s.Role == raft.Leader && !s.Isolated {
				return s.ID
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("no leader")
	return ""
}

func TestMeshFailoverAndQuorumGuard(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	m := New(ctx, 2*time.Second)
	old := leader(t, m)
	if _, err := m.Write(ctx); err != nil {
		t.Fatal(err)
	}
	if id, err := m.Isolate("leader"); err != nil || id != old {
		t.Fatalf("isolate: %s %v", id, err)
	}
	if _, err := m.Isolate("node-1"); err != ErrQuorum && old != "node-1" {
		t.Fatalf("second isolation allowed: %v", err)
	}
	var nl string
	for nl = leader(t, m); nl == old; nl = leader(t, m) {
		time.Sleep(20 * time.Millisecond)
	}
	if _, err := m.Write(ctx); err != nil {
		t.Fatalf("write with 3 of 4: %v", err)
	}
	time.Sleep(3 * time.Second) // partition heals, old leader catches up
	for _, s := range m.Status() {
		if s.Version != 2 {
			t.Errorf("%s at version %d, want 2", s.ID, s.Version)
		}
	}
}
