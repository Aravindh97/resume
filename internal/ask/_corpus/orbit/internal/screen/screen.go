// Package screen finds close approaches between satellites.
//
// Brute force compares every pair at every time step: for 20,000 objects
// that is 200 million pairs per step. The screener avoids it in three
// stages:
//
//  1. Altitude shells: two objects whose perigee-apogee bands never overlap
//     (with margin) can never meet, and are never compared.
//  2. Spatial hash: at each time step every object is put in a grid cell of
//     side C. Only objects in the same or adjacent cells are compared.
//     C is chosen so that any pair that comes within D at any instant of the
//     step is within C of each other at the sample time.
//  3. Linear prefilter, then exact refinement: with positions and velocities
//     at the sample, the straight-line closest approach is computed; only if
//     it is within D (+ slack for curvature) inside the step is the pair
//     refined, with SGP4 and a golden-section search on the true distance.
//
// Time is split into slices processed in parallel; each worker owns its own
// copies of the satellites, because SGP4 keeps per-call state.
package screen

import (
	"math"
	"runtime"
	"sort"
	"sync"
	"time"

	"github.com/Aravindh97/orbit/internal/sgp4"
)

type Object struct {
	ID      int
	Name    string
	Sat     sgp4.Satellite
	Perigee float64 // km altitude, approximate (from mean elements)
	Apogee  float64
}

type Config struct {
	Start, End time.Time
	Step       time.Duration // sample interval
	Threshold  float64       // km: report approaches closer than this
	Workers    int
	// Primaries, if set, limits screening to pairs involving at least one
	// of these IDs ("my satellites against the catalogue").
	Primaries map[int]bool
}

type Event struct {
	A          int       `json:"a"` // NORAD IDs, A < B
	B          int       `json:"b"`
	TCA        time.Time `json:"tca"`
	MinRange   float64   `json:"min_range_km"`
	RelSpeed   float64   `json:"rel_speed_kms"`
	CoOrbiting bool      `json:"co_orbiting,omitempty"` // stayed within threshold for >10 min (docked, formation)
}

type Stats struct {
	Objects         int
	Steps           int
	Propagations    int64
	PairsChecked    int64 // pairs reaching the linear prefilter
	Refinements     int64
	BruteForcePairs float64 // what checking every pair at every step would cost
	PropErrors      int
	Took            time.Duration
}

const (
	earthRadius = 6378.135 // WGS72
	mu          = 398600.8 // WGS72
	maxRelSpeed = 16.0     // km/s, head-on LEO
	slack       = 1.0      // km, curvature error of the straight-line estimate within half a step
)

// Shell computes approximate perigee and apogee altitude from mean motion
// (rev/day) and eccentricity.
func Shell(meanMotion, ecc float64) (perigee, apogee float64) {
	n := meanMotion * 2 * math.Pi / 86400 // rad/s
	a := math.Cbrt(mu / (n * n))
	return a*(1-ecc) - earthRadius, a*(1+ecc) - earthRadius
}

type cellKey struct{ x, y, z int32 }

func Run(objs []Object, c Config) ([]Event, Stats) {
	t0 := time.Now()
	if c.Workers <= 0 {
		c.Workers = runtime.NumCPU()
	}
	steps := int(c.End.Sub(c.Start) / c.Step)
	st := Stats{Objects: len(objs), Steps: steps}
	st.BruteForcePairs = float64(len(objs)) * float64(len(objs)-1) / 2 * float64(steps)
	cell := c.Threshold + slack + maxRelSpeed*c.Step.Seconds()

	type slice struct{ from, to int }
	var slices []slice
	per := (steps + c.Workers*4 - 1) / (c.Workers * 4) // more slices than workers balances load
	for s := 0; s < steps; s += per {
		slices = append(slices, slice{s, min(steps, s+per)})
	}
	work := make(chan slice)
	var mu sync.Mutex
	var all []Event
	var wg sync.WaitGroup
	for w := 0; w < c.Workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			sats := make([]sgp4.Satellite, len(objs)) // private copies
			for i := range objs {
				sats[i] = objs[i].Sat
			}
			ws := worker{objs: objs, sats: sats, c: c, cell: cell, grid: map[cellKey][]int32{}}
			for sl := range work {
				evs := ws.run(sl.from, sl.to)
				mu.Lock()
				all = append(all, evs...)
				mu.Unlock()
			}
			mu.Lock()
			st.Propagations += ws.props
			st.PairsChecked += ws.pairs
			st.Refinements += ws.refines
			st.PropErrors += ws.propErr
			mu.Unlock()
		}()
	}
	for _, s := range slices {
		work <- s
	}
	close(work)
	wg.Wait()
	st.Took = time.Since(t0)
	return merge(all, c), st
}

