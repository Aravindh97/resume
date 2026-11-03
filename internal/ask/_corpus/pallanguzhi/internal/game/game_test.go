package game

import (
	"errors"
	"math/rand/v2"
	"testing"
)

func board(turn int, pits map[int]int) Board {
	b := Board{Turn: turn}
	for i, n := range pits {
		b.Pits[i] = n
	}
	return b
}

func TestStart(t *testing.T) {
	b := New()
	if b.Total() != 148 || b.Pits[3] != 2 || b.Pits[10] != 2 || len(b.Moves()) != 7 {
		t.Fatalf("bad start: %+v", b)
	}
}

// Hand-worked positions, one per rule.
func TestRules(t *testing.T) {
	cases := []struct {
		name  string
		b     Board
		pit   int
		pits  map[int]int // expected non-zero pits after
		store [2]int
	}{
		{"empty then capture the pit beyond", board(South, map[int]int{0: 2, 4: 5, 9: 1}), 0,
			map[int]int{1: 1, 2: 1, 9: 1}, [2]int{5, 0}},
		{"pasu: a pit reaching 6 is captured", board(South, map[int]int{0: 1, 1: 5, 9: 1}), 0,
			map[int]int{9: 1}, [2]int{6, 0}},
		{"next pit full: lift and keep sowing", board(South, map[int]int{0: 1, 2: 3, 7: 4, 12: 1}), 0,
			map[int]int{1: 1, 3: 1, 4: 1, 5: 1, 12: 1}, [2]int{4, 0}},
		{"two empty pits: turn ends", board(South, map[int]int{0: 1, 9: 2}), 0,
			map[int]int{1: 1, 9: 2}, [2]int{0, 0}},
		{"sowing wraps from 13 to 0", board(North, map[int]int{13: 2, 3: 1, 5: 1}), 13,
			map[int]int{0: 1, 1: 1, 5: 1}, [2]int{0, 1}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			before, turn := c.b.Total(), c.b.Turn
			var tr []Event
			if err := c.b.Play(c.pit, &tr); err != nil {
				t.Fatal(err)
			}
			want := board(0, c.pits).Pits
			if c.b.Pits != want || c.b.Store != c.store {
				t.Fatalf("got pits %v store %v, want %v %v", c.b.Pits, c.b.Store, want, c.store)
			}
			if c.b.Total() != before || tr[len(tr)-1].Kind != "end" || c.b.Turn != 1-turn {
				t.Fatalf("total %d->%d, trace end %q, turn %d", before, c.b.Total(), tr[len(tr)-1].Kind, c.b.Turn)
			}
		})
	}
}

func TestIllegal(t *testing.T) {
	b := New()
	for _, p := range []int{-1, 7, 14} {
		if err := b.Play(p, nil); !errors.Is(err, ErrIllegal) {
			t.Fatalf("pit %d: %v", p, err)
		}
	}
	b.Pits[0] = 0
	if err := b.Play(0, nil); !errors.Is(err, ErrIllegal) {
		t.Fatal("empty pit allowed")
	}
}

// Over many random games no seed is created or lost, every game ends, and
// no turn hits the relay limit.
func TestRandomGames(t *testing.T) {
	r := rand.New(rand.NewPCG(1, 2))
	for g := 0; g < 5000; g++ {
		b := New()
		for n := 0; !b.Over(); n++ {
			if n > 1000 {
				t.Fatal("game did not end")
			}
			var tr []Event
			m := b.Moves()
			b.Play(m[r.IntN(len(m))], &tr)
			for _, e := range tr {
				if e.Kind == "limit" {
					t.Fatal("relay limit reached")
				}
			}
			if b.Total() != 148 {
				t.Fatalf("game %d: %d seeds", g, b.Total())
			}
		}
		if f := b.Final(); f[0]+f[1] != 148 {
			t.Fatalf("final %v", f)
		}
	}
}
