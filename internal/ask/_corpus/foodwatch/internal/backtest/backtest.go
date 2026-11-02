// Package backtest checks the signals against what actually happened.
//
// Ground truth comes from IPC analyses, not from the signals' own data. A
// region has a food-crisis EVENT at an IPC analysis when the share of its
// population in Phase 3+ (Crisis or worse) is at least 30% and has risen by
// at least 10 percentage points since the previous analysis.
//
// For each event we ask: was a WARNING active in the month before the
// analysis, using only data available then? If so, how long had it been
// continuously active (the lead time)? Precision asks the opposite: of the
// region-analyses where a WARNING was active, how many were events, or
// already at 30%+?
package backtest

import (
	"fmt"
	"math/rand/v2"
	"sort"
	"time"

	"github.com/Aravindh97/foodwatch/internal/data"
	"github.com/Aravindh97/foodwatch/internal/signals"
)

type Config struct {
	Share, Rise float64 // event: share >= Share and rise >= Rise
	MaxLead     int     // months to look back for the start of an alert run
}

func Default() Config { return Config{Share: 0.30, Rise: 0.10, MaxLead: 12} }

type Event struct {
	Region    string
	Analysis  data.Month
	From, To  float64 // Phase 3+ share before and at the event
	Flagged   bool
	LeadWeeks float64
	Reasons   string
}

type Result struct {
	Name                 string
	Events               []Event
	Flagged              int
	MedianLead, MinLead  float64
	AlertsAtAnalysis     int // region-analyses with an alert in the month before
	AlertsCorrect        int // ...that were events or already >= Share
	RegionAnalyses       int
	CrisisRegionAnalyses int // region-analyses at >= Share (events or not)
	// ChanceRecall is the recall expected from the same number of alerts
	// placed at random over the scored region-analyses, and PValue the
	// share of 10,000 random placements that did at least as well.
	ChanceRecall, PValue float64
}

// Rule decides from one month's signals whether to alert.
type Rule func(signals.Month) bool

func Run(name string, ipc []data.IPC, sig map[string][]signals.Month, c Config, rule Rule) Result {
	byRegion := map[string][]data.IPC{}
	for _, x := range ipc {
		byRegion[x.Region] = append(byRegion[x.Region], x)
	}
	idx := map[string]map[data.Month]signals.Month{}
	for reg, ms := range sig {
		idx[reg] = map[data.Month]signals.Month{}
		for _, m := range ms {
			idx[reg][m.Month] = m
		}
	}
	res := Result{Name: name}
	regions := make([]string, 0, len(byRegion))
	for r := range byRegion {
		regions = append(regions, r)
	}
	sort.Strings(regions)
	for _, reg := range regions {
		xs := byRegion[reg]
		s, ok := idx[reg]
		if !ok {
			continue // region has no market data
		}
		for i := 1; i < len(xs); i++ {
			prev, cur := xs[i-1], xs[i]
			res.RegionAnalyses++
			// Signals for the month before the analysis: data a forecaster
			// would have had when the analysis started.
			last := cur.Analysis.AddDate(0, -1, 0)
			alert := rule(s[last])
			isEvent := cur.Share3 >= c.Share && cur.Share3-prev.Share3 >= c.Rise
			if cur.Share3 >= c.Share {
				res.CrisisRegionAnalyses++
			}
			if alert {
				res.AlertsAtAnalysis++
				if cur.Share3 >= c.Share {
					res.AlertsCorrect++
				}
			}
			if !isEvent {
				continue
			}
			e := Event{Region: reg, Analysis: cur.Analysis, From: prev.Share3, To: cur.Share3, Flagged: alert}
			if alert {
				start := last
				for k := 1; k < c.MaxLead; k++ {
					m := last.AddDate(0, -k, 0)
					if !rule(s[m]) {
						break
					}
					start = m
				}
				// the alert for month `start` is available at the end of that month
				avail := start.AddDate(0, 1, 0)
				e.LeadWeeks = cur.Analysis.Sub(avail).Hours() / 24 / 7
				e.Reasons = s[last].Reasons()
				res.Flagged++
			}
			res.Events = append(res.Events, e)
		}
	}
	res.ChanceRecall, res.PValue = chance(res.RegionAnalyses, res.AlertsAtAnalysis, len(res.Events), res.Flagged)
	var leads []float64
	for _, e := range res.Events {
		if e.Flagged {
			leads = append(leads, e.LeadWeeks)
		}
	}
	sort.Float64s(leads)
	if len(leads) > 0 {
		res.MedianLead, res.MinLead = leads[len(leads)/2], leads[0]
	}
	return res
}

// chance shuffles k alerts over n region-analyses containing e events.
func chance(n, k, e, flagged int) (float64, float64) {
	if n == 0 || e == 0 {
		return 0, 1
	}
	rng := rand.New(rand.NewPCG(42, 42))
	slots := make([]int, n)
	for i := range slots {
		slots[i] = i
	}
	const trials = 10000
	atLeast, total := 0, 0
	for t := 0; t < trials; t++ {
		rng.Shuffle(n, func(i, j int) { slots[i], slots[j] = slots[j], slots[i] })
		hits := 0
		for _, s := range slots[:k] {
			if s < e { // the first e slots stand for the events
				hits++
			}
		}
		total += hits
		if hits >= flagged {
			atLeast++
		}
	}
	return float64(total) / trials / float64(e), float64(atLeast) / trials
}

func (r Result) Line() string {
	prec := 0.0
	if r.AlertsAtAnalysis > 0 {
		prec = float64(r.AlertsCorrect) / float64(r.AlertsAtAnalysis)
	}
	return fmt.Sprintf("%-34s flagged %2d/%-2d events (chance %4.1f, p=%.3f)  median lead %4.1f weeks   alerts %3d, precision %.2f",
		r.Name, r.Flagged, len(r.Events), r.ChanceRecall*float64(len(r.Events)), r.PValue, r.MedianLead, r.AlertsAtAnalysis, prec)
}

// Monthly is a helper for printing a timeline.
func Monthly(ms []signals.Month, from, to time.Time) []signals.Month {
	var out []signals.Month
	for _, m := range ms {
		if !m.Month.Before(from) && !m.Month.After(to) {
			out = append(out, m)
		}
	}
	return out
}
