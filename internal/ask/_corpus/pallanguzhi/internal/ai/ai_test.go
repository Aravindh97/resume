package ai

import (
	"math/rand/v2"
	"testing"

	"github.com/Aravindh97/pallanguzhi/internal/game"
)

func randomPosition(r *rand.Rand) game.Board {
	b := game.New()
	for k := r.IntN(30); k > 0 && !b.Over(); k-- {
		m := b.Moves()
		b.Play(m[r.IntN(len(m))], nil)
	}
	return b
}

// Alpha-beta and move ordering must never change the answer.
func TestPruningMatchesPlainSearch(t *testing.T) {
	r := rand.New(rand.NewPCG(3, 4))
	saved := 0
	for i := 0; i < 300; i++ {
		b := randomPosition(r)
		for d := 1; d <= 4; d++ {
			a, sa := Value(b, d, true)
			p, sp := Value(b, d, false)
			if a != p {
				t.Fatalf("depth %d: pruned %d plain %d", d, a, p)
			}
			saved += sp.Nodes - sa.Nodes
		}
	}
	if saved <= 0 {
		t.Fatal("pruning searched no fewer nodes")
	}
}

// At depth 1 the AI takes the biggest immediate gain.
func TestTakesTheCapture(t *testing.T) {
	var b game.Board
	b.Pits[0], b.Pits[4], b.Pits[5], b.Pits[9] = 2, 9, 1, 1 // pit 0 captures pit 4's 9
	m, sc, _ := Choose(b, Level{Depth: 1}, nil)
	if m != 0 || sc[0].Captured != 9 {
		t.Fatalf("chose %d: %+v", m, sc)
	}
}

func TestHardIsDeterministicAndDeeper(t *testing.T) {
	b := game.New()
	m1, _, s1 := Choose(b, Hard, rand.New(rand.NewPCG(1, 1)))
	m2, _, s2 := Choose(b, Hard, rand.New(rand.NewPCG(9, 9)))
	if m1 != m2 || s1 != s2 || s1.Depth <= Medium.Depth {
		t.Fatalf("%d %+v vs %d %+v", m1, s1, m2, s2)
	}
}
