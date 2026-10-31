package career

import (
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestCherryPick(t *testing.T) {
	out := CherryPick([]string{"go", "temporal"})
	for _, want := range []string{"granth", "smriti"} {
		if !strings.Contains(out, "feat("+want+")") {
			t.Errorf("missing %s:\n%s", want, out)
		}
	}
	if strings.Contains(out, "feat(sutra)") {
		t.Error("sutra does not use temporal")
	}
}

func TestShowAndUnknown(t *testing.T) {
	if s, err := Show("granth"); err != nil || !strings.Contains(s, "hit@5 0.977") {
		t.Fatal(s, err)
	}
	if _, err := Run([]string{"rebase"}); err == nil {
		t.Fatal("unknown command accepted")
	}
}

func TestInitWritesRealRepo(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	dir := filepath.Join(t.TempDir(), "career")
	if err := Init(dir); err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command("git", "-C", dir, "log", "--format=%ad %s", "--date=short").Output()
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	if len(lines) != len(Milestones) || !strings.HasPrefix(lines[len(lines)-1], "2015-01-02 init:") {
		t.Fatalf("git log:\n%s", out)
	}
	if err := Init(dir); err == nil {
		t.Fatal("init into a non-empty dir should fail")
	}
}