type worker struct {
	objs                  []Object
	sats                  []sgp4.Satellite
	c                     Config
	cell                  float64
	grid                  map[cellKey][]int32
	pos, vel              [][3]float64
	ok                    []bool
	props, pairs, refines int64
	propErr               int
}

func (w *worker) at(i int, t time.Time) (r, v [3]float64, ok bool) {
	s := &w.sats[i]
	r, v, err := s.At(s.MinutesSince(t))
	w.props++
	return r, v, err == nil
}

func (w *worker) run(from, to int) []Event {
	n := len(w.objs)
	if w.pos == nil {
		w.pos, w.vel, w.ok = make([][3]float64, n), make([][3]float64, n), make([]bool, n)
	}
	var evs []Event
	for step := from; step < to; step++ {
		t := w.c.Start.Add(time.Duration(step) * w.c.Step)
		clear(w.grid)
		for i := 0; i < n; i++ {
			w.pos[i], w.vel[i], w.ok[i] = w.at(i, t)
			if !w.ok[i] {
				w.propErr++
				continue
			}
			k := cellKey{int32(math.Floor(w.pos[i][0] / w.cell)), int32(math.Floor(w.pos[i][1] / w.cell)), int32(math.Floor(w.pos[i][2] / w.cell))}
			w.grid[k] = append(w.grid[k], int32(i))
		}
		for k, members := range w.grid {
			for dx := int32(-1); dx <= 1; dx++ {
				for dy := int32(-1); dy <= 1; dy++ {
					for dz := int32(-1); dz <= 1; dz++ {
						other, ok := w.grid[cellKey{k.x + dx, k.y + dy, k.z + dz}]
						if !ok {
							continue
						}
						for _, i := range members {
							for _, j := range other {
								if j <= i {
									continue
								}
								if e, hit := w.check(int(i), int(j), t); hit {
									evs = append(evs, e)
								}
							}
						}
					}
				}
			}
		}
	}
	return evs
}

func (w *worker) check(i, j int, t time.Time) (Event, bool) {
	a, b := &w.objs[i], &w.objs[j]
	if p := w.c.Primaries; p != nil && !p[a.ID] && !p[b.ID] {
		return Event{}, false
	}
	m := w.c.Threshold + slack
	if a.Perigee-m > b.Apogee || b.Perigee-m > a.Apogee {
		return Event{}, false
	}
	w.pairs++
	var dr, dv [3]float64
	for k := 0; k < 3; k++ {
		dr[k] = w.pos[j][k] - w.pos[i][k]
		dv[k] = w.vel[j][k] - w.vel[i][k]
	}
	vv := dot(dv, dv)
	tau := 0.0
	if vv > 0 {
		tau = -dot(dr, dv) / vv
	}
	half := w.c.Step.Seconds() / 2
	tau = math.Max(-half, math.Min(half, tau))
	var d [3]float64
	for k := 0; k < 3; k++ {
		d[k] = dr[k] + dv[k]*tau
	}
	if math.Sqrt(dot(d, d)) > m {
		return Event{}, false
	}
	// Refine on the true SGP4 distance around the estimate.
	w.refines++
	center := t.Add(time.Duration(tau * float64(time.Second)))
	tca, dmin, rel := w.refine(i, j, center, w.c.Step)
	if dmin > w.c.Threshold {
		return Event{}, false
	}
	idA, idB := a.ID, b.ID
	if idA > idB {
		idA, idB = idB, idA
	}
	return Event{A: idA, B: idB, TCA: tca, MinRange: dmin, RelSpeed: rel}, true
}

