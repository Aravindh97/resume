// Command orbit screens satellites for close approaches.
//
//	orbit screen -tle catalog.tle -start 2026-11-02T05:00:00Z -hours 24 -out events.json
//	orbit screen -tle catalog.tle -mine 25544,48274 ...       # only pairs with your satellites
//	orbit socrates -html table.html -out socrates.csv          # parse a SOCRATES result page
//	orbit eval -events events.json -socrates socrates.csv -tle catalog.tle
package main

import (
	"encoding/csv"
	"encoding/json"
	"flag"
	"fmt"
	"html"
	"math"
	"os"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/Aravindh97/orbit/internal/screen"
)

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: orbit screen|socrates|eval")
		os.Exit(2)
	}
	var err error
	switch os.Args[1] {
	case "screen":
		err = screenCmd(os.Args[2:])
	case "socrates":
		err = socratesCmd(os.Args[2:])
	case "eval":
		err = evalCmd(os.Args[2:])
	default:
		err = fmt.Errorf("unknown command %q", os.Args[1])
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func screenCmd(args []string) error {
	fs := flag.NewFlagSet("screen", flag.ExitOnError)
	tle := fs.String("tle", "testdata/catalog.tle", "3-line element file")
	startS := fs.String("start", "", "UTC start (RFC3339); default now, rounded down to the hour")
	hours := fs.Float64("hours", 24, "window length")
	step := fs.Duration("step", 10*time.Second, "sample interval")
	thr := fs.Float64("threshold", 5, "report approaches closer than this (km)")
	workers := fs.Int("workers", runtime.NumCPU(), "parallel workers")
	mine := fs.String("mine", "", "comma-separated NORAD IDs: only screen pairs involving these")
	out := fs.String("out", "events.json", "output file")
	fs.Parse(args)
	f, err := os.Open(*tle)
	if err != nil {
		return err
	}
	objs, skipped, err := screen.LoadTLE(f)
	f.Close()
	if err != nil {
		return err
	}
	start := time.Now().UTC().Truncate(time.Hour)
	if *startS != "" {
		if start, err = time.Parse(time.RFC3339, *startS); err != nil {
			return err
		}
	}
	c := screen.Config{Start: start, End: start.Add(time.Duration(*hours * float64(time.Hour))), Step: *step, Threshold: *thr, Workers: *workers}
	if *mine != "" {
		c.Primaries = map[int]bool{}
		for _, s := range strings.Split(*mine, ",") {
			id, err := strconv.Atoi(strings.TrimSpace(s))
			if err != nil {
				return fmt.Errorf("bad NORAD id %q", s)
			}
			c.Primaries[id] = true
		}
	}
	fmt.Fprintf(os.Stderr, "screening %d objects (%d records skipped) %s .. %s, step %s, threshold %.1f km, %d workers\n",
		len(objs), skipped, c.Start.Format(time.RFC3339), c.End.Format(time.RFC3339), c.Step, c.Threshold, c.Workers)
	evs, st := screen.Run(objs, c)
	names := map[int]string{}
	for _, o := range objs {
		names[o.ID] = o.Name
	}
	type named struct {
		screen.Event
		NameA string `json:"name_a"`
		NameB string `json:"name_b"`
	}
	ns := make([]named, len(evs))
	for i, e := range evs {
		ns[i] = named{e, names[e.A], names[e.B]}
	}
	b, _ := json.MarshalIndent(map[string]any{"config": map[string]any{"start": c.Start, "end": c.End, "step_s": c.Step.Seconds(),
		"threshold_km": c.Threshold, "objects": len(objs), "tle_file": *tle}, "stats": st, "events": ns}, "", " ")
	if err := os.WriteFile(*out, b, 0o644); err != nil {
		return err
	}
	fmt.Printf("objects=%d steps=%d took=%s  propagations=%d (%.1f M/s)  pairs checked=%d (%.4f%% of brute force)  refined=%d  sgp4 errors=%d\n",
		st.Objects, st.Steps, st.Took.Round(time.Millisecond), st.Propagations, float64(st.Propagations)/st.Took.Seconds()/1e6,
		st.PairsChecked, 100*float64(st.PairsChecked)/st.BruteForcePairs, st.Refinements, st.PropErrors)
	fmt.Printf("object-steps screened per second: %.0f\n", float64(st.Objects)*float64(st.Steps)/st.Took.Seconds())
	fmt.Printf("%d close approaches under %.1f km written to %s\n", len(evs), c.Threshold, *out)
	for i, e := range ns {
		if i == 10 {
			break
		}
		co := ""
		if e.CoOrbiting {
			co = " (co-orbiting)"
		}
		fmt.Printf("  %s  %6.3f km  %5.2f km/s  %d %s  x  %d %s%s\n", e.TCA.Format("2006-01-02 15:04:05.000"), e.MinRange, e.RelSpeed, e.A, e.NameA, e.B, e.NameB, co)
	}
	return nil
}

// socratesCmd parses a CelesTrak SOCRATES result table (two HTML rows per
// conjunction) into CSV: a,b,tca,min_range_km,rel_speed_kms.
func socratesCmd(args []string) error {
	fs := flag.NewFlagSet("socrates", flag.ExitOnError)
	in := fs.String("html", "", "saved table-socrates.php page")
	out := fs.String("out", "socrates.csv", "output CSV")
	fs.Parse(args)
	b, err := os.ReadFile(*in)
	if err != nil {
		return err
	}
	rowRe := regexp.MustCompile(`(?s)<tr[^>]*>(.*?)</tr>`)
	cellRe := regexp.MustCompile(`(?s)<t[dh][^>]*>(.*?)</t[dh]>`)
	tagRe := regexp.MustCompile(`<[^>]+>`)
	var rows [][]string
	for _, r := range rowRe.FindAllStringSubmatch(string(b), -1) {
		var cells []string
		for _, c := range cellRe.FindAllStringSubmatch(r[1], -1) {
			cells = append(cells, strings.TrimSpace(html.UnescapeString(tagRe.ReplaceAllString(c[1], ""))))
		}
		rows = append(rows, cells)
	}
	f, err := os.Create(*out)
	if err != nil {
		return err
	}
	defer f.Close()
	w := csv.NewWriter(f)
	w.Write([]string{"a", "b", "tca", "min_range_km", "rel_speed_kms", "days_since_epoch_a", "days_since_epoch_b"})
	n := 0
	for i := 0; i+1 < len(rows); i++ {
		p, s := rows[i], rows[i+1]
		// primary row: GP Data | id | name | days | TCA | range | speed
		if len(p) < 7 || len(s) < 3 || !strings.HasPrefix(p[0], "GP") {
			continue
		}
		a, err1 := strconv.Atoi(p[1])
		bid, err2 := strconv.Atoi(s[1])
		if _, err := time.Parse("2006-01-02 15:04:05.000", p[4]); err1 != nil || err2 != nil || err != nil {
			continue
		}
		w.Write([]string{strconv.Itoa(a), strconv.Itoa(bid), p[4], p[5], p[6], p[3], s[3]})
		n++
		i++
	}
	w.Flush()
	fmt.Printf("parsed %d conjunctions into %s\n", n, *out)
	return w.Error()
}

type socRow struct {
	a, b           int
	tca            time.Time
	rng            float64
	speed          float64
	epochA, epochB time.Time // element-set epochs SOCRATES used
}

func evalCmd(args []string) error {
	fs := flag.NewFlagSet("eval", flag.ExitOnError)
	evPath := fs.String("events", "events.json", "screen output")
	socPath := fs.String("socrates", "testdata/socrates-minrange.csv", "parsed SOCRATES list")
	tle := fs.String("tle", "testdata/catalog.tle", "catalog used for screening")
	tol := fs.Duration("tca-tolerance", 2*time.Minute, "max TCA difference to count as the same encounter")
	fs.Parse(args)
	var res struct {
		Config struct {
			Start, End time.Time
		} `json:"config"`
		Events []screen.Event `json:"events"`
	}
	b, err := os.ReadFile(*evPath)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(b, &res); err != nil {
		return err
	}
	f, err := os.Open(*tle)
	if err != nil {
		return err
	}
	objs, _, _ := screen.LoadTLE(f)
	f.Close()
	have := map[int]bool{}
	epoch := map[int]time.Time{}
	byID := map[int]screen.Object{}
	for _, o := range objs {
		byID[o.ID] = o
		have[o.ID] = true
		epoch[o.ID] = o.Sat.Epoch()
	}
	// SOCRATES prints days-since-epoch to 3 decimals (±43 s), so element
	// sets are "the same" if epochs agree within 2 minutes.
	same := func(id int, e time.Time) bool {
		d := epoch[id].Sub(e)
		return d < 2*time.Minute && d > -2*time.Minute
	}
	var matched, matchedFound int
	sf, err := os.Open(*socPath)
	if err != nil {
		return err
	}
	recs, err := csv.NewReader(sf).ReadAll()
	sf.Close()
	if err != nil {
		return err
	}
	var soc []socRow
	for _, r := range recs[1:] {
		a, _ := strconv.Atoi(r[0])
		bb, _ := strconv.Atoi(r[1])
		t, _ := time.Parse("2006-01-02 15:04:05.000", r[2])
		rg, _ := strconv.ParseFloat(r[3], 64)
		sp, _ := strconv.ParseFloat(r[4], 64)
		da, _ := strconv.ParseFloat(r[5], 64)
		db, _ := strconv.ParseFloat(r[6], 64)
		ea := t.Add(-time.Duration(da * 86400 * float64(time.Second)))
		eb := t.Add(-time.Duration(db * 86400 * float64(time.Second)))
		if a > bb {
			a, bb, ea, eb = bb, a, eb, ea
		}
		soc = append(soc, socRow{a, bb, t, rg, sp, ea, eb})
	}
	byPair := map[[2]int][]screen.Event{}
	for _, e := range res.Events {
		byPair[[2]int{e.A, e.B}] = append(byPair[[2]int{e.A, e.B}], e)
	}
	var inWindow, covered, found int
	var dRange, dTCA, ourAtTCA []float64
	type miss struct {
		r      socRow
		reason string
	}
	var misses []miss
	maxRange := 0.0
	for _, s := range soc {
		maxRange = math.Max(maxRange, s.rng)
		if s.tca.Before(res.Config.Start) || !s.tca.Before(res.Config.End) {
			continue
		}
		inWindow++
		if !have[s.a] || !have[s.b] {
			continue
		}
		covered++
		_, dNear := screen.ClosestNear(byID[s.a], byID[s.b], s.tca, 2*time.Minute)
		ourAtTCA = append(ourAtTCA, dNear)
		var best *screen.Event
		for i, e := range byPair[[2]int{s.a, s.b}] {
			if d := e.TCA.Sub(s.tca); d < *tol && d > -*tol {
				if best == nil || math.Abs(float64(e.TCA.Sub(s.tca))) < math.Abs(float64(best.TCA.Sub(s.tca))) {
					best = &byPair[[2]int{s.a, s.b}][i]
				}
			}
		}
		sameSets := same(s.a, s.epochA) && same(s.b, s.epochB)
		if sameSets {
			matched++
		}
		if best == nil {
			reason := "not found"
			if len(byPair[[2]int{s.a, s.b}]) > 0 {
				reason = "pair found at another time"
			}
			misses = append(misses, miss{s, reason})
			continue
		}
		found++
		if sameSets {
			matchedFound++
		}
		dRange = append(dRange, math.Abs(best.MinRange-s.rng))
		dTCA = append(dTCA, math.Abs(best.TCA.Sub(s.tca).Seconds()))
	}
	med := func(x []float64) float64 {
		if len(x) == 0 {
			return 0
		}
		sort.Float64s(x)
		return x[len(x)/2]
	}
	p90 := func(x []float64) float64 {
		if len(x) == 0 {
			return 0
		}
		sort.Float64s(x)
		return x[int(float64(len(x))*0.9)]
	}
	fmt.Printf("SOCRATES list: %d conjunctions (range up to %.3f km); %d with TCA in our window %s .. %s\n",
		len(soc), maxRange, inWindow, res.Config.Start.Format(time.RFC3339), res.Config.End.Format(time.RFC3339))
	fmt.Printf("both objects in our catalog: %d of %d (the rest involve objects CelesTrak's public groups do not include)\n", covered, inWindow)
	fmt.Printf("RECALL: found %d of %d covered conjunctions (%.1f%%) within ±%s of SOCRATES' TCA\n", found, covered, 100*float64(found)/math.Max(1, float64(covered)), *tol)
	fmt.Printf("agreement on found ones: |min range diff| median %.3f km, p90 %.3f km; |TCA diff| median %.1f s, p90 %.1f s\n",
		med(dRange), p90(dRange), med(dTCA), p90(dTCA))
	fmt.Printf("RECALL ON IDENTICAL ELEMENT SETS (our TLE epoch = SOCRATES' for both objects): %d of %d (%.1f%%)\n",
		matchedFound, matched, 100*float64(matchedFound)/math.Max(1, float64(matched)))
	under := func(x []float64, km float64) int {
		n := 0
		for _, v := range x {
			if v < km {
				n++
			}
		}
		return n
	}
	fmt.Printf("OUR PREDICTION near SOCRATES' TCA (±2 min), same objects, our newer element sets:\n")
	fmt.Printf("  median %.1f km, p90 %.1f km; under 1 km: %d, under 5 km: %d, under 25 km: %d (of %d)\n",
		med(append([]float64(nil), ourAtTCA...)), p90(append([]float64(nil), ourAtTCA...)),
		under(ourAtTCA, 1), under(ourAtTCA, 5), under(ourAtTCA, 25), len(ourAtTCA))
	reasons := map[string]int{}
	for _, m := range misses {
		reasons[m.reason]++
	}
	fmt.Printf("misses by reason: %v\n", reasons)
	for i, m := range misses {
		if i == 8 {
			break
		}
		fmt.Printf("  missed %d x %d at %s, SOCRATES %.3f km\n", m.r.a, m.r.b, m.r.tca.Format(time.RFC3339), m.r.rng)
	}
	// Our events closer than the list's cut-off that SOCRATES does not list.
	socPairs := map[[2]int]bool{}
	for _, s := range soc {
		socPairs[[2]int{s.a, s.b}] = true
	}
	extra := 0
	for _, e := range res.Events {
		if e.MinRange < maxRange && !e.CoOrbiting && !socPairs[[2]int{e.A, e.B}] {
			extra++
		}
	}
	fmt.Printf("our approaches under %.3f km that are not in the SOCRATES list: %d (expected: newer element sets, or objects SOCRATES treats as one)\n", maxRange, extra)
	return nil
}
