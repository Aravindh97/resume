//go:build !race

package racesim

import (
	"runtime"
	"testing"
)

// The unsafe mode is a real data race. Under -race the detector would stop
// the test (correctly), so this test only runs in normal builds.
func TestUnsafeModeTearsAndLosesUpdates(t *testing.T) {
	defer runtime.GOMAXPROCS(runtime.GOMAXPROCS(1)) // the browser is single-threaded too
	r := NewRecord(DefaultFields)
	torn := 0
	for i := 0; i < 50; i++ {
		r.Step(Unsafe, 32, picker(uint64(i)))
		for _, f := range r.Snapshot().Fields {
			if f.Torn {
				torn++
			}
		}
	}
	if s := r.Snapshot(); s.Views >= 50*32 || torn == 0 {
		t.Fatalf("expected lost updates and torn writes: views=%d torn=%d", s.Views, torn)
	}
}
