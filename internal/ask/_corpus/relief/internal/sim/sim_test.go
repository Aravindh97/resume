package sim

import "testing"

func TestRandomPartitions(t *testing.T) {
	runs := 2000
	if testing.Short() {
		runs = 200
	}
	lost := 0
	for i := 0; i < runs; i++ {
		r := Run(Config{Devices: 3 + i%6, Steps: 100 + i%400, Seed: uint64(i)})
		if !r.Converged || len(r.Violations) > 0 {
			t.Fatalf("seed %d: converged=%v violations=%v", i, r.Converged, r.Violations)
		}
		if r.BaselineLost > 0 {
			lost++
		}
	}
	if lost == 0 {
		t.Fatal("baseline never lost data; the simulation is not exercising concurrency")
	}
}
