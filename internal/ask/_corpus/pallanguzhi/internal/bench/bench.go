// Package bench plays AI levels against each other in parallel and reports
// win rates by seat. Every game has its own seed, so a run is reproducible
// regardless of how goroutines are scheduled.
package bench

import (
	"fmt"
	"io"
	"math/rand/v2"
	"runtime"
	"sync"

	"github.com/Aravindh97/pallanguzhi/internal/ai"
	"github.com/Aravindh97/pallanguzhi/internal/game"
)

// Result is A's record against B.
type Result struct {
	A, B       string
	Seat       int // the seat A played: 0 = South (moves first), 1 = North
	Games      int
	Wins, Draw int
	Margin     float64 // A's average final margin in seeds
}

// Opening is how many random plies start a game between two levels that
// always pick the same move (medium, hard); without it they would replay one
// game. Games involving a random or easy player start from the real start
// position, so the first mover's opening move is never random.
const Opening = 2

func deterministic(l ai.Level) bool { return l.Depth > 0 && l.Blunder == 0 }

// Play runs n games of a (in seat) against b.
func Play(a, b ai.Level, seat, n int, seed uint64) Result {
	res := Result{A: a.Name, B: b.Name, Seat: seat, Games: n}
	type out struct{ margin int }
	ch := make(chan out)
	jobs := make(chan int)
	var wg sync.WaitGroup
	for w := 0; w < runtime.GOMAXPROCS(0); w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for g := range jobs {
				r := rand.New(rand.NewPCG(seed, uint64(g)))
				bd := game.New()
				for k := 0; deterministic(a) && deterministic(b) && k < Opening && !bd.Over(); k++ {
					m := bd.Moves()
					bd.Play(m[r.IntN(len(m))], nil)
				}
				lv := [2]ai.Level{}
				lv[seat], lv[1-seat] = a, b
				for !bd.Over() {
					m, _, _ := ai.Choose(bd, lv[bd.Turn], r)
					bd.Play(m, nil)
				}
				f := bd.Final()
				ch <- out{f[seat] - f[1-seat]}
			}
		}()
	}
	go func() {
		for g := 0; g < n; g++ {
			jobs <- g
		}
		close(jobs)
		wg.Wait()
		close(ch)
	}()
	total := 0
	for o := range ch {
		total += o.margin
		switch {
		case o.margin > 0:
			res.Wins++
		case o.margin == 0:
			res.Draw++
		}
	}
	res.Margin = float64(total) / float64(n)
	return res
}

func Print(w io.Writer, rs []Result) {
	fmt.Fprintf(w, "%-8s %-8s %-15s %6s %8s %7s %8s\n", "A", "B", "A plays", "games", "A wins", "draws", "margin")
	for _, r := range rs {
		seat := "South (first)"
		if r.Seat == 1 {
			seat = "North (second)"
		}
		fmt.Fprintf(w, "%-8s %-8s %-15s %6d %7.1f%% %7d %+8.1f\n", r.A, r.B, seat, r.Games, 100*float64(r.Wins)/float64(r.Games), r.Draw, r.Margin)
	}
}
