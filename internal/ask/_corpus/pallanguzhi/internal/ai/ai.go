// Package ai is a Pallanguzhi opponent: depth-limited negamax search with
// alpha-beta pruning.
//
// The evaluation is the score the round would end with right now (seeds in
// your store plus seeds on your side, minus the opponent's), so at the end
// of the game it is exact.
package ai

import (
	"math/rand/v2"
	"sort"

	"github.com/Aravindh97/pallanguzhi/internal/game"
)

const inf = 1 << 20

// Eval scores b for the player to move.
func Eval(b game.Board) int {
	f := b.Final()
	return f[b.Turn] - f[1-b.Turn]
}

// Stats counts the work a search did.
type Stats struct {
	Nodes int `json:"nodes"`
	Depth int `json:"depth"`
}

type searcher struct {
	stats Stats
	prune bool
}

// negamax returns the value of b for the player to move.
func (s *searcher) negamax(b game.Board, depth, alpha, beta int) int {
	s.stats.Nodes++
	if depth == 0 || b.Over() {
		return Eval(b)
	}
	kids := children(b)
	if s.prune {
		// Try moves that win the most seeds immediately first: better
		// ordering makes alpha-beta cut more.
		sort.SliceStable(kids, func(i, j int) bool { return gain(b, kids[i]) > gain(b, kids[j]) })
	}
	best := -inf
	for _, c := range kids {
		v := -s.negamax(c, depth-1, -beta, -alpha)
		if v > best {
			best = v
		}
		if s.prune {
			alpha = max(alpha, v)
			if alpha >= beta {
				break
			}
		}
	}
	return best
}

func children(b game.Board) []game.Board {
	var out []game.Board
	for _, m := range b.Moves() {
		c, _ := b.After(m)
		out = append(out, c)
	}
	return out
}

// gain is what the move just made (b -> c) captured for the mover.
func gain(b, c game.Board) int { return c.Store[b.Turn] - b.Store[b.Turn] }

// MoveScore is one root move and its value for the mover after searching.
type MoveScore struct {
	Pit      int `json:"pit"`
	Score    int `json:"score"`    // final margin with best play by both, to the search depth
	Captured int `json:"captured"` // seeds this move captures by itself
}

// Analyse scores every legal move. Root moves get exact values (a full
// window each) so they can be shown and compared.
func Analyse(b game.Board, depth int) ([]MoveScore, Stats) {
	s := &searcher{prune: true}
	var out []MoveScore
	for _, m := range b.Moves() {
		c, _ := b.After(m)
		v := -s.negamax(c, depth-1, -inf, inf)
		out = append(out, MoveScore{m, v, gain(b, c)})
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Score > out[j].Score })
	return out, s.stats
}

// Value is the plain negamax value without pruning, for tests.
func Value(b game.Board, depth int, prune bool) (int, Stats) {
	s := &searcher{prune: prune}
	v := s.negamax(b, depth, -inf, inf)
	return v, s.stats
}

// Level is a difficulty setting.
type Level struct {
	Name  string
	Depth int // fixed search depth; with Budget, the starting depth
	// Budget, if set, deepens the search one ply at a time until this many
	// nodes have been searched (counting all iterations) or MaxDepth.
	// Counting nodes instead of time keeps play identical on every machine.
	Budget int
	// Blunder is the chance of playing a random legal move instead.
	Blunder float64
}

const MaxDepth = 30

var (
	Random = Level{Name: "random"}
	Easy   = Level{Name: "easy", Depth: 1, Blunder: 0.35}
	Medium = Level{Name: "medium", Depth: 4}
	Hard   = Level{Name: "hard", Depth: 4, Budget: 300_000}
)

// Choose picks a move for the player to move. Ties go to the lowest pit so
// play is reproducible for a given random source.
func Choose(b game.Board, l Level, r *rand.Rand) (int, []MoveScore, Stats) {
	moves := b.Moves()
	if l.Depth <= 0 || r != nil && r.Float64() < l.Blunder {
		return moves[r.IntN(len(moves))], nil, Stats{}
	}
	sc, st := Analyse(b, l.Depth)
	st.Depth = l.Depth
	for d := l.Depth + 1; l.Budget > 0 && st.Nodes < l.Budget && d <= MaxDepth; d++ {
		more, s2 := Analyse(b, d)
		sc, st.Nodes, st.Depth = more, st.Nodes+s2.Nodes, d
	}
	best := sc[0]
	for _, m := range sc {
		if m.Score == best.Score && m.Pit < best.Pit {
			best = m
		}
	}
	return best.Pit, sc, st
}
