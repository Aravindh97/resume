// Package calib turns raw low-cost readings into numbers that agree with
// reference stations.
//
// SDS011 sensors count particles optically. In humid air particles absorb
// water and grow, so the sensor over-reads; and every unit has its own gain.
// The model, fitted per sensor on a training period:
//
//	reference ≈ a + b · raw / g(RH)      g(RH) = 1 + κ · aw/(1 − aw),  aw = RH/100
//
// g is the κ-Köhler hygroscopic growth factor (Petters & Kreidenweis 2007).
// κ is fitted once for the city (it depends on the aerosol, not the sensor);
// a and b per sensor by least squares, with residual outliers removed and
// the fit repeated.
package calib

import (
	"math"
	"sort"
	"time"

	"github.com/Aravindh97/aqcal/internal/data"
)

// Hourly is one sensor-hour.
type Hourly struct {
	Sensor    int
	Hour      time.Time // UTC hour start
	PM25      float64   // median of the readings in the hour
	N         int
	Saturated int     // readings at the SDS011 maximum (999.9)
	Spread    float64 // max - min within the hour (0 with many readings = stuck)
}

const Saturation = 999.0

// Aggregate builds hourly values from readings (batch use; the streaming
// pipeline does the same incrementally).
func Aggregate(rs []data.Reading) map[int]map[time.Time]Hourly {
	vals := map[int]map[time.Time][]float64{}
	sat := map[int]map[time.Time]int{}
	for _, r := range rs {
		h := r.T.Truncate(time.Hour)
		if vals[r.Sensor] == nil {
			vals[r.Sensor], sat[r.Sensor] = map[time.Time][]float64{}, map[time.Time]int{}
		}
		if r.PM25 >= Saturation {
			sat[r.Sensor][h]++
			continue
		}
		vals[r.Sensor][h] = append(vals[r.Sensor][h], r.PM25)
	}
	out := map[int]map[time.Time]Hourly{}
	for s, hs := range vals {
		out[s] = map[time.Time]Hourly{}
		for h, v := range hs {
			out[s][h] = HourOf(s, h, v, sat[s][h])
		}
		for h, n := range sat[s] {
			if _, ok := out[s][h]; !ok {
				out[s][h] = Hourly{Sensor: s, Hour: h, Saturated: n}
			}
		}
	}
	return out
}

func HourOf(sensor int, h time.Time, v []float64, saturated int) Hourly {
	x := Hourly{Sensor: sensor, Hour: h, N: len(v), Saturated: saturated}
	if len(v) == 0 {
		return x
	}
	s := append([]float64(nil), v...)
	sort.Float64s(s)
	x.PM25 = s[len(s)/2]
	if len(s)%2 == 0 {
		x.PM25 = (s[len(s)/2-1] + s[len(s)/2]) / 2
	}
	x.Spread = s[len(s)-1] - s[0]
	return x
}

func Growth(rh, kappa float64) float64 {
	aw := math.Min(rh, 95) / 100 // above ~95% the correction explodes; cap it
	return 1 + kappa*aw/(1-aw)
}

type Model struct {
	Sensor int     `json:"sensor"`
	A      float64 `json:"a"`
	B      float64 `json:"b"`
	Kappa  float64 `json:"kappa"`
	R2     float64 `json:"train_r2"` // fit quality on the training period
	N      int     `json:"train_hours"`
	// Gain0 is the sensor's raw gain against the reference in training,
	// Σ raw·ref / Σ ref² over hours with ref ≥ 5. Drift detection compares
	// the recent gain with it.
	Gain0 float64 `json:"gain0"`
}

// Gain is Σ raw·ref / Σ ref² over pairs with ref ≥ 5 µg/m³.
func Gain(ps []Pair) (float64, int) {
	var sxy, syy float64
	n := 0
	for _, p := range ps {
		if p.Ref >= 5 {
			sxy += p.Raw * p.Ref
			syy += p.Ref * p.Ref
			n++
		}
	}
	if syy == 0 {
		return 0, 0
	}
	return sxy / syy, n
}

