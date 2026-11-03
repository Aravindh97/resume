//go:build js && wasm

// Command web runs the Pallanguzhi rules and AI in the browser. You play the
// bottom row (South, pits 0-6); the AI plays the top row.
package main

import (
	"encoding/json"
	"math/rand/v2"
	"net/url"
	"sync"
	"time"

	"github.com/Aravindh97/pallanguzhi/internal/ai"
	"github.com/Aravindh97/pallanguzhi/internal/game"
)

var (
	mu    sync.Mutex
	b     game.Board
	level = ai.Medium
	rng   = rand.New(rand.NewPCG(uint64(time.Now().UnixNano()), 7))
)

var levels = map[string]ai.Level{"easy": ai.Easy, "medium": ai.Medium, "hard": ai.Hard}

func state() map[string]any {
	return map[string]any{"board": b, "over": b.Over(), "final": b.Final(), "moves": b.Moves()}
}

func main() {
	b = game.New()
	serve(map[string]handler{
		"POST new": func(_ url.Values, body []byte) resp {
			var in struct {
				Level    string
				YouFirst bool
			}
			json.Unmarshal(body, &in)
			l, found := levels[in.Level]
			if !found {
				return fail(400, "level must be easy, medium or hard")
			}
			mu.Lock()
			defer mu.Unlock()
			level, b = l, game.New()
			if !in.YouFirst {
				b.Turn = game.North
			}
			return ok(state())
		},
		"POST move": func(_ url.Values, body []byte) resp {
			var in struct{ Pit int }
			json.Unmarshal(body, &in)
			mu.Lock()
			defer mu.Unlock()
			if b.Turn != game.South {
				return fail(409, "it is not your turn")
			}
			var tr []game.Event
			if err := b.Play(in.Pit, &tr); err != nil {
				return fail(400, "you can only lift a non-empty pit on your row")
			}
			s := state()
			s["trace"] = tr
			return ok(s)
		},
		"POST ai": func(url.Values, []byte) resp {
			mu.Lock()
			defer mu.Unlock()
			if b.Turn != game.North || b.Over() {
				return fail(409, "it is not the AI's turn")
			}
			t := time.Now()
			m, scores, st := ai.Choose(b, level, rng)
			took := time.Since(t)
			var tr []game.Event
			b.Play(m, &tr)
			s := state()
			s["trace"], s["pit"], s["scores"], s["stats"], s["ms"], s["level"] = tr, m, scores, st, took.Milliseconds(), level.Name
			return ok(s)
		},
		"GET hint": func(url.Values, []byte) resp {
			mu.Lock()
			defer mu.Unlock()
			if b.Turn != game.South || b.Over() {
				return fail(409, "no move to hint")
			}
			sc, st := ai.Analyse(b, ai.Medium.Depth)
			return ok(map[string]any{"scores": sc, "stats": st})
		},
	})
}
