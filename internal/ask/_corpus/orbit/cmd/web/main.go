//go:build js && wasm

// Command web screens a chosen satellite against the embedded catalogue
// snapshot, in the browser.
package main

import (
	"bytes"
	_ "embed"
	"encoding/json"
	"math"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/Aravindh97/orbit/internal/screen"
	"github.com/Aravindh97/orbit/internal/sgp4"
)

//go:embed embed/catalog.tle
var catalog []byte

//go:embed embed/FETCHED_AT
var fetchedAt string

func llaAt(o screen.Object, t time.Time) (lat, lon, alt float64, ok bool) {
	s := o.Sat
	r, _, err := s.At(s.MinutesSince(t))
	if err != nil {
		return 0, 0, 0, false
	}
	lat, lon, alt = sgp4.Geodetic(r, t)
	return lat, lon, alt, true
}

func main() {
	objs, skipped, err := screen.LoadTLE(bytes.NewReader(catalog))
	byID := map[int]screen.Object{}
	for _, o := range objs {
		byID[o.ID] = o
	}
	start, _ := time.Parse(time.RFC3339, strings.TrimSpace(fetchedAt))
	start = start.Truncate(time.Hour).Add(time.Hour)

	serve(map[string]handler{
		"GET info": func(url.Values, []byte) resp {
			if err != nil {
				return fail(500, err.Error())
			}
			return ok(map[string]any{"objects": len(objs), "skipped": skipped, "fetched_at": strings.TrimSpace(fetchedAt), "default_start": start})
		},
		"GET search": func(q url.Values, _ []byte) resp {
			term := strings.ToUpper(strings.TrimSpace(q.Get("q")))
			var out []map[string]any
			for _, o := range objs {
				if len(out) == 20 {
					break
				}
				if term != "" && (strings.Contains(strings.ToUpper(o.Name), term) || strconv.Itoa(o.ID) == term) {
					out = append(out, map[string]any{"id": o.ID, "name": o.Name, "perigee": math.Round(o.Perigee), "apogee": math.Round(o.Apogee)})
				}
			}
			return ok(out)
		},
		"POST screen": func(_ url.Values, b []byte) resp {
			var in struct {
				ID        int
				Hours     float64
				Threshold float64
				StepS     int
			}
			if json.Unmarshal(b, &in) != nil {
				return fail(400, "bad request")
			}
			p, found := byID[in.ID]
			if !found {
				return fail(404, "no object with that NORAD id in the snapshot")
			}
			in.Hours = math.Min(math.Max(in.Hours, 0.5), 24)
			in.Threshold = math.Min(math.Max(in.Threshold, 1), 50)
			if in.StepS < 5 || in.StepS > 60 {
				in.StepS = 20
			}
			c := screen.Config{Start: start, End: start.Add(time.Duration(in.Hours * float64(time.Hour))), Step: time.Duration(in.StepS) * time.Second,
				Threshold: in.Threshold, Workers: 1, Primaries: map[int]bool{in.ID: true}}
			evs, st := screen.Run(objs, c)
			sort.Slice(evs, func(i, j int) bool { return evs[i].TCA.Before(evs[j].TCA) })
			var rows []map[string]any
			for _, e := range evs {
				other := e.B
				if other == in.ID {
					other = e.A
				}
				lat, lon, alt, _ := llaAt(p, e.TCA)
				rows = append(rows, map[string]any{"tca": e.TCA, "other": other, "name": byID[other].Name, "range_km": e.MinRange,
					"rel_speed": e.RelSpeed, "co_orbiting": e.CoOrbiting, "lat": lat, "lon": lon, "alt": alt})
			}
			var track [][2]float64
			for t := c.Start; !t.After(c.End); t = t.Add(time.Minute) {
				if lat, lon, _, ok := llaAt(p, t); ok {
					track = append(track, [2]float64{lat, lon})
				}
			}
			return ok(map[string]any{"primary": map[string]any{"id": p.ID, "name": p.Name, "perigee": p.Perigee, "apogee": p.Apogee, "epoch": p.Sat.Epoch()},
				"start": c.Start, "end": c.End, "events": rows, "track": track,
				"stats": map[string]any{"objects_in_band": st.Objects, "steps": st.Steps, "propagations": st.Propagations,
					"pairs_checked": st.PairsChecked, "refined": st.Refinements, "took_ms": st.Took.Milliseconds()}})
		},
		"POST verify": func(q url.Values, _ []byte) resp {
			// Run and Brute on the same small sample: every pair Brute finds
			// must be found by Run, at the same distance.
			n, _ := strconv.Atoi(q.Get("n"))
			n = min(max(n, 20), 250)
			// Starlink's shells are dense enough that a small sample has real
			// close approaches within an hour.
			var sample []screen.Object
			for _, o := range objs {
				if strings.HasPrefix(o.Name, "STARLINK") && len(sample) < n {
					sample = append(sample, o)
				}
			}
			c := screen.Config{Start: start, End: start.Add(time.Hour), Step: 10 * time.Second, Threshold: 25, Workers: 1}
			t0 := time.Now()
			evs, st := screen.Run(sample, c)
			fast := time.Since(t0)
			t1 := time.Now()
			ref := screen.Brute(sample, c)
			slow := time.Since(t1)
			got := map[[2]int]float64{}
			for _, e := range evs {
				k := [2]int{e.A, e.B}
				if v, ok := got[k]; !ok || e.MinRange < v {
					got[k] = e.MinRange
				}
			}
			found, maxDiff := 0, 0.0
			for k, d := range ref {
				if g, ok := got[k]; ok {
					found++
					maxDiff = math.Max(maxDiff, math.Abs(g-d))
				}
			}
			return ok(map[string]any{"objects": n, "reference_pairs": len(ref), "found": found, "max_diff_m": maxDiff * 1000,
				"screen_ms": fast.Milliseconds(), "brute_ms": slow.Milliseconds(), "pairs_checked_pct": 100 * float64(st.PairsChecked) / st.BruteForcePairs})
		},
	})
}
