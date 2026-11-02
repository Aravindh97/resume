package stream

import (
	"context"
	"math"
	"testing"
	"time"

	"github.com/Aravindh97/aqcal/internal/calib"
	"github.com/Aravindh97/aqcal/internal/data"
)

func synth(sensors, hours int, mutate func(s int, t time.Time, v float64) (float64, bool)) []data.Reading {
	t0 := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	var rs []data.Reading
	for m := 0; m < hours*20; m++ { // every 3 minutes
		t := t0.Add(time.Duration(m) * 3 * time.Minute)
		for s := 1; s <= sensors; s++ {
			hour := m / 20
			// varies within the hour and from hour to hour, like real air
			v := 8 + 4*math.Sin(float64(hour)/3+float64(s)) + float64((m*7+s*13)%11)/5
			if mutate != nil {
				var keep bool
				if v, keep = mutate(s, t, v); !keep {
					continue
				}
			}
			rs = append(rs, data.Reading{T: t, Sensor: s, PM25: v})
		}
	}
	return rs
}

func run(t *testing.T, rs []data.Reading, n int) *Sink {
	var ss []data.Sensor
	models := map[int]calib.Model{}
	for s := 1; s <= n; s++ {
		ss = append(ss, data.Sensor{ID: s, Ref: "x"})
		models[s] = calib.Model{A: 0, B: 1}
	}
	ch := make(chan data.Reading, 64)
	sink := &Sink{}
	done := make(chan error)
	go func() { done <- Run(context.Background(), Config{Sensors: ss, Models: models, Shards: 3}, ch, sink) }()
	for _, r := range rs {
		ch <- r
	}
	close(ch)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	return sink
}

func kinds(s *Sink, sensor int) map[Kind]bool {
	out := map[Kind]bool{}
	for _, a := range s.Alerts {
		if a.Sensor == sensor {
			out[a.Kind] = true
		}
	}
	return out
}

func TestDetectsEachFault(t *testing.T) {
	at := func(t time.Time, from, to int) bool { h := t.Hour() + 24*(t.Day()-1); return h >= from && h < to }
	rs := synth(8, 30, func(s int, t time.Time, v float64) (float64, bool) {
		switch {
		case s == 1 && at(t, 10, 16):
			return 4.2, true // stuck
		case s == 2 && at(t, 10, 13):
			return 999.9, true // saturated
		case s == 3 && at(t, 10, 16):
			return 0, false // offline
		case s == 4 && at(t, 12, 13):
			return v*8 + 60, true // spike
		}
		return v, true
	})
	sink := run(t, rs, 8)
	for s, k := range map[int]Kind{1: Stuck, 2: Saturated, 3: Offline, 4: Spike} {
		if !kinds(sink, s)[k] {
			t.Errorf("sensor %d: no %s alert (got %v)", s, k, kinds(sink, s))
		}
	}
	for s := 5; s <= 8; s++ {
		if len(kinds(sink, s)) != 0 {
			t.Errorf("healthy sensor %d alerted: %v", s, sink.Alerts)
		}
	}
}

func TestEveryHourEvaluatedOnce(t *testing.T) {
	sink := run(t, synth(5, 10, nil), 5)
	if sink.Hours != 10 || sink.Values != 50 {
		t.Fatalf("hours=%d values=%d, want 10 and 50", sink.Hours, sink.Values)
	}
}
