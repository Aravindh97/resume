package room

import (
	"errors"
	"math/rand/v2"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Aravindh97/pallanguzhi/internal/game"
)

func drain(s *Sub) State {
	st, _ := s.Take()
	return st
}

func twoPlayers(t *testing.T) (*Hub, string, *Sub, *Sub) {
	t.Helper()
	h := NewHub()
	code, err := h.Create()
	if err != nil {
		t.Fatal(err)
	}
	a, err := h.Join(code, "Anita", "", true)
	if err != nil {
		t.Fatal(err)
	}
	b, err := h.Join(code, "Ravi", "", true)
	if err != nil {
		t.Fatal(err)
	}
	return h, code, a, b
}

func TestSeatsAndTurns(t *testing.T) {
	h, code, a, b := twoPlayers(t)
	if a.Seat != 0 || b.Seat != 1 || a.Token == "" || a.Token == b.Token {
		t.Fatalf("seats %d %d", a.Seat, b.Seat)
	}
	c, _ := h.Join(code, "Meena", "", true)
	if c.Seat != Spectator {
		t.Fatal("third player got a seat")
	}
	st := drain(a)
	if !st.Players[0].Online || !st.Players[1].Online || st.Spectators != 1 || st.Players[0].Name != "Anita" {
		t.Fatalf("state %+v", st)
	}
	if err := b.Move(st.Moves[0], st.Version); !errors.Is(err, ErrNotTurn) {
		t.Fatalf("north moved on south's turn: %v", err)
	}
	if err := c.Move(st.Moves[0], st.Version); !errors.Is(err, ErrNotPlayer) {
		t.Fatalf("spectator moved: %v", err)
	}
	if err := a.Move(7, st.Version); err == nil {
		t.Fatal("south lifted a north pit")
	}
	if err := a.Move(0, st.Version); err != nil {
		t.Fatal(err)
	}
	got := drain(b)
	if got.LastMove == nil || got.LastMove.Pit != 0 || got.Board.Turn != 1 || got.Version != st.Version+1 {
		t.Fatalf("opponent's update: %+v", got.LastMove)
	}
	if got.Board.Total() != 148 {
		t.Fatal("seeds not conserved")
	}
}

// A double-click sends the same move twice against the same version: the
// second must be refused, not played as a second move.
func TestDoubleSubmitIsRejected(t *testing.T) {
	_, _, a, _ := twoPlayers(t)
	st := drain(a)
	if err := a.Move(0, st.Version); err != nil {
		t.Fatal(err)
	}
	if err := a.Move(0, st.Version); !errors.Is(err, ErrStale) {
		t.Fatalf("second identical move: %v", err)
	}
}

func TestReconnectKeepsSeat(t *testing.T) {
	h, code, a, _ := twoPlayers(t)
	tok := a.Token
	a.Leave(time.Now())
	st, _ := h.Peek(code)
	if st.Players[0].Online || !st.Players[0].Joined {
		t.Fatalf("after leaving: %+v", st.Players[0])
	}
	back, err := h.Join(code, "", tok, true)
	if err != nil || back.Seat != 0 {
		t.Fatalf("reconnect: seat %d err %v", back.Seat, err)
	}
	if st := drain(back); !st.Players[0].Online || st.Players[0].Name != "Anita" {
		t.Fatalf("seat not restored: %+v", st.Players[0])
	}
	// A wrong token never takes someone's seat.
	if x, _ := h.Join(code, "Eve", "WRONGTOKEN", true); x.Seat != Spectator {
		t.Fatal("guessed token took a seat")
	}
}

func TestRematchSwapsFirstPlayer(t *testing.T) {
	h, code, a, b := twoPlayers(t)
	r, _ := h.get(code)
	r.mu.Lock()
	r.board.Pits = [14]int{} // end the game
	r.board.Pits[7] = 148
	r.version++
	r.mu.Unlock()
	if err := a.Rematch(); err != nil {
		t.Fatal(err)
	}
	if st := drain(b); !st.Rematch[0] || st.Game != 1 {
		t.Fatalf("one vote: %+v", st)
	}
	b.Rematch()
	st := drain(a)
	if st.Game != 2 || st.Board.Turn != 1 || st.Over || st.Board.Total() != 148 {
		t.Fatalf("rematch: game %d turn %d", st.Game, st.Board.Turn)
	}
}

