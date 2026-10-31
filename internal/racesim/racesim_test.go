package racesim

import (
	"math/rand/v2"
	"testing"
)

func picker(seed uint64) func(int) (int, int) {
	r := rand.New(rand.NewPCG(seed, 1))
	return func(int) (int, int) { return r.IntN(len(DefaultFields)), r.IntN(2) }
}

func TestSafeModesNeverTearOrLoseUpdates(t *testing.T) {
	for _, m := range []Mode{Mutex, Channel} {
		r := NewRecord(DefaultFields)
		for i := 0; i < 50; i++ {
			r.Step(m, 32, picker(uint64(i)))
		}
		s := r.Snapshot()
		if s.Views != 50*32 {
			t.Errorf("%s: views = %d, want %d", m, s.Views, 50*32)
		}
		for _, f := range s.Fields {
			if f.Torn {
				t.Errorf("%s: torn field %s = %q", m, f.Name, f.Value)
			}
		}
	}
}
