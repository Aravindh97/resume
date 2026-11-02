// Command foodwatch runs the food-stress signals and backtests them
// against IPC classifications.
//
//	foodwatch backtest                 # events, recall, lead time, precision, baselines
//	foodwatch timeline -region Bay     # month-by-month signals for one region
//	foodwatch now                      # current alert level for every region
package main

import (
	"flag"
	"fmt"
	"math"
	"os"
	"sort"

	"github.com/Aravindh97/foodwatch/internal/backtest"
	"github.com/Aravindh97/foodwatch/internal/data"
	"github.com/Aravindh97/foodwatch/internal/signals"
)

type inputs struct {
	prices []data.Price
	rain   map[string]map[data.Month]float64
	ipc    []data.IPC
}

func load(dir, rain string) (*inputs, error) {
	in := &inputs{}
	var err error
	if in.prices, err = data.LoadPrices(dir + "/wfp_food_prices_som.csv"); err != nil {
		return nil, err
	}
	files := map[string]string{"chirps": "/chirps_monthly_rain_mm.json", "era5": "/era5_monthly_rain_mm.json"}
	f, ok := files[rain]
	if !ok {
		return nil, fmt.Errorf("-rain must be chirps or era5")
	}
	if in.rain, err = data.LoadRain(dir + f); err != nil {
		return nil, err
	}
	if in.ipc, err = data.LoadIPC(dir + "/ipc_som_level1_long.csv"); err != nil {
		return nil, err
	}
	return in, nil
}

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: foodwatch backtest|timeline|now")
		os.Exit(2)
	}
	fs := flag.NewFlagSet(os.Args[1], flag.ExitOnError)
	dir := fs.String("data", "testdata", "data directory")
	region := fs.String("region", "Bay", "region for timeline")
	rain := fs.String("rain", "chirps", "rainfall source: chirps (CHIRPS v2.0, the FEWS NET standard) or era5")
	fs.Parse(os.Args[2:])
	in, err := load(*dir, *rain)
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
	c := signals.Default()
	sig := signals.Compute(in.prices, in.rain, c, data.M(2019, 1), data.M(2026, 10))
	switch os.Args[1] {
	case "backtest":
		rainName = *rain
		report(in, sig, c)
	case "timeline":
		for _, m := range sig[data.Region(*region)] {
			fmt.Printf("%s  rain %-14s price %-6s ToT %-6s  %-7s %s\n", m.Month.Format("2006-01"), fmtRain(m), fmtR(m.PriceRatio), fmtR(m.ToTRatio), m.Level, m.Reasons())
		}
	case "now":
		// as of the month after the latest month with any price data
		var latest data.Month
		for _, p := range in.prices {
			if p.Month.After(latest) {
				latest = p.Month
			}
		}
		asOf := latest.AddDate(0, c.DataLag, 0)
		fmt.Printf("signals as of %s (latest WFP prices: %s)\n", asOf.Format("2006-01"), latest.Format("2006-01"))
		regs := make([]string, 0, len(sig))
		for r := range sig {
			regs = append(regs, r)
		}
		sort.Strings(regs)
		for _, r := range regs {
			ms := sig[r]
			last := ms[len(ms)-1]
			for _, m := range ms {
				if m.Month.Equal(asOf) {
					last = m
				}
			}
			note := ""
			if math.IsNaN(last.PriceRatio) {
				note = "  (no recent cereal prices: market signals unavailable)"
			}
			fmt.Printf("%-16s %s %-7s %s%s\n", r, last.Month.Format("2006-01"), last.Level, last.Reasons(), note)
		}
	}
}

func fmtR(x float64) string {
	if math.IsNaN(x) {
		return "-"
	}
	return fmt.Sprintf("%.2f", x)
}

func fmtRain(m signals.Month) string {
	if math.IsNaN(m.RainPct) {
		return "-"
	}
	return fmt.Sprintf("%s p%02.0f", m.Season, m.RainPct*100)
}

var rainName string

func report(in *inputs, sig map[string][]signals.Month, c signals.Config) {
	bc := backtest.Default()
	analyses := map[string]bool{}
	for _, x := range in.ipc {
		analyses[x.Analysis.Format("2006-01")] = true
	}
	fmt.Printf("IPC analyses: %d (%s), %d regions with market data, rainfall: %s\n", len(analyses), keys(analyses), len(sig), rainName)
	fmt.Printf("event: a region's Phase 3+ share reaches %.0f%% and rose %.0f points since the previous analysis\n",
		bc.Share*100, bc.Rise*100)
	fmt.Printf("signals use data up to the month before each analysis; rain climatology %d-%d (before every analysis)\n\n", c.ClimateStart, c.ClimateEnd)

	rules := []struct {
		name string
		r    backtest.Rule
	}{
		{"WARNING (rain deficit + market)", func(m signals.Month) bool { return m.Level == signals.Warning }},
		{"WATCH or WARNING (any signal)", func(m signals.Month) bool { return m.Level >= signals.Watch }},
		{"baseline: rain deficit only", func(m signals.Month) bool { return m.RainDeficit }},
		{"baseline: cereal price spike only", func(m signals.Month) bool { return m.PriceSpike }},
		{"baseline: terms of trade only", func(m signals.Month) bool { return m.ToTDrop }},
	}
	var main backtest.Result
	for i, ru := range rules {
		r := backtest.Run(ru.name, in.ipc, sig, bc, ru.r)
		if i == 0 {
			main = r
		}
		fmt.Println(r.Line())
	}
	fmt.Printf("\nregion-analyses scored: %d; at crisis level (>= %.0f%%): %d, so precision by chance is %.2f\n",
		main.RegionAnalyses, bc.Share*100, main.CrisisRegionAnalyses, float64(main.CrisisRegionAnalyses)/float64(main.RegionAnalyses))
	rainOnly := backtest.Run("rain", in.ipc, sig, bc, func(m signals.Month) bool { return m.RainDeficit })
	fmt.Println("\nby episode (rain deficit signal):")
	type ep struct {
		name     string
		from, to string
	}
	for _, e := range []ep{{"2020-23 Horn of Africa drought", "2021-01", "2023-12"}, {"2025 Deyr failure", "2025-01", "2026-12"}} {
		n, f := 0, 0
		var leads []float64
		for _, ev := range rainOnly.Events {
			k := ev.Analysis.Format("2006-01")
			if k < e.from || k > e.to {
				continue
			}
			n++
			if ev.Flagged {
				f++
				leads = append(leads, ev.LeadWeeks)
			}
		}
		sort.Float64s(leads)
		med := 0.0
		if len(leads) > 0 {
			med = leads[len(leads)/2]
		}
		fmt.Printf("  %-32s %2d of %2d region crisis onsets flagged, median %.1f weeks before the IPC analysis\n", e.name, f, n, med)
	}
	fmt.Println("\nevents, and the rain-deficit signal the month before:")
	for _, e := range rainOnly.Events {
		lead := "MISSED"
		if e.Flagged {
			lead = fmt.Sprintf("%4.1f weeks early", e.LeadWeeks)
		}
		fmt.Printf("  %s %-16s %3.0f%% -> %3.0f%%   %-17s %s\n", e.Analysis.Format("2006-01"), e.Region, e.From*100, e.To*100, lead, e.Reasons)
	}
}

func keys(m map[string]bool) string {
	var s []string
	for k := range m {
		s = append(s, k)
	}
	sort.Strings(s)
	return s[0] + " .. " + s[len(s)-1]
}
