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
	"net"
	"net/http"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/Aravindh97/aqcal/internal/calib"
	"github.com/Aravindh97/aqcal/internal/data"
	"github.com/Aravindh97/aqcal/internal/stream"
	"github.com/Aravindh97/aqcal/internal/study"
)

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

func eval(args []string) error {
	fs := flag.NewFlagSet("eval", flag.ExitOnError)
	dir := fs.String("data", "testdata", "data directory")
	seed := fs.Uint64("seed", 1, "fault injection seed")
	seeds := fs.Int("seeds", 1, "run the fault-injection test with this many seeds (seed, seed+1, ...) and report totals")
	fs.Parse(args)
	d, err := study.Load(*dir)
	if err != nil {
		return err
	}
	split, start, end := study.Split, d.Start, d.End
	fmt.Printf("data: %d readings from %d SDS011 sensors (%d unparseable rows), %s .. %s; reference: %d UBA stations; train before %s\n\n",
		len(d.Readings), len(d.Sensors), d.Bad, start.Format("2006-01-02"), end.Format("2006-01-02"), len(d.Ref), split.Format("2006-01-02"))

	// 1. Time alignment: the shift between sensor UTC hours and the
	//    reference hours that maximises correlation.
	fmt.Println("1. time alignment: median over sensors of the correlation between hourly sensor medians and the reference, by shift:")
	for _, off := range []int{-2, -1, 0, 1, 2} {
		var rs []float64
		for _, s := range d.Sensors {
			var x, y []float64
			for h, v := range d.Hourly[s.ID] {
				if r, ok := d.Ref[s.Ref][h.Add(time.Duration(off)*time.Hour)]; ok && v.N > 0 && v.Saturated == 0 {
					x, y = append(x, v.PM25), append(y, r.PM25)
				}
			}
			if c := study.Pearson(x, y); !math.IsNaN(c) {
				rs = append(rs, c)
			}
		}
		sort.Float64s(rs)
		fmt.Printf("   shift %+d h: median r = %.3f\n", off, rs[len(rs)/2])
	}
	fmt.Println("   (UBA timestamps are CET; the loader converts them to UTC, so the peak should be at 0. Without the conversion it is at +1.)")

	// 2. Health check on training days only.
	broken, _, err := d.Health()
	if err != nil {
		return err
	}
	ids := make([]int, 0, len(broken))
	for id := range broken {
		ids = append(ids, id)
	}
	sort.Ints(ids)
	fmt.Printf("\n2. health check on training days only: %d of %d sensors unusable, excluded from calibration\n", len(ids), len(d.Sensors))
	for _, id := range ids {
		fmt.Printf("   sensor %-6d %s\n", id, strings.Join(broken[id], ", "))
	}

	// 3. Calibrate the rest and score on held-out days.
	f := d.Fit(broken)
	fmt.Printf("\n3. calibration on held-out days %s .. %s, %d sensors, hourly PM2.5 vs nearest reference station (≤ 1.5 km):\n",
		split.Format("2006-01-02"), end.Add(-time.Hour).Format("2006-01-02"), len(f.Models))
	fmt.Printf("   %-34s %6s %8s %8s %7s %7s\n", "method", "hours", "MAE", "RMSE", "R²", "bias")
	for _, m := range f.Methods() {
		s, h := f.Score(m)
		fmt.Printf("   %-34s %6d %8.2f %8.2f %7.3f %+7.2f   (RH≥85%%: MAE %.2f over %d h)\n", m.Name, s.N, s.MAE, s.RMSE, s.R2, s.Bias, h.MAE, h.N)
	}

	// 4. Fault injection on held-out days, healthy sensors only.
	testR := d.Between(split, end)
	cfg := stream.Config{Sensors: d.Sensors, Humidity: d.RH, Reference: d.Ref, Shards: 4, Models: f.Models}
	clean, took, err := study.RunStream(cfg, testR)
	if err != nil {
		return err
	}
	healthy := f.Healthy()
	falseAlarms := 0
	for _, a := range clean.Alerts {
		if f.Models[a.Sensor].Sensor != 0 {
			falseAlarms++
		}
	}
	days := end.Sub(split).Hours() / 24
	hit, total := map[stream.Kind]int{}, map[stream.Kind]int{}
	delays := map[stream.Kind][]float64{}
	var missed []string
	for k := 0; k < *seeds; k++ {
		r, err := study.Inject(cfg, testR, healthy, end, *seed+uint64(k))
		if err != nil {
			return err
		}
		for kk, v := range r.Hit {
			hit[kk] += v
		}
		for kk, v := range r.Total {
			total[kk] += v
		}
		for kk, v := range r.Delays {
			delays[kk] = append(delays[kk], v...)
		}
		missed = append(missed, r.Missed...)
	}
	fmt.Printf("\n4. fault detection on held-out days: %d healthy sensors, one injected fault each, %d seed(s) from %d (%d faults):\n",
		len(healthy), *seeds, *seed, len(healthy)**seeds)
	nHit, nTot := 0, 0
	for _, k := range study.Kinds {
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
		if f.Models[a.Sensor].Sensor != 0 {
			note := ""
			if a.Kind == stream.Offline {
				note = fmt.Sprintf("  [real gap: longest in this sensor's data that day %s]", longestGap(testR, a.Sensor, a.Hour))
			}
			fmt.Printf("     %s sensor %d %s: %s%s\n", a.Hour.Format("01-02 15h"), a.Sensor, a.Kind, a.Detail, note)
		}
	}

	// 5. Throughput: the held-out stream, and the whole month cloned 20×.
	fmt.Printf("\n5. throughput: held-out stream %d readings in %s (%.0f readings/s)\n", len(testR), took.Round(time.Millisecond), float64(len(testR))/took.Seconds())
	big := make([]data.Reading, 0, len(d.Readings)*20)
	var bigSensors []data.Sensor
	for k := 0; k < 20; k++ {
		for _, s := range d.Sensors {
			s2 := s
			s2.ID += k * 1_000_000
			bigSensors = append(bigSensors, s2)
		}
	}
	for _, r := range d.Readings {
		for k := 0; k < 20; k++ {
			r2 := r
			r2.Sensor += k * 1_000_000
			big = append(big, r2)
		}
	}
	bigModels := map[int]calib.Model{}
	for id, m := range f.Models {
		for k := 0; k < 20; k++ {
			bigModels[id+k*1_000_000] = m
		}
	}
	for _, shards := range []int{1, 4, 8} {
		_, tb, err := study.RunStream(stream.Config{Sensors: bigSensors, Models: bigModels, Humidity: d.RH, Reference: d.Ref, Shards: shards}, big)
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
	d, err := study.Load(*dir)
	if err != nil {
		return err
	}
	// Same health check and fits as eval: training days only.
	broken, _, err := d.Health()
	if err != nil {
		return err
	}
	models := d.Fit(broken).Models
	sink := &stream.Sink{}
	ch := make(chan data.Reading, 1024)
	go stream.Run(context.Background(), stream.Config{Sensors: d.Sensors, Models: models, Humidity: d.RH, Reference: d.Ref}, ch, sink)
	go func() {
		t0, w0 := time.Now(), d.Readings[0].T
		for _, r := range d.Readings {
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
	fmt.Fprintf(os.Stderr, "replaying %d readings at %.0f×; API on http://%s/sensors and /alerts\n", len(d.Readings), *speed, *addr)
	return (&http.Server{Addr: *addr, Handler: mux, ReadHeaderTimeout: 5 * time.Second}).ListenAndServe()
}
