//go:build js && wasm

// Command web runs the aqcal calibration study and fault detector in the
// browser on the embedded Berlin data.
package main

import (
	_ "embed"
	"encoding/json"
	"math"
	"net/url"
	"sort"
	"strconv"
	"time"

	"github.com/Aravindh97/aqcal/internal/calib"
	"github.com/Aravindh97/aqcal/internal/data"
	"github.com/Aravindh97/aqcal/internal/stream"
	"github.com/Aravindh97/aqcal/internal/study"
)

var (
	//go:embed embed/lowcost-2026-10.csv.gz
	readings []byte
	//go:embed embed/reference-uba-2026-10.csv
	reference []byte
	//go:embed embed/humidity-berlin-2026-10.json
	humidity []byte
	//go:embed embed/sensors.json
	sensors []byte
)

type state struct {
	d      *study.Dataset
	broken map[int][]string
	corr   map[int]float64
	fits   *study.Fits
	cfg    stream.Config
	testR  []data.Reading
}

func setup() (*state, error) {
	d, err := study.Parse(readings, reference, humidity, sensors)
	if err != nil {
		return nil, err
	}
	broken, corr, err := d.Health()
	if err != nil {
		return nil, err
	}
	f := d.Fit(broken)
	return &state{d: d, broken: broken, corr: corr, fits: f, testR: d.Between(study.Split, d.End),
		cfg: stream.Config{Sensors: d.Sensors, Humidity: d.RH, Reference: d.Ref, Shards: 4, Models: f.Models}}, nil
}

func r2(x float64) any {
	if math.IsNaN(x) || math.IsInf(x, 0) {
		return nil
	}
	return math.Round(x*100) / 100
}

func (s *state) series(sensor int, rs []data.Reading) []map[string]any {
	hourly := calib.Aggregate(rs)[sensor]
	m, hasModel := s.fits.Models[sensor]
	ref := s.d.Ref[s.d.RefOf(sensor)]
	var hours []time.Time
	for h := s.d.Start; h.Before(s.d.End); h = h.Add(time.Hour) {
		hours = append(hours, h)
	}
	var out []map[string]any
	for _, h := range hours {
		row := map[string]any{"t": h.Format("01-02 15h")}
		if v, ok := hourly[h]; ok && v.N > 0 {
			row["raw"] = r2(v.PM25)
			if hasModel {
				row["cal"] = r2(m.Apply(v.PM25, s.d.RH[h]))
			}
		}
		if r, ok := ref[h]; ok {
			row["ref"] = r2(r.PM25)
		}
		out = append(out, row)
	}
	return out
}

func main() {
	s, err := setup()
	serve(map[string]handler{
		"GET overview": func(url.Values, []byte) resp {
			if err != nil {
				return fail(500, err.Error())
			}
			var ss []map[string]any
			for _, x := range s.d.Sensors {
				row := map[string]any{"id": x.ID, "ref": x.RefName, "dist_km": x.DistKm, "train_r": r2(s.corr[x.ID]), "broken": s.broken[x.ID]}
				if m, ok := s.fits.Models[x.ID]; ok {
					row["a"], row["b"], row["fit_r2"] = r2(m.A), r2(m.B), r2(m.R2)
				}
				ss = append(ss, row)
			}
			var ms []map[string]any
			for _, m := range s.fits.Methods() {
				a, h := s.fits.Score(m)
				ms = append(ms, map[string]any{"name": m.Name, "hours": a.N, "mae": r2(a.MAE), "rmse": r2(a.RMSE), "r2": r2(a.R2), "humid_mae": r2(h.MAE)})
			}
			return ok(map[string]any{"readings": len(s.d.Readings), "split": study.Split.Format("2006-01-02"), "kappa": s.fits.Kappa, "sensors": ss, "methods": ms})
		},
		"GET series": func(q url.Values, _ []byte) resp {
			if err != nil {
				return fail(500, err.Error())
			}
			id, _ := strconv.Atoi(q.Get("sensor"))
			return ok(map[string]any{"sensor": id, "split": study.Split.Format("01-02 15h"), "hours": s.series(id, s.d.Readings)})
		},
		"POST inject": func(_ url.Values, b []byte) resp {
			if err != nil {
				return fail(500, err.Error())
			}
			var in struct {
				Sensor int
				Kind   string
				Day    int // days after the start of the held-out period
			}
			if json.Unmarshal(b, &in) != nil {
				return fail(400, "bad request")
			}
			if _, ok := s.fits.Models[in.Sensor]; !ok {
				return fail(400, "pick a healthy sensor: broken ones are already excluded")
			}
			k := stream.Kind(in.Kind)
			dur, found := study.Durations[k]
			if !found {
				return fail(400, "unknown fault kind")
			}
			from := study.Split.Add(time.Duration(min(max(in.Day, 1), 6))*24*time.Hour + 10*time.Hour)
			f := study.Fault{Sensor: in.Sensor, Kind: k, From: from, To: from.Add(dur)}
			if k == stream.Drift {
				f.To = s.d.End
			}
			faulty := study.Apply(s.testR, []study.Fault{f})
			sink, took, err := study.RunStream(s.cfg, faulty)
			if err != nil {
				return fail(500, err.Error())
			}
			caught, at := study.Caught(f, sink.Alerts)
			var alerts []stream.Alert
			for _, a := range sink.Alerts {
				if a.Sensor == in.Sensor {
					alerts = append(alerts, a)
				}
			}
			all := append(s.d.Between(s.d.Start, study.Split), faulty...)
			res := map[string]any{"fault": map[string]any{"kind": k, "from": f.From.Format("01-02 15h"), "to": f.To.Format("01-02 15h")},
				"caught": caught, "alerts": alerts, "readings": len(faulty), "took_ms": took.Milliseconds(), "hours": s.series(in.Sensor, all)}
			if caught {
				res["delay_h"] = at.Sub(f.From).Hours()
			}
			return ok(res)
		},
		"POST faults": func(q url.Values, _ []byte) resp {
			if err != nil {
				return fail(500, err.Error())
			}
			seeds, _ := strconv.Atoi(q.Get("seeds"))
			seeds = min(max(seeds, 1), 20)
			hit, total := map[stream.Kind]int{}, map[stream.Kind]int{}
			delays := map[stream.Kind][]float64{}
			t := time.Now()
			for k := 0; k < seeds; k++ {
				r, err := study.Inject(s.cfg, s.testR, s.fits.Healthy(), s.d.End, uint64(1+k))
				if err != nil {
					return fail(500, err.Error())
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
			}
			var rows []map[string]any
			for _, k := range study.Kinds {
				ds := delays[k]
				sort.Float64s(ds)
				var med any
				if len(ds) > 0 {
					med = ds[len(ds)/2]
				}
				rows = append(rows, map[string]any{"kind": k, "hit": hit[k], "total": total[k], "median_delay_h": med})
			}
			return ok(map[string]any{"seeds": seeds, "rows": rows, "took_ms": time.Since(t).Milliseconds()})
		},
	})
}
