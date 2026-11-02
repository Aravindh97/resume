// Package data loads the public datasets foodwatch uses, all for Somalia:
//
//   - WFP market prices (HDX "wfp-food-prices-for-somalia"): monthly retail
//     prices per market and commodity, 1995 onwards
//   - ERA5 reanalysis rainfall (via Open-Meteo) at each region's market
//     centroid, monthly totals 1991 onwards
//   - IPC acute food insecurity classifications (HDX "somalia-acute-food-
//     insecurity-country-data"): for each analysis, the share of each
//     region's population in Phase 3 (Crisis) or worse
//
// Region names differ between sources ("Juba Hoose" vs "Lower Juba",
// "Woqooyi galbeed" vs "Woqooyi Galbeed"); Region() maps them to one name.
package data

import (
	"encoding/csv"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"
)

var aliases = map[string]string{
	"juba hoose":      "Lower Juba",
	"lower juba":      "Lower Juba",
	"juba dhexe":      "Middle Juba",
	"middle juba":     "Middle Juba",
	"shabelle hoose":  "Lower Shabelle",
	"lower shabelle":  "Lower Shabelle",
	"shabelle dhexe":  "Middle Shabelle",
	"middle shabelle": "Middle Shabelle",
	"woqooyi galbeed": "Woqooyi Galbeed",
}

// Region returns the canonical region name.
func Region(s string) string {
	k := strings.ToLower(strings.TrimSpace(s))
	if v, ok := aliases[k]; ok {
		return v
	}
	if k == "" {
		return ""
	}
	return strings.ToUpper(k[:1]) + k[1:]
}

// Month is the first day of a month, UTC.
type Month = time.Time

func M(y int, m time.Month) Month { return time.Date(y, m, 1, 0, 0, 0, 0, time.UTC) }

type Price struct {
	Month     Month
	Region    string
	Market    string
	Commodity string
	Unit      string
	Price     float64 // local currency
}

func LoadPrices(path string) ([]Price, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return ReadPrices(f)
}

// ReadPrices parses the WFP CSV from any reader (a file, or bytes embedded
// in the browser build).
func ReadPrices(in io.Reader) ([]Price, error) {
	r := csv.NewReader(in)
	hdr, err := r.Read()
	if err != nil {
		return nil, err
	}
	col := map[string]int{}
	for i, h := range hdr {
		col[h] = i
	}
	var out []Price
	for {
		rec, err := r.Read()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, err
		}
		if strings.HasPrefix(rec[0], "#") || rec[col["admin1"]] == "" || rec[col["pricetype"]] != "Retail" {
			continue // HXL tag row, unlocated rows, wholesale prices
		}
		t, err := time.Parse("2006-01-02", rec[col["date"]])
		if err != nil {
			continue
		}
		p, err := strconv.ParseFloat(rec[col["price"]], 64)
		if err != nil || p <= 0 {
			continue
		}
		out = append(out, Price{Month: M(t.Year(), t.Month()), Region: Region(rec[col["admin1"]]), Market: rec[col["market"]],
			Commodity: rec[col["commodity"]], Unit: rec[col["unit"]], Price: p})
	}
	return out, nil
}

// LoadRain reads monthly rainfall totals (mm) per region: region ->
// "YYYYMM" -> mm. The file is built from Open-Meteo's ERA5 daily archive
// (see scripts/fetch.sh); months with fewer than 28 days of data are
// already dropped.
func LoadRain(path string) (map[string]map[Month]float64, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return ParseRain(b)
}

func ParseRain(b []byte) (map[string]map[Month]float64, error) {
	var raw map[string]map[string]float64
	if err := json.Unmarshal(b, &raw); err != nil {
		return nil, err
	}
	out := map[string]map[Month]float64{}
	for reg, ms := range raw {
		r := Region(reg)
		out[r] = map[Month]float64{}
		for k, v := range ms {
			if len(k) != 6 || v < 0 {
				continue
			}
			y, _ := strconv.Atoi(k[:4])
			m, _ := strconv.Atoi(k[4:])
			if m < 1 || m > 12 {
				continue
			}
			out[r][M(y, time.Month(m))] = v
		}
	}
	return out, nil
}

// IPC is one region in one analysis.
type IPC struct {
	Analysis Month // start of the "current" validity period
	Region   string
	Share3   float64 // share of population in Phase 3+
	People3  float64
}

func LoadIPC(path string) ([]IPC, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return ReadIPC(f)
}

func ReadIPC(in io.Reader) ([]IPC, error) {
	rs, err := csv.NewReader(in).ReadAll()
	if err != nil {
		return nil, err
	}
	col := map[string]int{}
	for i, h := range rs[0] {
		col[h] = i
	}
	seen := map[string]bool{}
	var out []IPC
	for _, rec := range rs[1:] {
		if rec[col["Validity period"]] != "current" || rec[col["Phase"]] != "3+" {
			continue
		}
		t, err := time.Parse("2006-01-02", rec[col["From"]])
		if err != nil {
			return nil, fmt.Errorf("IPC date %q: %w", rec[col["From"]], err)
		}
		reg := Region(rec[col["Level 1"]])
		k := t.Format("2006-01") + reg
		if seen[k] {
			continue // the file repeats some region-analysis rows
		}
		seen[k] = true
		share, _ := strconv.ParseFloat(rec[col["Percentage"]], 64)
		n, _ := strconv.ParseFloat(rec[col["Number"]], 64)
		out = append(out, IPC{Analysis: M(t.Year(), t.Month()), Region: reg, Share3: share, People3: n})
	}
	sort.Slice(out, func(i, j int) bool {
		if !out[i].Analysis.Equal(out[j].Analysis) {
			return out[i].Analysis.Before(out[j].Analysis)
		}
		return out[i].Region < out[j].Region
	})
	return out, nil
}
