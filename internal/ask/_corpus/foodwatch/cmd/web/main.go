//go:build js && wasm

// Command web runs foodwatch in the browser on the embedded public data.
package main

import (
	"bytes"
	_ "embed"
	"math"
	"net/url"
	"sort"

	"github.com/Aravindh97/foodwatch/internal/backtest"
	"github.com/Aravindh97/foodwatch/internal/data"
	"github.com/Aravindh97/foodwatch/internal/signals"
)

var (
	//go:embed embed/wfp.csv
	wfpCSV []byte
	//go:embed embed/ipc.csv
	ipcCSV []byte
	//go:embed embed/chirps.json
	chirpsJSON []byte
	//go:embed embed/era5.json
	era5JSON []byte
)

type world struct {
	prices []data.Price
	ipc    []data.IPC
	sig    map[string]map[string][]signals.Month // rain source -> region -> months
	latest data.Month
}

func load() (*world, error) {
	w := &world{sig: map[string]map[string][]signals.Month{}}
	var err error
	if w.prices, err = data.ReadPrices(bytes.NewReader(wfpCSV)); err != nil {
		return nil, err
	}
	if w.ipc, err = data.ReadIPC(bytes.NewReader(ipcCSV)); err != nil {
		return nil, err
	}
	c := signals.Default()
	for name, raw := range map[string][]byte{"chirps": chirpsJSON, "era5": era5JSON} {
		rain, err := data.ParseRain(raw)
		if err != nil {
			return nil, err
		}
		w.sig[name] = signals.Compute(w.prices, rain, c, data.M(2019, 1), data.M(2026, 10))
	}
	for _, p := range w.prices {
		if p.Month.After(w.latest) {
			w.latest = p.Month
		}
	}
	return w, nil
}

func num(x float64) any {
	if math.IsNaN(x) {
		return nil
	}
	return math.Round(x*1000) / 1000
}

type monthOut struct {
	Month   string `json:"month"`
	RainPct any    `json:"rain_pct"`
	Season  string `json:"season"`
	Price   any    `json:"price_ratio"`
	ToT     any    `json:"tot_ratio"`
	Level   string `json:"level"`
	Reasons string `json:"reasons"`
}

func out(m signals.Month) monthOut {
	return monthOut{m.Month.Format("2006-01"), num(m.RainPct), m.Season, num(m.PriceRatio), num(m.ToTRatio), m.Level.String(), m.Reasons()}
}

func source(q url.Values) string {
	if q.Get("rain") == "era5" {
		return "era5"
	}
	return "chirps"
}

func main() {
	w, err := load()
	serve(map[string]handler{
		"GET regions": func(q url.Values, _ []byte) resp {
			if err != nil {
				return fail(500, err.Error())
			}
			asOf := w.latest.AddDate(0, signals.Default().DataLag, 0)
			sig := w.sig[source(q)]
			var rows []map[string]any
			for reg, ms := range sig {
				for _, m := range ms {
					if m.Month.Equal(asOf) {
						rows = append(rows, map[string]any{"region": reg, "signal": out(m)})
					}
				}
			}
			sort.Slice(rows, func(i, j int) bool { return rows[i]["region"].(string) < rows[j]["region"].(string) })
			return ok(map[string]any{"as_of": asOf.Format("2006-01"), "latest_prices": w.latest.Format("2006-01"), "regions": rows})
		},
		"GET timeline": func(q url.Values, _ []byte) resp {
			if err != nil {
				return fail(500, err.Error())
			}
			reg := data.Region(q.Get("region"))
			ms, found := w.sig[source(q)][reg]
			if !found {
				return fail(404, "no such region")
			}
			var months []monthOut
			for _, m := range ms {
				months = append(months, out(m))
			}
			var ipc []map[string]any
			for _, x := range w.ipc {
				if x.Region == reg {
					ipc = append(ipc, map[string]any{"month": x.Analysis.Format("2006-01"), "share3": x.Share3})
				}
			}
			return ok(map[string]any{"region": reg, "months": months, "ipc": ipc})
		},
		"GET backtest": func(q url.Values, _ []byte) resp {
			if err != nil {
				return fail(500, err.Error())
			}
			sig := w.sig[source(q)]
			bc := backtest.Default()
			rules := []struct {
				name string
				r    backtest.Rule
			}{
				{"rain deficit", func(m signals.Month) bool { return m.RainDeficit }},
				{"any signal (WATCH)", func(m signals.Month) bool { return m.Level >= signals.Watch }},
				{"rain + market (WARNING)", func(m signals.Month) bool { return m.Level == signals.Warning }},
				{"cereal price spike only", func(m signals.Month) bool { return m.PriceSpike }},
				{"terms of trade only", func(m signals.Month) bool { return m.ToTDrop }},
			}
			var rs []map[string]any
			var events []map[string]any
			for i, ru := range rules {
				r := backtest.Run(ru.name, w.ipc, sig, bc, ru.r)
				prec := 0.0
				if r.AlertsAtAnalysis > 0 {
					prec = float64(r.AlertsCorrect) / float64(r.AlertsAtAnalysis)
				}
				rs = append(rs, map[string]any{"name": ru.name, "flagged": r.Flagged, "events": len(r.Events),
					"chance": r.ChanceRecall * float64(len(r.Events)), "p": r.PValue, "median_lead_weeks": r.MedianLead,
					"alerts": r.AlertsAtAnalysis, "precision": prec, "chance_precision": float64(r.CrisisRegionAnalyses) / float64(max(r.RegionAnalyses, 1))})
				if i == 0 {
					for _, e := range r.Events {
						events = append(events, map[string]any{"month": e.Analysis.Format("2006-01"), "region": e.Region,
							"from": e.From, "to": e.To, "flagged": e.Flagged, "lead_weeks": e.LeadWeeks, "reasons": e.Reasons})
					}
				}
			}
			return ok(map[string]any{"rules": rs, "events": events})
		},
	})
}
