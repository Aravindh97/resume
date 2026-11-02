// Package signals turns the raw series into monthly early-warning signals
// per region.
//
// Data arrives late: CHIRPS final rainfall for a month is published about
// three weeks after it ends, WFP prices about a month after. So the signal
// for month m uses rainfall and prices up to month m-DataLag only, and is
// itself available at the end of month m.
//
//	rain deficit   rainfall in the current rainy season so far (Gu: Apr-Jun,
//	               Deyr: Oct-Dec), or in the last completed season outside
//	               them, is in the driest 30% of 1991-2020 for the same
//	               months in that region
//	price spike    the region's staple cereal price (median across markets
//	               of red sorghum / white maize / white sorghum, each
//	               relative to its own previous 24-month median) is 40% or
//	               more above normal
//	terms of trade kilograms of cereal one goat buys have fallen to 70% or
//	               less of the previous 24-month median: pastoralists, who
//	               sell animals to buy grain, are losing purchasing power
//
// The alert is WARNING when a rain deficit coincides with a market signal,
// WATCH when any single signal is active.
package signals

import (
	"fmt"
	"math"
	"sort"
	"strings"
	"time"

	"github.com/Aravindh97/foodwatch/internal/data"
)

type Level int

const (
	None Level = iota
	Watch
	Warning
)

func (l Level) String() string { return [...]string{"none", "WATCH", "WARNING"}[l] }

type Month struct {
	Month       data.Month
	RainPct     float64 // percentile of the last completed season, NaN if unknown
	Season      string
	PriceRatio  float64 // cereal price vs previous 24-month median, NaN if unknown
	ToTRatio    float64 // goat-to-cereal terms of trade vs previous 24-month median
	RainDeficit bool
	PriceSpike  bool
	ToTDrop     bool
	Level       Level
}

func (m Month) Reasons() string {
	var r []string
	if m.RainDeficit {
		r = append(r, fmt.Sprintf("%s rains at percentile %.0f", m.Season, m.RainPct*100))
	}
	if m.PriceSpike {
		r = append(r, fmt.Sprintf("cereal price %.0f%% above normal", (m.PriceRatio-1)*100))
	}
	if m.ToTDrop {
		r = append(r, fmt.Sprintf("goat buys %.0f%% of its usual cereal", m.ToTRatio*100))
	}
	return strings.Join(r, "; ")
}

type Config struct {
	RainPct      float64 // deficit below this percentile
	PriceSpike   float64
	ToTDrop      float64
	ClimateStart int
	ClimateEnd   int
	DataLag      int  // months between a month ending and its data being usable
	SeasonToDate bool // score a season while it is still running
}

func Default() Config {
	return Config{RainPct: 0.30, PriceSpike: 1.40, ToTDrop: 0.70, ClimateStart: 1991, ClimateEnd: 2020, DataLag: 1, SeasonToDate: true}
}

var cereals = map[string]bool{"Sorghum (red)": true, "Maize (white)": true, "Sorghum (white)": true}

type season struct {
	name   string
	months []time.Month
}

var seasons = []season{{"Gu", []time.Month{4, 5, 6}}, {"Deyr", []time.Month{10, 11, 12}}}