// A client that reads nothing while 50 states are broadcast is not
// disconnected and holds one state, not 50: it gets the newest.
func TestSlowClientGetsTheLatestState(t *testing.T) {
	_, _, a, b := twoPlayers(t)
	r := a.room
	for i := 0; i < 50; i++ {
		r.mu.Lock()
		r.version++
		r.broadcastLocked(false)
		r.mu.Unlock()
	}
	if b.gone || len(b.Notify) != 1 {
		t.Fatalf("slow client: gone=%v notifications=%d", b.gone, len(b.Notify))
	}
	st, ok := b.Take()
	r.mu.Lock()
	want := r.version
	r.mu.Unlock()
	if !ok || st.Version != want {
		t.Fatalf("got version %d, want latest %d", st.Version, want)
	}
	if _, again := b.Take(); again {
		t.Fatal("same state delivered twice")
	}
}

func TestSweepRemovesIdleRooms(t *testing.T) {
	h, code, a, b := twoPlayers(t)
	now := time.Now()
	h.now = func() time.Time { return now }
	a.Leave(now)
	b.Leave(now)
	h.now = func() time.Time { return now.Add(IdleTTL - time.Minute) }
	if h.Sweep() != 0 {
		t.Fatal("removed too early")
	}
	h.now = func() time.Time { return now.Add(IdleTTL + time.Minute) }
	if h.Sweep() != 1 {
		t.Fatal("idle room kept")
	}
	if _, err := h.Join(code, "x", "", true); !errors.Is(err, ErrNoRoom) {
		t.Fatal("expired room still joinable")
	}
}

func TestRoomLimit(t *testing.T) {
	h := NewHub()
	h.MaxRooms = 3
	for i := 0; i < 3; i++ {
		if _, err := h.Create(); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := h.Create(); !errors.Is(err, ErrTooMany) {
		t.Fatal("room limit not enforced")
	}
}

// Many clients hammer one room at once: two players sending moves (including
// duplicates and stale ones), spectators joining and leaving, players
// reconnecting. Every applied move must be legal and in turn, and the board
// must always hold 148 seeds. Run with -race.
func TestConcurrentStress(t *testing.T) {
	h, code, a, b := twoPlayers(t)
	players := []*Sub{a, b}
	var applied, refused atomic.Int64
	var wg sync.WaitGroup
	stop := make(chan struct{})
	for p := 0; p < 2; p++ {
		for w := 0; w < 4; w++ { // 4 tabs per player
			wg.Add(1)
			go func(p, w int) {
				defer wg.Done()
				r := rand.New(rand.NewPCG(uint64(p), uint64(w)))
				for {
					select {
					case <-stop:
						return
					default:
					}
					st, _ := h.Peek(code)
					if st.Over {
						return
					}
					pit := r.IntN(14)
					if len(st.Moves) > 0 && r.IntN(3) > 0 {
						pit = st.Moves[r.IntN(len(st.Moves))]
					}
					if err := players[p].Move(pit, st.Version); err == nil {
						applied.Add(1)
					} else {
						refused.Add(1)
					}
				}
			}(p, w)
		}
	}
	for w := 0; w < 8; w++ { // spectators coming and going
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 200; i++ {
				s, err := h.Join(code, "", "", false)
				if err == nil {
					drain(s)
					s.Leave(time.Now())
					if _, open := <-s.Notify; open {
						// a notification may still be buffered; the channel must then be closed
						if _, open = <-s.Notify; open {
							t.Error("Notify not closed after Leave")
						}
					}
				}
			}
		}()
	}
	go func() {
		time.Sleep(3 * time.Second)
		close(stop)
	}()
	wg.Wait()
	st, _ := h.Peek(code)
	if st.Board.Total() != 148 {
		t.Fatalf("seeds: %d", st.Board.Total())
	}
	// Replay the recorded moves on a fresh board: each must be legal and made
	// by the player whose turn it was, and the result must equal the room's board.
	r, _ := h.get(code)
	r.mu.Lock()
	hist := append([][2]int(nil), r.history...)
	r.mu.Unlock()
	if int64(len(hist)) != applied.Load() {
		t.Fatalf("history %d, applied %d", len(hist), applied.Load())
	}
	replay := game.New()
	for i, m := range hist {
		if m[0] != replay.Turn {
			t.Fatalf("move %d by seat %d on seat %d's turn", i, m[0], replay.Turn)
		}
		if err := replay.Play(m[1], nil); err != nil {
			t.Fatalf("move %d illegal on replay: %v", i, err)
		}
	}
	if replay != st.Board {
		t.Fatal("replayed board differs from the room's board")
	}
	t.Logf("applied %d moves, refused %d (stale, wrong turn or illegal); game over: %v", applied.Load(), refused.Load(), st.Over)
}
