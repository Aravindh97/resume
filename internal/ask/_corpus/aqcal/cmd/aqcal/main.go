// Command aqcal calibrates low-cost air sensors against reference stations
// and runs the streaming fault detector.
//
//	aqcal eval            # calibration study + fault-injection test + throughput
//	aqcal replay -speed 3600 -addr 127.0.0.1:8090   # replay October as a live stream with an API
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"math"
	"math/rand/v2"
	"net"
	"net/http"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/Aravindh97/aqcal/internal/calib"
	"github.com/Aravindh97/aqcal/internal/data"
	"github.com/Aravindh97/aqcal/internal/stream"
)

type dataset struct {
	readings []data.Reading
	ref      map[string]map[time.Time]data.Ref
	rh       map[time.Time]float64
	sensors  []data.Sensor
	bad      int
}

func load(dir string) (*dataset, error) {
	d := &dataset{}
	var err error
	if d.readings, d.bad, err = data.LoadReadings(dir + "/lowcost-2026-10.csv.gz"); err != nil {
		return nil, err
	}
	if d.ref, err = data.LoadReference(dir + "/reference-uba-2026-10.csv"); err != nil {
		return nil, err
	}
	if d.rh, err = data.LoadHumidity(dir + "/humidity-berlin-2026-10.json"); err != nil {
		return nil, err
	}
	if d.sensors, err = data.LoadSensors(dir + "/sensors.json"); err != nil {
		return nil, err
	}
	return d, nil
}

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: aqcal eval|replay")
		os.Exit(2)
	}
	var err error
	switch os.Args[1] {
	case "eval":
		err = eval(os.Args[2:])
	case "replay":
		err = replay(os.Args[2:])
	default:
		err = fmt.Errorf("unknown command %q", os.Args[1])
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

var split = time.Date(2026, 10, 19, 0, 0, 0, 0, time.UTC) // train: 1-18 Oct, test: 19-28 Oct

func runStream(c stream.Config, rs []data.Reading) (*stream.Sink, time.Duration, error) {
	ch := make(chan data.Reading, 4096)
	sink := &stream.Sink{}
	t := time.Now()
	errc := make(chan error, 1)
	go func() { errc <- stream.Run(context.Background(), c, ch, sink) }()
	for _, r := range rs {
		ch <- r
	}
	close(ch)
	err := <-errc
	return sink, time.Since(t), err
}

func between(rs []data.Reading, from, to time.Time) []data.Reading {
	var out []data.Reading
	for _, r := range rs {
		if !r.T.Before(from) && r.T.Before(to) {
			out = append(out, r)
		}
	}
	return out
}

func eval(args []string) error {
	fs := flag.NewFlagSet("eval", flag.ExitOnError)
	dir := fs.String("data", "testdata", "data directory")
	seed := fs.Uint64("seed", 1, "fault injection seed")
	seeds := fs.Int("seeds", 1, "run the fault-injection test with this many seeds (seed, seed+1, ...) and report totals")
	fs.Parse(args)
	d, err := load(*dir)
	if err != nil {
		return err
	}
	start, end := d.readings[0].T.Truncate(24*time.Hour), d.readings[len(d.readings)-1].T.Add(time.Hour).Truncate(time.Hour)
	fmt.Printf("data: %d readings from %d SDS011 sensors (%d unparseable rows), %s .. %s; reference: %d UBA stations; train before %s\n\n",
		len(d.readings), len(d.sensors), d.bad, start.Format("2006-01-02"), end.Format("2006-01-02"), len(d.ref), split.Format("2006-01-02"))

	// 1. Time alignment: find the shift between sensor UTC hours and the
	//    reference file's hours by correlation.
	hourly := calib.Aggregate(d.readings)
	fmt.Println("1. time alignment: median over sensors of the correlation between hourly sensor medians and the reference, by shift:")
	for _, off := range []int{-2, -1, 0, 1, 2} {
		var rs []float64
		for _, s := range d.sensors {
			var x, y []float64
			for h, v := range hourly[s.ID] {
				if r, ok := d.ref[s.Ref][h.Add(time.Duration(off)*time.Hour)]; ok && v.N > 0 && v.Saturated == 0 {
					x, y = append(x, v.PM25), append(y, r.PM25)
				}
			}
			if c := pearson(x, y); !math.IsNaN(c) {
				rs = append(rs, c)
			}
		}
		sort.Float64s(rs)
		fmt.Printf("   shift %+d h: median r = %.3f\n", off, rs[len(rs)/2])
	}
	fmt.Println("   (UBA timestamps are CET; the loader converts them to UTC, so the peak should be at 0. Without the conversion it is at +1.)")

	// 2. Health check on the training period: the detector (saturated,
	//    stuck) plus correlation with the nearest reference station. Only
	//    training data is used, so the test period stays unseen.
	trainR := between(d.readings, start, split)
	cfg := stream.Config{Sensors: d.sensors, Humidity: d.rh, Reference: d.ref, Shards: 4}
	sink, _, err := runStream(cfg, trainR)
	if err != nil {
		return err
	}
	broken := map[int][]string{}
	for _, a := range sink.Alerts {
		if a.Kind == stream.Saturated || a.Kind == stream.Stuck {
			broken[a.Sensor] = appendUnique(broken[a.Sensor], string(a.Kind))
		}
	}
	trainCorr := map[int]float64{}
	for _, s := range d.sensors {
		var x, y []float64
		for h, v := range hourly[s.ID] {
			if r, ok := d.ref[s.Ref][h]; ok && h.Before(split) && v.N > 0 && v.Saturated == 0 {
				x, y = append(x, v.PM25), append(y, r.PM25)
			}
		}
		trainCorr[s.ID] = pearson(x, y)
		switch {
		case len(x) < 48:
			broken[s.ID] = appendUnique(broken[s.ID], "too little training data")
		case trainCorr[s.ID] < 0.6:
			broken[s.ID] = appendUnique(broken[s.ID], fmt.Sprintf("does not track reference (r=%.2f)", trainCorr[s.ID]))
		}
	}
	ids := make([]int, 0, len(broken))
	for id := range broken {
		ids = append(ids, id)
	}
	sort.Ints(ids)
	fmt.Printf("\n2. health check on training days only: %d of %d sensors unusable, excluded from calibration\n", len(ids), len(d.sensors))
	for _, id := range ids {
		fmt.Printf("   sensor %-6d %s\n", id, strings.Join(broken[id], ", "))
	}

	// 3. Calibrate the rest.
	train := map[int][]calib.Pair{}
	test := map[int][]calib.Pair{}
	for _, s := range d.sensors {
		if broken[s.ID] != nil {
			continue
		}
		for h, v := range hourly[s.ID] {
			r, ok := d.ref[s.Ref][h]
			rh, ok2 := d.rh[h]
			if !ok || !ok2 || v.N < 6 || v.Saturated > 0 {
				continue
			}
			p := calib.Pair{Raw: v.PM25, RH: rh, Ref: r.PM25}
			if h.Before(split) {
				train[s.ID] = append(train[s.ID], p)
			} else {
				test[s.ID] = append(test[s.ID], p)
			}
		}
	}
	kappa := calib.FitKappa(train)
	models := map[int]calib.Model{}
	linear := map[int]calib.Model{}
	var allTrain []calib.Pair
	for id, ps := range train {
		models[id] = calib.Fit(id, ps, kappa)
		linear[id] = calib.Fit(id, ps, 0)
		allTrain = append(allTrain, ps...)
	}
	global := calib.Fit(0, allTrain, 0)
	type method struct {
		name string
		f    func(id int, p calib.Pair) float64
	}
	methods := []method{
		{"raw sensor", func(_ int, p calib.Pair) float64 { return p.Raw }},
		{"one linear fit for all sensors", func(_ int, p calib.Pair) float64 { return global.Apply(p.Raw, p.RH) }},
		{"per-sensor linear", func(id int, p calib.Pair) float64 { return linear[id].Apply(p.Raw, p.RH) }},
		{fmt.Sprintf("per-sensor + humidity (κ=%.2f)", kappa), func(id int, p calib.Pair) float64 { return models[id].Apply(p.Raw, p.RH) }},
	}
	fmt.Printf("\n3. calibration on held-out days %s .. %s, %d sensors, hourly PM2.5 vs nearest reference station (≤ 1.5 km):\n",
		split.Format("2006-01-02"), end.Add(-time.Hour).Format("2006-01-02"), len(models))
	fmt.Printf("   %-34s %6s %8s %8s %7s %7s\n", "method", "hours", "MAE", "RMSE", "R²", "bias")
	humid := func(p calib.Pair) bool { return p.RH >= 85 }
	for _, m := range methods {
		var pr, rf, prH, rfH []float64
		for id, ps := range test {
			for _, p := range ps {
				v := m.f(id, p)
				pr, rf = append(pr, v), append(rf, p.Ref)
				if humid(p) {
					prH, rfH = append(prH, v), append(rfH, p.Ref)
				}
			}
		}
		s, h := calib.Score(pr, rf), calib.Score(prH, rfH)
		fmt.Printf("   %-34s %6d %8.2f %8.2f %7.3f %+7.2f   (RH≥85%%: MAE %.2f over %d h)\n", m.name, s.N, s.MAE, s.RMSE, s.R2, s.Bias, h.MAE, h.N)
	}

	// 4. Fault injection on the held-out period, healthy sensors only.
	testR := between(d.readings, split, end)
	cfg.Models = models
	clean, took, err := runStream(cfg, testR)
	if err != nil {
		return err
	}
	healthy := make([]int, 0, len(models))
	_ = trainCorr
	for id := range models {
		healthy = append(healthy, id)
	}
	sort.Ints(healthy)
	falseAlarms := 0
	for _, a := range clean.Alerts {
		if models[a.Sensor].Sensor != 0 {
			falseAlarms++
		}
	}
	days := end.Sub(split).Hours() / 24
	kinds := []stream.Kind{stream.Stuck, stream.Saturated, stream.Offline, stream.Spike, stream.Drift}
	hit, total := map[stream.Kind]int{}, map[stream.Kind]int{}
	delays := map[stream.Kind][]float64{}
	var missed []string
	for k := 0; k < *seeds; k++ {
		r, err := inject(cfg, testR, healthy, split, end, *seed+uint64(k))
		if err != nil {
			return err
		}
		for kk, v := range r.hit {
			hit[kk] += v
		}
		for kk, v := range r.total {
			total[kk] += v
		}
		for kk, v := range r.delays {
			delays[kk] = append(delays[kk], v...)
		}
		missed = append(missed, r.missed...)
	}
	fmt.Printf("\n4. fault detection on held-out days: %d healthy sensors, one injected fault each, %d seed(s) from %d (%d faults):\n",
		len(healthy), *seeds, *seed, len(healthy)**seeds)
	nHit, nTot := 0, 0
	for _, k := range kinds {
		ds := delays[k]
		sort.Float64s(ds)
		med := math.NaN()
		if len(ds) > 0 {
			med = ds[len(ds)/2]
		}
		fmt.Printf("   %-10s detected %3d/%-3d  median delay %4.0f h\n", k, hit[k], total[k], med)
		nHit += hit[k]
		nTot += total[k]
	}
	fmt.Printf("   overall %d/%d (%.1f%%)\n", nHit, nTot, 100*float64(nHit)/float64(nTot))
	for i, m := range missed {
		if i == 10 {
			fmt.Printf("   ... %d more misses\n", len(missed)-10)
			break
		}
		fmt.Printf("   missed %s\n", m)
	}
	fmt.Printf("   alerts on the same sensors with no injected faults: %d over %.0f sensor-days\n", falseAlarms, float64(len(healthy))*days)
	for _, a := range clean.Alerts {
		if models[a.Sensor].Sensor != 0 {
			note := ""
			if a.Kind == stream.Offline {
				note = fmt.Sprintf("  [real gap: longest in this sensor's data that day %s]", longestGap(testR, a.Sensor, a.Hour))
			}
			fmt.Printf("     %s sensor %d %s: %s%s\n", a.Hour.Format("01-02 15h"), a.Sensor, a.Kind, a.Detail, note)
		}
	}

	// 5. Throughput: the held-out stream, and the whole month cloned 20×.
	fmt.Printf("\n5. throughput: held-out stream %d readings in %s (%.0f readings/s)\n", len(testR), took.Round(time.Millisecond), float64(len(testR))/took.Seconds())
	big := make([]data.Reading, 0, len(d.readings)*20)
	var bigSensors []data.Sensor
	for k := 0; k < 20; k++ {
		for _, s := range d.sensors {
			s2 := s
			s2.ID += k * 1_000_000
			bigSensors = append(bigSensors, s2)
		}
	}
	for _, r := range d.readings {
		for k := 0; k < 20; k++ {
			r2 := r
			r2.Sensor += k * 1_000_000
			big = append(big, r2)
		}
	}
	bigModels := map[int]calib.Model{}
	for id, m := range models {
		for k := 0; k < 20; k++ {
			bigModels[id+k*1_000_000] = m
		}
	}
	for _, shards := range []int{1, 4, 8} {
		_, tb, err := runStream(stream.Config{Sensors: bigSensors, Models: bigModels, Humidity: d.rh, Reference: d.ref, Shards: shards}, big)
		if err != nil {
			return err
		}
		fmt.Printf("   %d readings from %d sensors, %d aggregator shards: %s (%.0f readings/s)\n", len(big), len(bigSensors), shards, tb.Round(time.Millisecond), float64(len(big))/tb.Seconds())
	}
	return nil
}

func longestGap(rs []data.Reading, sensor int, around time.Time) time.Duration {
	var prev time.Time
	var best time.Duration
	for _, r := range rs {
		if r.Sensor != sensor || r.T.Before(around.Add(-12*time.Hour)) || r.T.After(around.Add(12*time.Hour)) {
			continue
		}
		if !prev.IsZero() && r.T.Sub(prev) > best {
			best = r.T.Sub(prev)
		}
		prev = r.T
	}
	return best.Round(time.Minute)
}

type injResult struct {
	hit, total map[stream.Kind]int
	delays     map[stream.Kind][]float64
	missed     []string
}

func inject(cfg stream.Config, testR []data.Reading, healthy []int, split, end time.Time, seed uint64) (*injResult, error) {
	res := &injResult{hit: map[stream.Kind]int{}, total: map[stream.Kind]int{}, delays: map[stream.Kind][]float64{}}
	rng := rand.New(rand.NewPCG(seed, 77))
	type truth struct {
		sensor   int
		kind     stream.Kind
		from, to time.Time
	}
	kinds := []stream.Kind{stream.Stuck, stream.Saturated, stream.Offline, stream.Spike, stream.Drift}
	var faults []truth
	for i, id := range healthy {
		k := kinds[(i+int(seed))%len(kinds)]
		dur := map[stream.Kind]time.Duration{stream.Stuck: 8 * time.Hour, stream.Saturated: 4 * time.Hour,
			stream.Offline: 6 * time.Hour, stream.Spike: 2 * time.Hour, stream.Drift: 72 * time.Hour}[k]
		room := end.Sub(split) - 24*time.Hour - dur - 6*time.Hour
		if k == stream.Drift {
			room = 48 * time.Hour
		}
		from := split.Add(24*time.Hour + time.Duration(rng.Int64N(int64(room/time.Hour)))*time.Hour)
		to := from.Add(dur)
		if k == stream.Drift {
			to = end
		}
		faults = append(faults, truth{id, k, from, to})
	}
	var out []data.Reading
	for _, r := range testR {
		keep := true
		for _, f := range faults {
			if r.Sensor != f.sensor || r.T.Before(f.from) || !r.T.Before(f.to) {
				continue
			}
			switch f.kind {
			case stream.Stuck:
				r.PM25 = 7.3
			case stream.Saturated:
				r.PM25 = 999.9
			case stream.Offline:
				keep = false
			case stream.Spike:
				r.PM25 = r.PM25*8 + 60
			case stream.Drift:
				r.PM25 *= 1 + 1.5*math.Min(1, r.T.Sub(f.from).Hours()/72)
			}
		}
		if keep {
			out = append(out, r)
		}
	}
	faulted, _, err := runStream(cfg, out)
	if err != nil {
		return nil, err
	}
	for _, f := range faults {
		res.total[f.kind]++
		found := false
		for _, a := range faulted.Alerts {
			if a.Sensor == f.sensor && a.Kind == f.kind && !a.Hour.Before(f.from.Add(-time.Hour)) && a.Hour.Before(f.to.Add(3*time.Hour)) {
				res.hit[f.kind]++
				res.delays[f.kind] = append(res.delays[f.kind], a.Hour.Add(time.Hour).Sub(f.from).Hours())
				found = true
				break
			}
		}
		if !found {
			var got []string
			for _, a := range faulted.Alerts {
				if a.Sensor == f.sensor && !a.Hour.Before(f.from.Add(-time.Hour)) && a.Hour.Before(f.to.Add(3*time.Hour)) {
					got = append(got, string(a.Kind))
				}
			}
			res.missed = append(res.missed, fmt.Sprintf("seed %d: %s on sensor %d from %s (other alerts on it then: %v)", seed, f.kind, f.sensor, f.from.Format("01-02 15h"), got))
		}
	}
	return res, nil
}

func sensorRef(ss []data.Sensor, id int) string {
	for _, s := range ss {
		if s.ID == id {
			return s.Ref
		}
	}
	return ""
}

func appendUnique(s []string, v string) []string {
	for _, x := range s {
		if x == v {
			return s
		}
	}
	return append(s, v)
}

func pearson(x, y []float64) float64 {
	if len(x) < 3 {
		return math.NaN()
	}
	var mx, my float64
	for i := range x {
		mx += x[i]
		my += y[i]
	}
	mx /= float64(len(x))
	my /= float64(len(y))
	var c, vx, vy float64
	for i := range x {
		c += (x[i] - mx) * (y[i] - my)
		vx += (x[i] - mx) * (x[i] - mx)
		vy += (y[i] - my) * (y[i] - my)
	}
	return c / math.Sqrt(vx*vy)
}

// replay streams the month in (sped-up) real time and serves the current
// state on a loopback-only HTTP API.
func replay(args []string) error {
	fs := flag.NewFlagSet("replay", flag.ExitOnError)
	dir := fs.String("data", "testdata", "data directory")
	speed := fs.Float64("speed", 3600, "replay speed (3600 = one hour of data per second)")
	addr := fs.String("addr", "127.0.0.1:8090", "API address (loopback only)")
	fs.Parse(args)
	host, _, err := net.SplitHostPort(*addr)
	if err != nil {
		return err
	}
	if ip := net.ParseIP(host); host != "localhost" && (ip == nil || !ip.IsLoopback()) {
		return errors.New("the replay API only listens on loopback")
	}
	d, err := load(*dir)
	if err != nil {
		return err
	}
	// Fit on the training days, as in eval.
	hourly := calib.Aggregate(between(d.readings, d.readings[0].T, split))
	train := map[int][]calib.Pair{}
	for _, s := range d.sensors {
		for h, v := range hourly[s.ID] {
			if r, ok := d.ref[s.Ref][h]; ok && v.N >= 6 && v.Saturated == 0 {
				train[s.ID] = append(train[s.ID], calib.Pair{Raw: v.PM25, RH: d.rh[h], Ref: r.PM25})
			}
		}
	}
	k := calib.FitKappa(train)
	models := map[int]calib.Model{}
	for id, ps := range train {
		if m := calib.Fit(id, ps, k); m.R2 > 0.5 {
			models[id] = m
		}
	}
	sink := &stream.Sink{}
	ch := make(chan data.Reading, 1024)
	go stream.Run(context.Background(), stream.Config{Sensors: d.sensors, Models: models, Humidity: d.rh, Reference: d.ref}, ch, sink)
	go func() {
		t0, w0 := time.Now(), d.readings[0].T
		for _, r := range d.readings {
			if wait := time.Duration(float64(r.T.Sub(w0))/(*speed)) - time.Since(t0); wait > 0 {
				time.Sleep(wait)
			}
			ch <- r
		}
		close(ch)
	}()
	mux := http.NewServeMux()
	mux.HandleFunc("GET /sensors", func(w http.ResponseWriter, r *http.Request) {
		_, latest := sink.Snapshot()
		json.NewEncoder(w).Encode(latest)
	})
	mux.HandleFunc("GET /alerts", func(w http.ResponseWriter, r *http.Request) {
		alerts, _ := sink.Snapshot()
		json.NewEncoder(w).Encode(alerts)
	})
	fmt.Fprintf(os.Stderr, "replaying %d readings at %.0f×; API on http://%s/sensors and /alerts\n", len(d.readings), *speed, *addr)
	return (&http.Server{Addr: *addr, Handler: mux, ReadHeaderTimeout: 5 * time.Second}).ListenAndServe()
}
