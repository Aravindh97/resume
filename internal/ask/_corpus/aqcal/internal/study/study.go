// Package study is the calibration study shared by the CLI and the browser
// build: the health check, the fits and the fault-injection test, all
// using only training days for anything that decides a model.
package study

import (
	"bytes"
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"math"
	"math/rand/v2"
	"sort"
	"time"

	"github.com/Aravindh97/aqcal/internal/calib"
	"github.com/Aravindh97/aqcal/internal/data"
	"github.com/Aravindh97/aqcal/internal/stream"
)

// Split separates training (1-18 Oct) from held-out days (19-28 Oct).
var Split = time.Date(2026, 10, 19, 0, 0, 0, 0, time.UTC)

type Dataset struct {
	Readings []data.Reading
	Ref      map[string]map[time.Time]data.Ref
	RH       map[time.Time]float64
	Sensors  []data.Sensor
	Bad      int
	Start    time.Time
	End      time.Time
	Hourly   map[int]map[time.Time]calib.Hourly
}

func Load(dir string) (*Dataset, error) {
	d := &Dataset{}
	var err error
	if d.Readings, d.Bad, err = data.LoadReadings(dir + "/lowcost-2026-10.csv.gz"); err != nil {
		return nil, err
	}
	if d.Ref, err = data.LoadReference(dir + "/reference-uba-2026-10.csv"); err != nil {
		return nil, err
	}
	if d.RH, err = data.LoadHumidity(dir + "/humidity-berlin-2026-10.json"); err != nil {
		return nil, err
	}
	if d.Sensors, err = data.LoadSensors(dir + "/sensors.json"); err != nil {
		return nil, err
	}
	d.finish()
	return d, nil
}

// Parse builds a dataset from the same four files held in memory.
func Parse(readingsGz, ref, humidity, sensors []byte) (*Dataset, error) {
	d := &Dataset{}
	gz, err := gzip.NewReader(bytes.NewReader(readingsGz))
	if err != nil {
		return nil, err
	}
	raw, err := io.ReadAll(gz)
	if err != nil {
		return nil, err
	}
	if d.Readings, d.Bad, err = data.ReadReadings(bytes.NewReader(raw)); err != nil {
		return nil, err
	}
	if d.Ref, err = data.ReadReference(bytes.NewReader(ref)); err != nil {
		return nil, err
	}
	if d.RH, err = data.ParseHumidity(humidity); err != nil {
		return nil, err
	}
	if d.Sensors, err = data.ParseSensors(sensors); err != nil {
		return nil, err
	}
	d.finish()
	return d, nil
}

func (d *Dataset) finish() {
	sort.SliceStable(d.Readings, func(i, j int) bool { return d.Readings[i].T.Before(d.Readings[j].T) })
	d.Start = d.Readings[0].T.Truncate(24 * time.Hour)
	d.End = d.Readings[len(d.Readings)-1].T.Add(time.Hour).Truncate(time.Hour)
	d.Hourly = calib.Aggregate(d.Readings)
}

func (d *Dataset) Between(from, to time.Time) []data.Reading {
	var out []data.Reading
	for _, r := range d.Readings {
		if !r.T.Before(from) && r.T.Before(to) {
			out = append(out, r)
		}
	}
	return out
}

func (d *Dataset) RefOf(id int) string {
	for _, s := range d.Sensors {
		if s.ID == id {
			return s.Ref
		}
	}
	return ""
}