func (m Model) Apply(raw, rh float64) float64 {
	v := m.A + m.B*raw/Growth(rh, m.Kappa)
	return math.Max(0, v)
}

type Pair struct {
	Raw, RH, Ref float64
}

// ols fits y = a + b x.
func ols(x, y []float64) (a, b, r2 float64) {
	n := float64(len(x))
	if n < 3 {
		return 0, 1, 0
	}
	var sx, sy, sxx, sxy, syy float64
	for i := range x {
		sx += x[i]
		sy += y[i]
		sxx += x[i] * x[i]
		sxy += x[i] * y[i]
		syy += y[i] * y[i]
	}
	d := n*sxx - sx*sx
	if d == 0 {
		return sy / n, 0, 0
	}
	b = (n*sxy - sx*sy) / d
	a = (sy - b*sx) / n
	var ssr, sst float64
	my := sy / n
	for i := range x {
		e := y[i] - (a + b*x[i])
		ssr += e * e
		sst += (y[i] - my) * (y[i] - my)
	}
	if sst > 0 {
		r2 = 1 - ssr/sst
	}
	return a, b, r2
}

// Fit fits one sensor with a given κ, dropping residual outliers (beyond
// 4 median absolute deviations) and refitting once.
func Fit(sensor int, ps []Pair, kappa float64) Model {
	x := make([]float64, len(ps))
	y := make([]float64, len(ps))
	for i, p := range ps {
		x[i], y[i] = p.Raw/Growth(p.RH, kappa), p.Ref
	}
	a, b, _ := ols(x, y)
	res := make([]float64, len(x))
	for i := range x {
		res[i] = math.Abs(y[i] - (a + b*x[i]))
	}
	med := median(res)
	var x2, y2 []float64
	for i := range x {
		if res[i] <= 4*1.4826*med+1e-9 {
			x2, y2 = append(x2, x[i]), append(y2, y[i])
		}
	}
	a, b, r2 := ols(x2, y2)
	g, _ := Gain(ps)
	return Model{Sensor: sensor, A: a, B: b, Kappa: kappa, R2: r2, N: len(x2), Gain0: g}
}

// FitKappa picks the city-wide κ that minimises total squared error over
// the given sensors' training data.
func FitKappa(train map[int][]Pair) float64 {
	best, bestErr := 0.0, math.Inf(1)
	for k := 0.0; k <= 1.0001; k += 0.02 {
		var e float64
		for s, ps := range train {
			m := Fit(s, ps, k)
			for _, p := range ps {
				d := m.Apply(p.Raw, p.RH) - p.Ref
				e += math.Min(d*d, 400) // cap so a broken sensor cannot decide κ
			}
		}
		if e < bestErr {
			best, bestErr = k, e
		}
	}
	return best
}

type Metrics struct {
	N             int
	MAE, RMSE, R2 float64
	Bias          float64
}

func Score(pred, ref []float64) Metrics {
	m := Metrics{N: len(pred)}
	if m.N == 0 {
		return m
	}
	var my float64
	for _, y := range ref {
		my += y
	}
	my /= float64(m.N)
	var se, ae, b, st float64
	for i := range pred {
		d := pred[i] - ref[i]
		se += d * d
		ae += math.Abs(d)
		b += d
		st += (ref[i] - my) * (ref[i] - my)
	}
	m.MAE, m.RMSE, m.Bias = ae/float64(m.N), math.Sqrt(se/float64(m.N)), b/float64(m.N)
	if st > 0 {
		m.R2 = 1 - se/st
	}
	return m
}

func median(v []float64) float64 {
	if len(v) == 0 {
		return 0
	}
	s := append([]float64(nil), v...)
	sort.Float64s(s)
	return s[len(s)/2]
}
