// Package game implements the rules of Pallanguzhi, the Tamil mancala game.
//
// The board has 14 pits in two rows of 7. Pits 0-6 belong to South (the
// human in the web demo), left to right; pits 7-13 belong to North, right to
// left as seen by South. Sowing goes counter-clockwise, which is increasing
// pit index, wrapping from 13 back to 0.
//
// Rules (following the Wikipedia description; variants differ by region):
//   - Every pit starts with 12 seeds except the middle pit of each row, which
//     starts with 2: 148 seeds in all.
//   - A move lifts every seed from one of your non-empty pits and sows them
//     one per pit, starting with the next pit.
//   - Pasu: if a dropped seed makes a pit hold exactly 6, the sower captures
//     those 6 at once.
//   - After the last seed, look at the next pit. If it has seeds, lift them
//     and keep sowing from the pit after it.
//   - If it is empty, the sower captures the seeds in the pit after the
//     empty one, and the turn ends. If both are empty, the turn ends.
//   - When the player to move has no seeds on their side, the round ends and
//     each player keeps the seeds left on their own side. Most seeds wins.
//
// The traditional game continues over several rounds with "rubbish" pits
// for the loser; this package plays a single round.
package game

import (
	"errors"
	"fmt"
)

const (
	Pits    = 14
	PerSide = 7
	South   = 0
	North   = 1
	// MaxLifts bounds the relay chain of one turn. Random self-play has never
	// reached it (see the tests); it exists so a move can never loop forever.
	MaxLifts = 2000
)

type Board struct {
	Pits  [Pits]int `json:"pits"`
	Store [2]int    `json:"store"`
	Turn  int       `json:"turn"`
}

// New returns the starting position with South to move.
func New() Board {
	var b Board
	for i := range b.Pits {
		b.Pits[i] = 12
	}
	b.Pits[3], b.Pits[10] = 2, 2
	return b
}

// Owner returns which player a pit belongs to.
func Owner(pit int) int { return pit / PerSide }

// Opposite returns the pit facing this one across the board.
func Opposite(pit int) int { return Pits - 1 - pit }

// Total counts every seed on the board and in the stores; it is always 148.
func (b Board) Total() int {
	t := b.Store[0] + b.Store[1]
	for _, n := range b.Pits {
		t += n
	}
	return t
}

// Moves returns the pits the player to move may lift, in pit order.
func (b Board) Moves() []int {
	var out []int
	for i := b.Turn * PerSide; i < (b.Turn+1)*PerSide; i++ {
		if b.Pits[i] > 0 {
			out = append(out, i)
		}
	}
	return out
}

// Over reports whether the round has ended: the player to move has no seeds.
func (b Board) Over() bool { return len(b.Moves()) == 0 }

// Final returns the scores after each side keeps the seeds on its side.
func (b Board) Final() [2]int {
	s := b.Store
	for i, n := range b.Pits {
		s[Owner(i)] += n
	}
	return s
}

// Event is one step of a move, for animation and explanation.
type Event struct {
	Kind  string    `json:"kind"` // lift | drop | pasu | capture | end | limit
	Pit   int       `json:"pit"`
	Seeds int       `json:"seeds"` // seeds lifted or captured
	Pits  [Pits]int `json:"pits"`  // board after this event
	Store [2]int    `json:"store"` // stores after this event
}

var ErrIllegal = errors.New("illegal move")

// Play makes a move for the player to move. If trace is non-nil, every step
// is appended to it.
func (b *Board) Play(pit int, trace *[]Event) error {
	if pit < 0 || pit >= Pits || Owner(pit) != b.Turn || b.Pits[pit] == 0 {
		return fmt.Errorf("%w: pit %d", ErrIllegal, pit)
	}
	p := b.Turn
	emit := func(kind string, at, seeds int) {
		if trace != nil {
			*trace = append(*trace, Event{kind, at, seeds, b.Pits, b.Store})
		}
	}
	n := b.Pits[pit]
	b.Pits[pit] = 0
	emit("lift", pit, n)
	i := pit
	for lifts := 1; ; lifts++ {
		for ; n > 0; n-- {
			i = (i + 1) % Pits
			b.Pits[i]++
			emit("drop", i, 1)
			if b.Pits[i] == 6 {
				b.Store[p] += 6
				b.Pits[i] = 0
				emit("pasu", i, 6)
			}
		}
		q := (i + 1) % Pits
		if b.Pits[q] > 0 {
			if lifts >= MaxLifts {
				emit("limit", q, 0)
				break
			}
			n, b.Pits[q], i = b.Pits[q], 0, q
			emit("lift", q, n)
			continue
		}
		r := (q + 1) % Pits
		if c := b.Pits[r]; c > 0 {
			b.Store[p] += c
			b.Pits[r] = 0
			emit("capture", r, c)
		}
		break
	}
	b.Turn = 1 - p
	emit("end", -1, 0)
	return nil
}

// After returns the board after a move, without changing b.
func (b Board) After(pit int) (Board, error) {
	err := b.Play(pit, nil)
	return b, err
}
