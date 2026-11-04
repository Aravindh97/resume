// Command loadtest plays many online games at once against a server and
// reports how long a move takes to come back to the player who made it.
//
//	go run ./cmd/loadtest -rooms 200            # starts its own server on loopback
//	go run ./cmd/loadtest -url http://127.0.0.1:8080 -rooms 200
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"math/rand/v2"
	"net"
	"net/http"
	"os"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"

	"github.com/Aravindh97/pallanguzhi/internal/room"
	"github.com/Aravindh97/pallanguzhi/internal/server"
)

func main() {
	url := flag.String("url", "", "server to test (default: start one on loopback)")
	rooms := flag.Int("rooms", 200, "games played at the same time")
	spectators := flag.Int("spectators", 2, "spectators per room")
	flag.Parse()

	if *url == "" {
		cfg := server.DefaultConfig()
		cfg.RoomsPerMinute, cfg.ConnsPerIP, cfg.MsgsPerSecond = 1<<20, 1<<20, 1000 // one IP is every client here
		s := server.New(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			panic(err)
		}
		go http.Serve(ln, s.Handler())
		*url = "http://" + ln.Addr().String()
	}

	var mu sync.Mutex
	var lat []time.Duration
	var moves, failed int
	var wg sync.WaitGroup
	start := time.Now()
	for i := 0; i < *rooms; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			l, err := playGame(*url, *spectators, uint64(i))
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				failed++
				fmt.Fprintln(os.Stderr, "room", i, err)
				return
			}
			lat = append(lat, l...)
			moves += len(l)
		}(i)
	}
	wg.Wait()
	took := time.Since(start)
	slices.Sort(lat)
	q := func(p float64) time.Duration {
		if len(lat) == 0 {
			return 0
		}
		return lat[min(len(lat)-1, int(p*float64(len(lat))))]
	}
	fmt.Printf("%d games at once (%d connections), %d moves in %s, %d games failed\n",
		*rooms, *rooms*(2+*spectators), moves, took.Round(time.Millisecond), failed)
	fmt.Printf("move round trip (send move -> receive new state): p50 %s  p90 %s  p99 %s  max %s\n",
		q(0.5).Round(time.Microsecond), q(0.9).Round(time.Microsecond), q(0.99).Round(time.Microsecond), q(1).Round(time.Microsecond))
}

type conn struct {
	c    *websocket.Conn
	last room.State
}

func dial(ctx context.Context, base, code, q string) (*conn, error) {
	c, _, err := websocket.Dial(ctx, strings.Replace(base, "http", "ws", 1)+"/api/rooms/"+code+"/ws?"+q, nil)
	if err != nil {
		return nil, err
	}
	c.SetReadLimit(1 << 20)
	return &conn{c: c}, nil
}

func (k *conn) next(ctx context.Context) (server.ServerMsg, error) {
	_, b, err := k.c.Read(ctx)
	var m server.ServerMsg
	if err == nil {
		err = json.Unmarshal(b, &m)
	}
	if m.State != nil {
		k.last = *m.State
	}
	return m, err
}

func (k *conn) until(ctx context.Context, ok func(room.State) bool) error {
	for {
		m, err := k.next(ctx)
		if err != nil {
			return err
		}
		if m.State != nil && ok(*m.State) {
			return nil
		}
	}
}

func playGame(base string, spectators int, seed uint64) ([]time.Duration, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	r, err := http.Post(base+"/api/rooms", "", nil)
	if err != nil {
		return nil, err
	}
	var created struct{ Code string }
	json.NewDecoder(r.Body).Decode(&created)
	r.Body.Close()
	var p [2]*conn
	for i := range p {
		if p[i], err = dial(ctx, base, created.Code, "play=1"); err != nil {
			return nil, err
		}
		defer p[i].c.CloseNow()
	}
	for i := 0; i < spectators; i++ {
		s, err := dial(ctx, base, created.Code, "")
		if err != nil {
			return nil, err
		}
		defer s.c.CloseNow()
		go func() { // spectators just read
			for {
				if _, err := s.next(ctx); err != nil {
					return
				}
			}
		}()
	}
	if err := p[0].until(ctx, func(s room.State) bool { return s.Players[1].Joined }); err != nil {
		return nil, err
	}
	// Keep player 1's view current in the background.
	p1err := make(chan error, 1)
	go func() {
		for {
			if _, err := p[1].next(ctx); err != nil {
				p1err <- err
				return
			}
		}
	}()
	rng := rand.New(rand.NewPCG(seed, 9))
	var lat []time.Duration
	st := p[0].last
	for !st.Over {
		mover := p[st.Board.Turn]
		moves := st.Board.Moves()
		msg, _ := json.Marshal(server.ClientMsg{Type: "move", Pit: moves[rng.IntN(len(moves))], Version: st.Version})
		v := st.Version
		t := time.Now()
		if err := mover.c.Write(ctx, websocket.MessageText, msg); err != nil {
			select {
			case e := <-p1err:
				return nil, fmt.Errorf("player 2's connection closed earlier: %v (close status %d)", e, websocket.CloseStatus(e))
			default:
			}
			return nil, err
		}
		// Player 0 reads its own stream; for player 1's moves, wait on player 0 too
		// (it sees every state), but only time moves made by player 0.
		if err := p[0].until(ctx, func(s room.State) bool { return s.Version > v }); err != nil {
			return nil, err
		}
		if mover == p[0] {
			lat = append(lat, time.Since(t))
		}
		st = p[0].last
	}
	return lat, nil
}
