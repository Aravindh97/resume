package signals

import (
	"math"
	"testing"
	"time"

	"github.com/Aravindh97/foodwatch/internal/data"
)

func rainSeries(f func(y int, m time.Month) float64) map[data.Month]float64 {
	r := map[data.Month]float64{}
	for y := 1991; y <= 2023; y++ {
		for m := time.January; m <= time.December; m++ {
			r[data.M(y, m)] = f(y, m)
		}
	}
	return r
}

func TestRainWindow(t *testing.T) {
	for _, c := range []struct {
		d       data.Month
		std     bool
		name    string
		n       int
		lastMon time.Month
	}{
		{data.M(2022, 2), true, "Deyr", 3, time.December},
		{data.M(2022, 5), true, "Gu so far", 2, time.May},
		{data.M(2022, 5), false, "Deyr", 3, time.December},
		{data.M(2022, 6), true, "Gu", 3, time.June},
		{data.M(2022, 10), true, "Deyr so far", 1, time.October},
	} {
		name, ms, ok := rainWindow(c.d, c.std)
		if !ok || name != c.name || len(ms) != c.n || ms[len(ms)-1].Month() != c.lastMon {
			t.Errorf("%s std=%v: got %s %v", c.d.Format("2006-01"), c.std, name, ms)
		}
	}
	r := rainSeries(func(y int, m time.Month) float64 { return float64(y - 1990) })
	clim := climatology(r, []data.Month{data.M(2022, 4), data.M(2022, 5)}, 1991, 2020)
	if len(clim) != 30 || percentile(clim, 0) != 0 {
		t.Fatalf("climatology %v", clim)
	}
}

func TestLevels(t *testing.T) {
	// A region where Deyr 2021 fails and sorghum doubles.
	rain := rainSeries(func(y int, m time.Month) float64 {
		if y == 2021 && m >= 10 {
			return 1
		}
		return 50 + float64(y%7)
	})
	var ps []data.Price
	for m := data.M(2019, 1); m.Before(data.M(2023, 1)); m = m.AddDate(0, 1, 0) {
		p := 1000.0
		if !m.Before(data.M(2022, 1)) {
			p = 2200
		}
		ps = append(ps, data.Price{Month: m, Region: "Bay", Commodity: "Sorghum (red)", Price: p})
	}
	c := Default()
	c.DataLag = 0
	sig := Compute(ps, map[string]map[data.Month]float64{"Bay": rain}, c, data.M(2021, 9), data.M(2022, 3))
	got := map[string]Level{}
	for _, m := range sig["Bay"] {
		got[m.Month.Format("2006-01")] = m.Level
	}
	// Sep: no season running, last Gu was normal. Oct: Deyr failing so far.
	// Jan: prices doubled on top of the failed season.
	if got["2021-09"] != None || got["2021-10"] != Watch || got["2022-01"] != Warning {
		t.Fatalf("levels %v", got)
	}
	// With a one-month data lag, October's failure is only known in November.
	c.DataLag = 1
	sig = Compute(ps, map[string]map[data.Month]float64{"Bay": rain}, c, data.M(2021, 10), data.M(2021, 11))
	if sig["Bay"][0].Level != None || sig["Bay"][1].Level != Watch {
		t.Fatalf("lagged levels %v %v", sig["Bay"][0].Level, sig["Bay"][1].Level)
	}
	if r := sig["Bay"][len(sig["Bay"])-1].Reasons(); r == "" {
		t.Fatal("warning without reasons")
	}
}

func TestMissingDataIsNotAnAlert(t *testing.T) {
	sig := Compute([]data.Price{{Month: data.M(2022, 1), Region: "Gedo", Commodity: "Maize (white)", Price: 5000}},
		nil, Default(), data.M(2022, 1), data.M(2022, 1))
	m := sig["Gedo"][0]
	if m.Level != None || !math.IsNaN(m.PriceRatio) || !math.IsNaN(m.RainPct) {
		t.Fatalf("%+v", m)
	}
}

func TestRegionAliases(t *testing.T) {
	for in, want := range map[string]string{"Juba Hoose": "Lower Juba", "Woqooyi galbeed": "Woqooyi Galbeed", " bay ": "Bay", "Shabelle Dhexe": "Middle Shabelle"} {
		if got := data.Region(in); got != want {
			t.Errorf("%q -> %q", in, got)
		}
	}
}
