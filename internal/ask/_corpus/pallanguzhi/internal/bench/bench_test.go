package bench

import (
	"testing"

	"github.com/Aravindh97/pallanguzhi/internal/ai"
)

// Results depend only on the seed, not on goroutine scheduling.
func TestReproducible(t *testing.T) {
	a := Play(ai.Easy, ai.Random, 1, 60, 42)
	b := Play(ai.Easy, ai.Random, 1, 60, 42)
	if a != b {
		t.Fatalf("%+v != %+v", a, b)
	}
	if a.Wins+a.Draw > a.Games || a.Games != 60 {
		t.Fatalf("bad counts %+v", a)
	}
}

// Searching beats random from either seat on average margin.
func TestMediumBeatsRandom(t *testing.T) {
	for seat := 0; seat < 2; seat++ {
		if r := Play(ai.Medium, ai.Random, seat, 60, 7); r.Margin <= 0 {
			t.Fatalf("seat %d: %+v", seat, r)
		}
	}
}
