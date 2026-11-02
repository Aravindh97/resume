package eval

import (
	"bytes"
	"context"
	"os"
	"strings"
	"testing"

	"github.com/Aravindh97/smriti/internal/embed"
	"github.com/Aravindh97/smriti/internal/store"
)

// The in-memory store must reproduce the Postgres eval in docs/eval.txt.
func TestMemMatchesPostgresEval(t *testing.T) {
	sc, err := Load("../../testdata/eval/scenario.json")
	if err != nil {
		t.Fatal(err)
	}
	rep, err := Run(context.Background(), store.NewMem(embed.NewHashing(384)), sc, "t")
	if err != nil {
		t.Fatal(err)
	}
	var b bytes.Buffer
	Print(&b, rep, true)
	want, _ := os.ReadFile("../../docs/eval.txt")
	if strings.TrimSpace(b.String()) != strings.TrimSpace(string(want)) {
		t.Fatalf("in-memory eval differs from docs/eval.txt:\n%s", b.String())
	}
}
