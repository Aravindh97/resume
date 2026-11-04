package tsgen

import (
	"strings"
	"testing"
	"time"
)

type inner struct {
	N int `json:"n"`
}
type Kind string
type Embedded struct {
	E string `json:"e"`
}
type outer struct {
	Embedded
	Name    string         `json:"name"`
	Opt     string         `json:"opt,omitempty"`
	Skip    string         `json:"-"`
	Ptr     *inner         `json:"ptr"`
	List    []inner        `json:"list"`
	Arr     [2]int         `json:"arr"`
	M       map[string]int `json:"m"`
	At      time.Time      `json:"at"`
	K       Kind           `json:"k"`
	Raw     []byte         `json:"raw"`
	NoTag   bool
	private int
}

func TestWrite(t *testing.T) {
	var b strings.Builder
	if err := Write(&b, "test", outer{}); err != nil {
		t.Fatal(err)
	}
	got := b.String()
	for _, want := range []string{
		"export interface outer {", "  e: string;", "  name: string;", "  opt?: string;",
		"  ptr?: inner | null;", "  list: inner[] | null;", "  arr: number[];", "  m: Record<string, number>;",
		"  at: string;", "  k: Kind;", "  raw: string;", "  NoTag: boolean;",
		"export interface inner {\n  n: number;\n}", "export type Kind = string;",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in:\n%s", want, got)
		}
	}
	for _, bad := range []string{"Skip", "private"} {
		if strings.Contains(got, bad) {
			t.Errorf("%s should be left out", bad)
		}
	}
}