func (w *worker) dist(i, j int, t time.Time) (float64, float64) {
	ri, vi, ok1 := w.at(i, t)
	rj, vj, ok2 := w.at(j, t)
	if !ok1 || !ok2 {
		return math.Inf(1), 0
	}
	var dr, dv [3]float64
	for k := 0; k < 3; k++ {
		dr[k], dv[k] = rj[k]-ri[k], vj[k]-vi[k]
	}
	return math.Sqrt(dot(dr, dr)), math.Sqrt(dot(dv, dv))
}

// refine minimises the true distance over [center-span, center+span] with a
// golden-section search to 1 ms.
func (w *worker) refine(i, j int, center time.Time, span time.Duration) (time.Time, float64, float64) {
	gr := (math.Sqrt(5) - 1) / 2
	lo, hi := -span.Seconds(), span.Seconds()
	f := func(s float64) float64 {
		d, _ := w.dist(i, j, center.Add(time.Duration(s*float64(time.Second))))
		return d
	}
	x1, x2 := hi-gr*(hi-lo), lo+gr*(hi-lo)
	f1, f2 := f(x1), f(x2)
	for hi-lo > 1e-3 {
		if f1 < f2 {
			hi, x2, f2 = x2, x1, f1
			x1 = hi - gr*(hi-lo)
			f1 = f(x1)
		} else {
			lo, x1, f1 = x1, x2, f2
			x2 = lo + gr*(hi-lo)
			f2 = f(x2)
		}
	}
	s := (lo + hi) / 2
	tca := center.Add(time.Duration(s * float64(time.Second)))
	d, rel := w.dist(i, j, tca)
	return tca.Round(time.Millisecond), d, rel
}

func dot(a, b [3]float64) float64 { return a[0]*b[0] + a[1]*b[1] + a[2]*b[2] }

// merge collapses detections of the same encounter (found from several
// steps or slices) into one event: the closest. Detections of a pair more
// than 10 minutes apart are separate encounters, except when the pair stays
// close the whole time (docked or formation flying), which is reported once
// and marked co-orbiting.
func merge(evs []Event, c Config) []Event {
	sort.Slice(evs, func(i, j int) bool {
		if evs[i].A != evs[j].A {
			return evs[i].A < evs[j].A
		}
		if evs[i].B != evs[j].B {
			return evs[i].B < evs[j].B
		}
		return evs[i].TCA.Before(evs[j].TCA)
	})
	var out []Event
	for i := 0; i < len(evs); {
		j := i
		best := evs[i]
		first, last := evs[i].TCA, evs[i].TCA
		for j+1 < len(evs) && evs[j+1].A == evs[i].A && evs[j+1].B == evs[i].B && evs[j+1].TCA.Sub(evs[j].TCA) < 10*time.Minute {
			j++
			last = evs[j].TCA
			if evs[j].MinRange < best.MinRange {
				best = evs[j]
			}
		}
		if last.Sub(first) > 10*time.Minute {
			best.CoOrbiting = true
		}
		out = append(out, best)
		i = j + 1
	}
	sort.Slice(out, func(i, j int) bool { return out[i].MinRange < out[j].MinRange })
	return out
}

// ClosestNear returns the minimum distance between two objects within
// ±span of t (golden-section on SGP4 distance), and when it occurs.
func ClosestNear(a, b Object, t time.Time, span time.Duration) (time.Time, float64) {
	w := &worker{objs: []Object{a, b}, sats: []sgp4.Satellite{a.Sat, b.Sat}}
	// coarse scan first so the search starts in the right basin
	best, bestT := math.Inf(1), t
	for s := -span; s <= span; s += 5 * time.Second {
		if d, _ := w.dist(0, 1, t.Add(s)); d < best {
			best, bestT = d, t.Add(s)
		}
	}
	tca, d, _ := w.refine(0, 1, bestT, 5*time.Second)
	return tca, d
}
