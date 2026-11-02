package screen

import (
	"math"
	"os"
	"sort"
	"testing"
	"time"

	"github.com/Aravindh97/orbit/internal/sgp4"
)

func load(t *testing.T) []Object {
	f, err := os.Open("../../testdata/debris.tle")
	if err != nil {
		t.Skip("testdata/debris.tle missing")
	}
	defer f.Close()
	objs, _, err := LoadTLE(f)
	if err != nil {
		t.Fatal(err)
	}
	return objs
}

func brute(objs []Object, c Config) map[[2]int]float64 { return Brute(objs, c) }

func TestScreenMatchesBruteForce(t *testing.T) {
	objs := load(t)
	// The Fengyun-1C and Cosmos 2251 debris clouds cross each other often.
	n, hours := 600, 6
	if testing.Short() {
		n, hours = 150, 1
	}
	sample := objs[:n]
	start := sample[0].Sat.Epoch().Truncate(time.Hour)
	c := Config{Start: start, End: start.Add(time.Duration(hours) * time.Hour), Step: 10 * time.Second, Threshold: 20, Workers: 4}
	evs, st := Run(sample, c)
	ref := brute(sample, c)
	got := map[[2]int]float64{}
	for _, e := range evs {
		k := [2]int{e.A, e.B}
		if v, ok := got[k]; !ok || e.MinRange < v {
			got[k] = e.MinRange
		}
	}
	if len(ref) == 0 {
		t.Fatal("reference found no approaches; widen the test")
	}
	missed := 0
	for k, d := range ref {
		g, ok := got[k]
		if !ok {
			missed++
			t.Errorf("missed pair %v (%.3f km)", k, d)
			continue
		}
		if math.Abs(g-d) > 0.01 {
			t.Errorf("pair %v: %.4f km vs reference %.4f km", k, g, d)
		}
	}
	t.Logf("%d pairs, %d found, %d missed; screener checked %.4f%% of brute-force pairs", len(ref), len(got), missed,
		100*float64(st.PairsChecked)/st.BruteForcePairs)
}

func TestRefineFindsKnownMinimum(t *testing.T) {
	objs := load(t)
	start := objs[0].Sat.Epoch()
	n := 1500
	c := Config{Start: start, End: start.Add(24 * time.Hour), Step: 20 * time.Second, Threshold: 30}
	if testing.Short() {
		n, c.End = 500, start.Add(6*time.Hour)
	}
	evs, _ := Run(objs[:n], c)
	if len(evs) == 0 {
		t.Skip("no approaches in sample")
	}
	sort.Slice(evs, func(i, j int) bool { return evs[i].MinRange < evs[j].MinRange })
	e := evs[0]
	// Distance 1 s either side of TCA must not be smaller.
	idx := map[int]int{}
	for i, o := range objs[:n] {
		idx[o.ID] = i
	}
	w := &worker{objs: objs[:n], c: c}
	w.sats = make([]sgp4.Satellite, n)
	for i := range w.sats {
		w.sats[i] = objs[i].Sat
	}
	for _, dt := range []time.Duration{-time.Second, time.Second} {
		d, _ := w.dist(idx[e.A], idx[e.B], e.TCA.Add(dt))
		if d < e.MinRange {
			t.Fatalf("not a minimum: %.4f km at %v vs %.4f at TCA", d, dt, e.MinRange)
		}
	}
}

func TestShell(t *testing.T) {
	// ISS: ~15.5 rev/day, nearly circular, ~420 km.
	p, a := Shell(15.5, 0.0005)
	if p < 380 || a > 450 {
		t.Fatalf("ISS shell %.0f-%.0f km", p, a)
	}
}
