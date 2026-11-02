// Package data loads the three public datasets aqcal uses:
//
//   - Sensor.Community SDS011 low-cost sensor readings (about every 2.5 min),
//     UTC timestamps
//   - Umweltbundesamt (German Environment Agency) reference station hourly
//     means. UBA timestamps are CET (UTC+1) all year, with no daylight
//     saving; cross-correlation with the sensors confirms the 1-hour shift
//     (see README).
//   - Open-Meteo hourly relative humidity for the city, UTC
package data

import (
	"compress/gzip"
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

type Reading struct {
	T      time.Time
	Sensor int
	PM25   float64
	PM10   float64
}

type Sensor struct {
	ID       int     `json:"sds"`
	Location int     `json:"loc"`
	Ref      string  `json:"ref"` // nearest reference station id
	RefName  string  `json:"ref_name"`
	DistKm   float64 `json:"dist"`
}

type Ref struct{ PM25, PM10 float64 }

func open(path string) (io.ReadCloser, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	if !strings.HasSuffix(path, ".gz") {
		return f, nil
	}
	gz, err := gzip.NewReader(f)
	if err != nil {
		f.Close()
		return nil, err
	}
	return struct {
		io.Reader
		io.Closer
	}{gz, f}, nil
}

// LoadReadings returns readings sorted by time. Rows that do not parse are
// counted, not fatal: real sensor feeds contain them.
func LoadReadings(path string) ([]Reading, int, error) {
	r, err := open(path)
	if err != nil {
		return nil, 0, err
	}
	defer r.Close()
	cr := csv.NewReader(r)
	cr.ReuseRecord = true
	if _, err := cr.Read(); err != nil {
		return nil, 0, err
	}
	var out []Reading
	bad := 0
	for {
		rec, err := cr.Read()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, bad, err
		}
		t, err1 := time.Parse("2006-01-02T15:04:05", rec[0])
		id, err2 := strconv.Atoi(rec[1])
		p10, err3 := strconv.ParseFloat(rec[2], 64)
		p25, err4 := strconv.ParseFloat(rec[3], 64)
		if err1 != nil || err2 != nil || err3 != nil || err4 != nil {
			bad++
			continue
		}
		out = append(out, Reading{T: t.UTC(), Sensor: id, PM25: p25, PM10: p10})
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].T.Before(out[j].T) })
	return out, bad, nil
}

// LoadReference returns station -> UTC hour start -> hourly mean.
func LoadReference(path string) (map[string]map[time.Time]Ref, error) {
	r, err := open(path)
	if err != nil {
		return nil, err
	}
	defer r.Close()
	recs, err := csv.NewReader(r).ReadAll()
	if err != nil {
		return nil, err
	}
	cet := time.FixedZone("CET", 3600)
	out := map[string]map[time.Time]Ref{}
	for _, rec := range recs[1:] {
		t, err := time.ParseInLocation("2006-01-02 15:04:05", rec[1], cet)
		if err != nil {
			return nil, fmt.Errorf("reference time %q: %w", rec[1], err)
		}
		p25, err := strconv.ParseFloat(rec[2], 64)
		if err != nil {
			continue // missing hour
		}
		p10, _ := strconv.ParseFloat(rec[3], 64)
		if out[rec[0]] == nil {
			out[rec[0]] = map[time.Time]Ref{}
		}
		out[rec[0]][t.UTC()] = Ref{p25, p10}
	}
	return out, nil
}

// LoadHumidity reads an Open-Meteo archive response.
func LoadHumidity(path string) (map[time.Time]float64, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var r struct {
		Hourly struct {
			Time []string  `json:"time"`
			RH   []float64 `json:"relative_humidity_2m"`
		} `json:"hourly"`
	}
	if err := json.Unmarshal(b, &r); err != nil {
		return nil, err
	}
	out := map[time.Time]float64{}
	for i, s := range r.Hourly.Time {
		t, err := time.Parse("2006-01-02T15:04", s)
		if err != nil {
			return nil, err
		}
		if i < len(r.Hourly.RH) {
			out[t.UTC()] = r.Hourly.RH[i]
		}
	}
	return out, nil
}

func LoadSensors(path string) ([]Sensor, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var s []Sensor
	return s, json.Unmarshal(b, &s)
}