// Compute returns region -> month -> signals, for months in [from, to].
func Compute(prices []data.Price, rain map[string]map[data.Month]float64, c Config, from, to data.Month) map[string][]Month {
	// median price per region, commodity and month (across markets)
	type key struct {
		region, commodity string
		m                 data.Month
	}
	byKey := map[key][]float64{}
	for _, p := range prices {
		if cereals[p.Commodity] || p.Commodity == "Livestock (Goat)" {
			k := key{p.Region, p.Commodity, p.Month}
			byKey[k] = append(byKey[k], p.Price)
		}
	}
	price := map[key]float64{}
	for k, v := range byKey {
		price[k] = median(v)
	}
	regions := map[string]bool{}
	for k := range price {
		regions[k.region] = true
	}
	out := map[string][]Month{}
	for reg := range regions {
		for m := from; !m.After(to); m = m.AddDate(0, 1, 0) {
			s := Month{Month: m, RainPct: math.NaN(), PriceRatio: math.NaN(), ToTRatio: math.NaN()}
			d := m.AddDate(0, -c.DataLag, 0) // latest month whose data is in hand
			if name, months, ok := rainWindow(d, c.SeasonToDate); ok {
				if total, ok := sum(rain[reg], d.Year(), months); ok {
					clim := climatology(rain[reg], months, c.ClimateStart, c.ClimateEnd)
					if len(clim) >= 20 {
						s.Season, s.RainPct = name, percentile(clim, total)
						s.RainDeficit = s.RainPct < c.RainPct
					}
				}
			}
			m := d // market signals are computed for the latest month in hand
			// cereal price ratio: median across cereals of price / prior-24-month median
			var ratios []float64
			var cerealNow []float64
			for cm := range cereals {
				cur, ok := price[key{reg, cm, m}]
				if !ok {
					continue
				}
				cerealNow = append(cerealNow, cur)
				var hist []float64
				for b := 1; b <= 24; b++ {
					if v, ok := price[key{reg, cm, m.AddDate(0, -b, 0)}]; ok {
						hist = append(hist, v)
					}
				}
				if len(hist) >= 12 {
					ratios = append(ratios, cur/median(hist))
				}
			}
			if len(ratios) > 0 {
				s.PriceRatio = median(ratios)
				s.PriceSpike = s.PriceRatio >= c.PriceSpike
			}
			// terms of trade: goat price / cereal price
			tot := func(mm data.Month) (float64, bool) {
				g, ok := price[key{reg, "Livestock (Goat)", mm}]
				if !ok {
					return 0, false
				}
				var cs []float64
				for cm := range cereals {
					if v, ok := price[key{reg, cm, mm}]; ok {
						cs = append(cs, v)
					}
				}
				if len(cs) == 0 {
					return 0, false
				}
				return g / median(cs), true
			}
			if now, ok := tot(m); ok {
				var hist []float64
				for b := 1; b <= 24; b++ {
					if v, ok := tot(m.AddDate(0, -b, 0)); ok {
						hist = append(hist, v)
					}
				}
				if len(hist) >= 12 {
					s.ToTRatio = now / median(hist)
					s.ToTDrop = s.ToTRatio <= c.ToTDrop
				}
			}
			m = s.Month
			market := s.PriceSpike || s.ToTDrop
			switch {
			case s.RainDeficit && market:
				s.Level = Warning
			case s.RainDeficit || market:
				s.Level = Watch
			}
			out[reg] = append(out[reg], s)
		}
	}
	return out
}

// rainWindow returns the rainy-season months to score as of month d: the
// season so far if d is inside one (and seasonToDate is set), otherwise the
// last completed season. Months belong to d's year; Deyr scored in Jan-Mar
// refers to the previous year's Oct-Dec.
func rainWindow(d data.Month, seasonToDate bool) (string, []data.Month, bool) {
	for back := 0; back < 12; back++ {
		cand := d.AddDate(0, -back, 0)
		for _, s := range seasons {
			for i, mo := range s.months {
				if cand.Month() != mo {
					continue
				}
				last := i
				if back > 0 || !seasonToDate {
					if i != len(s.months)-1 {
						continue // not the end of a completed season
					}
				}
				var ms []data.Month
				for _, mm := range s.months[:last+1] {
					ms = append(ms, data.M(cand.Year(), mm))
				}
				name := s.name
				if last < len(s.months)-1 {
					name += " so far"
				}
				return name, ms, true
			}
		}
	}
	return "", nil, false
}

func sum(r map[data.Month]float64, _ int, ms []data.Month) (float64, bool) {
	t := 0.0
	for _, m := range ms {
		v, ok := r[m]
		if !ok {
			return 0, false
		}
		t += v
	}
	return t, true
}

// climatology returns the same calendar months' totals for each year y0..y1.
func climatology(r map[data.Month]float64, ms []data.Month, y0, y1 int) []float64 {
	var out []float64
	for y := y0; y <= y1; y++ {
		var shifted []data.Month
		for _, m := range ms {
			shifted = append(shifted, data.M(y, m.Month()))
		}
		if t, ok := sum(r, y, shifted); ok {
			out = append(out, t)
		}
	}
	return out
}

// percentile returns the fraction of climatology values below x.
func percentile(clim []float64, x float64) float64 {
	n := 0
	for _, v := range clim {
		if v < x {
			n++
		}
	}
	return float64(n) / float64(len(clim))
}

func median(v []float64) float64 {
	s := append([]float64(nil), v...)
	sort.Float64s(s)
	n := len(s)
	if n == 0 {
		return math.NaN()
	}
	if n%2 == 1 {
		return s[n/2]
	}
	return (s[n/2-1] + s[n/2]) / 2
}