func RunStream(c stream.Config, rs []data.Reading) (*stream.Sink, time.Duration, error) {
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

func Pearson(x, y []float64) float64 {
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

func appendUnique(s []string, v string) []string {
	for _, x := range s {
		if x == v {
			return s
		}
	}
	return append(s, v)
}

// Health runs the detector and a reference correlation on training days
// only, and returns the reasons each unusable sensor was excluded.
func (d *Dataset) Health() (map[int][]string, map[int]float64, error) {
	cfg := stream.Config{Sensors: d.Sensors, Humidity: d.RH, Reference: d.Ref, Shards: 4}
	sink, _, err := RunStream(cfg, d.Between(d.Start, Split))
	if err != nil {
		return nil, nil, err
	}
	broken := map[int][]string{}
	for _, a := range sink.Alerts {
		if a.Kind == stream.Saturated || a.Kind == stream.Stuck {
			broken[a.Sensor] = appendUnique(broken[a.Sensor], string(a.Kind))
		}
	}
	corr := map[int]float64{}
	for _, s := range d.Sensors {
		var x, y []float64
		for h, v := range d.Hourly[s.ID] {
			if r, ok := d.Ref[s.Ref][h]; ok && h.Before(Split) && v.N > 0 && v.Saturated == 0 {
				x, y = append(x, v.PM25), append(y, r.PM25)
			}
		}
		corr[s.ID] = Pearson(x, y)
		switch {
		case len(x) < 48:
			broken[s.ID] = appendUnique(broken[s.ID], "too little training data")
		case corr[s.ID] < 0.6:
			broken[s.ID] = appendUnique(broken[s.ID], fmt.Sprintf("does not track reference (r=%.2f)", corr[s.ID]))
		}
	}
	return broken, corr, nil
}

type Fits struct {
	Kappa  float64
	Models map[int]calib.Model // per sensor, with humidity correction
	Linear map[int]calib.Model // per sensor, no humidity term
	Global calib.Model         // one fit for all sensors
	Train  map[int][]calib.Pair
	Test   map[int][]calib.Pair
}

func (d *Dataset) Fit(broken map[int][]string) *Fits {
	f := &Fits{Models: map[int]calib.Model{}, Linear: map[int]calib.Model{}, Train: map[int][]calib.Pair{}, Test: map[int][]calib.Pair{}}
	for _, s := range d.Sensors {
		if broken[s.ID] != nil {
			continue
		}
		for h, v := range d.Hourly[s.ID] {
			r, ok := d.Ref[s.Ref][h]
			rh, ok2 := d.RH[h]
			if !ok || !ok2 || v.N < 6 || v.Saturated > 0 {
				continue
			}
			p := calib.Pair{Raw: v.PM25, RH: rh, Ref: r.PM25}
			if h.Before(Split) {
				f.Train[s.ID] = append(f.Train[s.ID], p)
			} else {
				f.Test[s.ID] = append(f.Test[s.ID], p)
			}
		}
	}
	f.Kappa = calib.FitKappa(f.Train)
	var all []calib.Pair
	for id, ps := range f.Train {
		f.Models[id] = calib.Fit(id, ps, f.Kappa)
		f.Linear[id] = calib.Fit(id, ps, 0)
		all = append(all, ps...)
	}
	f.Global = calib.Fit(0, all, 0)
	return f
}

type Method struct {
	Name string
	F    func(id int, p calib.Pair) float64
}

func (f *Fits) Methods() []Method {
	return []Method{
		{"raw sensor", func(_ int, p calib.Pair) float64 { return p.Raw }},
		{"one linear fit for all sensors", func(_ int, p calib.Pair) float64 { return f.Global.Apply(p.Raw, p.RH) }},
		{"per-sensor linear", func(id int, p calib.Pair) float64 { return f.Linear[id].Apply(p.Raw, p.RH) }},
		{fmt.Sprintf("per-sensor + humidity (κ=%.2f)", f.Kappa), func(id int, p calib.Pair) float64 { return f.Models[id].Apply(p.Raw, p.RH) }},
	}
}

// Score evaluates a method on the held-out pairs: all hours, and humid ones.
func (f *Fits) Score(m Method) (all, humid calib.Metrics) {
	var pr, rf, prH, rfH []float64
	for id, ps := range f.Test {
		for _, p := range ps {
			v := m.F(id, p)
			pr, rf = append(pr, v), append(rf, p.Ref)
			if p.RH >= 85 {
				prH, rfH = append(prH, v), append(rfH, p.Ref)
			}
		}
	}
	return calib.Score(pr, rf), calib.Score(prH, rfH)
}

func (f *Fits) Healthy() []int {
	ids := make([]int, 0, len(f.Models))
	for id := range f.Models {
		ids = append(ids, id)
	}
	sort.Ints(ids)
	return ids
}

type Fault struct {
	Sensor   int
	Kind     stream.Kind
	From, To time.Time
}

// Durations of the injected faults (drift ramps for 72 h, then holds).
var Durations = map[stream.Kind]time.Duration{stream.Stuck: 8 * time.Hour, stream.Saturated: 4 * time.Hour,
	stream.Offline: 6 * time.Hour, stream.Spike: 2 * time.Hour, stream.Drift: 72 * time.Hour}

// Apply returns a copy of rs with the faults injected.
func Apply(rs []data.Reading, faults []Fault) []data.Reading {
	var out []data.Reading
	for _, r := range rs {
		keep := true
		for _, f := range faults {
			if r.Sensor != f.Sensor || r.T.Before(f.From) || !r.T.Before(f.To) {
				continue
			}
			switch f.Kind {
			case stream.Stuck:
				r.PM25 = 7.3
			case stream.Saturated:
				r.PM25 = 999.9
			case stream.Offline:
				keep = false
			case stream.Spike:
				r.PM25 = r.PM25*8 + 60
			case stream.Drift:
				r.PM25 *= 1 + 1.5*math.Min(1, r.T.Sub(f.From).Hours()/72)
			}
		}
		if keep {
			out = append(out, r)
		}
	}
	return out
}

// Caught reports whether alerts include the fault's kind for its sensor
// within the fault window (plus 3 h), and when.
func Caught(f Fault, alerts []stream.Alert) (bool, time.Time) {
	for _, a := range alerts {
		if a.Sensor == f.Sensor && a.Kind == f.Kind && !a.Hour.Before(f.From.Add(-time.Hour)) && a.Hour.Before(f.To.Add(3*time.Hour)) {
			return true, a.Hour.Add(time.Hour)
		}
	}
	return false, time.Time{}
}

type InjResult struct {
	Hit, Total map[stream.Kind]int
	Delays     map[stream.Kind][]float64
	Missed     []string
}

var Kinds = []stream.Kind{stream.Stuck, stream.Saturated, stream.Offline, stream.Spike, stream.Drift}

// Inject puts one fault on every healthy sensor (kind rotating with the
// seed), runs the stream, and scores detection.
func Inject(cfg stream.Config, testR []data.Reading, healthy []int, end time.Time, seed uint64) (*InjResult, error) {
	res := &InjResult{Hit: map[stream.Kind]int{}, Total: map[stream.Kind]int{}, Delays: map[stream.Kind][]float64{}}
	rng := rand.New(rand.NewPCG(seed, 77))
	var faults []Fault
	for i, id := range healthy {
		k := Kinds[(i+int(seed))%len(Kinds)]
		dur := Durations[k]
		room := end.Sub(Split) - 24*time.Hour - dur - 6*time.Hour
		if k == stream.Drift {
			room = 48 * time.Hour
		}
		from := Split.Add(24*time.Hour + time.Duration(rng.Int64N(int64(room/time.Hour)))*time.Hour)
		to := from.Add(dur)
		if k == stream.Drift {
			to = end
		}
		faults = append(faults, Fault{id, k, from, to})
	}
	sink, _, err := RunStream(cfg, Apply(testR, faults))
	if err != nil {
		return nil, err
	}
	for _, f := range faults {
		res.Total[f.Kind]++
		if ok, at := Caught(f, sink.Alerts); ok {
			res.Hit[f.Kind]++
			res.Delays[f.Kind] = append(res.Delays[f.Kind], at.Sub(f.From).Hours())
			continue
		}
		var got []string
		for _, a := range sink.Alerts {
			if a.Sensor == f.Sensor && !a.Hour.Before(f.From.Add(-time.Hour)) && a.Hour.Before(f.To.Add(3*time.Hour)) {
				got = append(got, string(a.Kind))
			}
		}
		res.Missed = append(res.Missed, fmt.Sprintf("seed %d: %s on sensor %d from %s (other alerts on it then: %v)", seed, f.Kind, f.Sensor, f.From.Format("01-02 15h"), got))
	}
	return res, nil
}
