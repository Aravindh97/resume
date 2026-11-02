// Package stream is the live pipeline:
//
//	source ──► aggregators (sharded by sensor) ──► detector/calibrator ──► sink
//	   └─ watermarks: "no reading older than T will arrive" ─┘
//
// Stages are goroutines joined by bounded channels, so a slow stage slows
// the source instead of growing memory. Readings for one sensor always go to
// the same aggregator shard, which keeps them in order.
//
// The detector closes an hour once the watermark passes its end plus a
// grace period, then, for every sensor:
//
//	saturated  most readings at the SDS011 maximum (999.9): sensor fault
//	stuck      identical median for 4+ hours, or zero spread with many
//	           readings: frozen firmware or a blocked fan
//	offline    no readings for 3+ hours
//	spike      calibrated value far above the median of the other sensors
//	           in the same hour (local event or fault: a human checks)
//	drift      the sensor's raw gain against its reference station over the
//	           last 72 hours has moved away from its gain in
//	           training by more than 1.5x for 24 hours in a row, after
//	           dividing by the typical change across all sensors (so a
//	           city-wide change in aerosol or weather, which moves every
//	           sensor together, is not called drift)
//
// and publishes the calibrated value unless the sensor is faulty.
package stream

import (
	"context"
	"fmt"
	"math"
	"sort"
	"sync"
	"time"

	"github.com/Aravindh97/aqcal/internal/calib"
	"github.com/Aravindh97/aqcal/internal/data"
)

type Kind string

const (
	Saturated Kind = "saturated"
	Stuck     Kind = "stuck"
	Offline   Kind = "offline"
	Spike     Kind = "spike"
	Drift     Kind = "drift"
)

type Alert struct {
	Sensor int       `json:"sensor"`
	Kind   Kind      `json:"kind"`
	Hour   time.Time `json:"hour"`
	Detail string    `json:"detail"`
}

type Value struct {
	Sensor     int       `json:"sensor"`
	Hour       time.Time `json:"hour"`
	Raw        float64   `json:"raw_pm25"`
	Calibrated float64   `json:"pm25"`
	Trusted    bool      `json:"trusted"`
}

type Config struct {
	Sensors    []data.Sensor
	Models     map[int]calib.Model // sensors without a model publish raw, untrusted
	Humidity   map[time.Time]float64
	Reference  map[string]map[time.Time]data.Ref
	Shards     int
	Grace      time.Duration // how long to wait for late readings
	ChannelCap int
}

type Sink struct {
	mu     sync.Mutex
	Alerts []Alert
	Latest map[int]Value
	Values int
	Hours  int
}

func (s *Sink) snapshot() ([]Alert, map[int]Value) {
	s.mu.Lock()
	defer s.mu.Unlock()
	l := make(map[int]Value, len(s.Latest))
	for k, v := range s.Latest {
		l[k] = v
	}
	return append([]Alert(nil), s.Alerts...), l
}

// Snapshot returns a copy of current alerts and latest values (for the API).
func (s *Sink) Snapshot() ([]Alert, map[int]Value) { return s.snapshot() }

type msg struct {
	h         *calib.Hourly
	watermark time.Time
}

// Run consumes readings (in time order) until the channel closes.
func Run(ctx context.Context, c Config, in <-chan data.Reading, sink *Sink) error {
	if c.Shards <= 0 {
		c.Shards = 4
	}
	if c.Grace == 0 {
		c.Grace = 15 * time.Minute
	}
	if c.ChannelCap == 0 {
		c.ChannelCap = 1024
	}
	if sink.Latest == nil {
		sink.Latest = map[int]Value{}
	}
	shardIn := make([]chan msg, c.Shards)
	toDet := make(chan msg, c.ChannelCap)
	type rmsg struct {
		r  data.Reading
		wm time.Time
	}
	raw := make([]chan rmsg, c.Shards)
	var wg sync.WaitGroup
	for i := range raw {
		raw[i] = make(chan rmsg, c.ChannelCap)
		shardIn[i] = nil
		wg.Add(1)
		go func(ch <-chan rmsg) {
			defer wg.Done()
			type buf struct {
				hour time.Time
				v    []float64
				sat  int
			}
			cur := map[int]*buf{}
			emit := func(s int, b *buf) {
				h := calib.HourOf(s, b.hour, b.v, b.sat)
				toDet <- msg{h: &h}
			}
			for m := range ch {
				if !m.wm.IsZero() {
					// Close hours that ended before the watermark.
					for s, b := range cur {
						if !b.hour.Add(time.Hour).After(m.wm) {
							emit(s, b)
							delete(cur, s)
						}
					}
					toDet <- msg{watermark: m.wm}
					continue
				}
				r := m.r
				h := r.T.Truncate(time.Hour)
				b := cur[r.Sensor]
				if b != nil && h.After(b.hour) {
					emit(r.Sensor, b)
					b = nil
				}
				if b == nil {
					b = &buf{hour: h}
					cur[r.Sensor] = b
				}
				if h.Before(b.hour) {
					continue // later than the grace period: dropped
				}
				if r.PM25 >= calib.Saturation {
					b.sat++
				} else {
					b.v = append(b.v, r.PM25)
				}
			}
			for s, b := range cur {
				emit(s, b)
			}
		}(raw[i])
	}
	det := newDetector(c, sink, c.Shards)
	detDone := make(chan struct{})
	go func() {
		defer close(detDone)
		for m := range toDet {
			det.handle(m)
		}
		det.flush()
	}()

	// Source loop: route readings to shards, inject a watermark every
	// simulated 5 minutes.
	var lastWM time.Time
	for r := range in {
		if err := ctx.Err(); err != nil {
			break
		}
		raw[r.Sensor%c.Shards] <- rmsg{r: r}
		if wm := r.T.Add(-c.Grace).Truncate(5 * time.Minute); wm.After(lastWM) {
			lastWM = wm
			for _, ch := range raw {
				ch <- rmsg{wm: wm}
			}
		}
	}
	for _, ch := range raw {
		close(ch)
	}
	wg.Wait()
	close(toDet)
	<-detDone
	return ctx.Err()
}

type sensorState struct {
	lastSeen   time.Time
	offline    bool
	sameRun    int
	lastMedian float64
	zeroSpread int
	pairs      []calib.Pair // raw vs reference, last 72 h (for drift)
	drifting   bool
	outside    int // consecutive hours outside the drift band
	alertedAt  map[Kind]time.Time
}

type detector struct {
	c       Config
	sink    *Sink
	shards  int
	wmSeen  map[time.Time]int
	pending map[time.Time]map[int]calib.Hourly
	closed  time.Time
	state   map[int]*sensorState
	sensors map[int]data.Sensor
}

func newDetector(c Config, sink *Sink, shards int) *detector {
	d := &detector{c: c, sink: sink, shards: shards, wmSeen: map[time.Time]int{}, pending: map[time.Time]map[int]calib.Hourly{},
		state: map[int]*sensorState{}, sensors: map[int]data.Sensor{}}
	for _, s := range c.Sensors {
		d.sensors[s.ID] = s
		d.state[s.ID] = &sensorState{alertedAt: map[Kind]time.Time{}}
	}
	return d
}

func (d *detector) handle(m msg) {
	if m.h != nil {
		h := *m.h
		if !h.Hour.After(d.closed) && !d.closed.IsZero() {
			return // arrived after its hour was evaluated
		}
		if d.pending[h.Hour] == nil {
			d.pending[h.Hour] = map[int]calib.Hourly{}
		}
		d.pending[h.Hour][h.Sensor] = h
		return
	}
	// A watermark counts once every shard has passed it.
	d.wmSeen[m.watermark]++
	if d.wmSeen[m.watermark] < d.shards {
		return
	}
	delete(d.wmSeen, m.watermark)
	var ready []time.Time
	for h := range d.pending {
		if !h.Add(time.Hour).After(m.watermark) {
			ready = append(ready, h)
		}
	}
	sort.Slice(ready, func(i, j int) bool { return ready[i].Before(ready[j]) })
	for _, h := range ready {
		d.evaluate(h, d.pending[h])
		delete(d.pending, h)
	}
	// Offline: hours with nothing at all from a sensor.
	for id, st := range d.state {
		if !st.lastSeen.IsZero() && !st.offline && m.watermark.Sub(st.lastSeen) >= 3*time.Hour {
			st.offline = true
			d.alert(id, Offline, m.watermark.Truncate(time.Hour), fmt.Sprintf("no readings since %s", st.lastSeen.Format("01-02 15:04")))
		}
	}
}

func (d *detector) flush() {
	var hs []time.Time
	for h := range d.pending {
		hs = append(hs, h)
	}
	sort.Slice(hs, func(i, j int) bool { return hs[i].Before(hs[j]) })
	for _, h := range hs {
		d.evaluate(h, d.pending[h])
	}
}

func (d *detector) alert(sensor int, k Kind, hour time.Time, detail string) {
	st := d.state[sensor]
	// One alert per kind per sensor per 12 hours, so a long fault is one
	// ticket, not seventy.
	if last, ok := st.alertedAt[k]; ok && hour.Sub(last) < 12*time.Hour {
		return
	}
	st.alertedAt[k] = hour
	d.sink.mu.Lock()
	d.sink.Alerts = append(d.sink.Alerts, Alert{sensor, k, hour, detail})
	d.sink.mu.Unlock()
}

func (d *detector) evaluate(hour time.Time, hs map[int]calib.Hourly) {
	if hour.After(d.closed) {
		d.closed = hour
	}
	rh := d.c.Humidity[hour]
	cal := map[int]float64{}
	faulty := map[int]bool{}
	for id, h := range hs {
		st := d.state[id]
		if st == nil {
			continue
		}
		st.lastSeen, st.offline = hour.Add(time.Hour), false
		total := h.N + h.Saturated
		if total > 0 && h.Saturated*2 > total {
			d.alert(id, Saturated, hour, fmt.Sprintf("%d of %d readings at 999.9", h.Saturated, total))
			faulty[id] = true
			continue
		}
		if h.N == 0 {
			continue
		}
		if h.N >= 10 && h.Spread == 0 {
			st.zeroSpread++
		} else {
			st.zeroSpread = 0
		}
		// Below ~2 µg/m³ the SDS011 reports a few quantised values, so a
		// repeated median there is clean air, not a frozen sensor.
		if math.Abs(h.PM25-st.lastMedian) < 1e-9 && h.PM25 >= 2 {
			st.sameRun++
		} else {
			st.sameRun = 0
		}
		st.lastMedian = h.PM25
		if st.sameRun >= 3 || st.zeroSpread >= 2 {
			d.alert(id, Stuck, hour, fmt.Sprintf("value %.1f unchanged", h.PM25))
			faulty[id] = true
		}
		v := h.PM25
		if m, ok := d.c.Models[id]; ok {
			v = m.Apply(h.PM25, rh)
		}
		cal[id] = v
		if ref, ok := d.c.Reference[d.sensors[id].Ref][hour]; ok {
			st.pairs = append(st.pairs, calib.Pair{Raw: h.PM25, Ref: ref.PM25})
			if len(st.pairs) > 72 {
				st.pairs = st.pairs[len(st.pairs)-72:]
			}
		}
	}
	// drift: recent gain / training gain, relative to all sensors
	rels := map[int]float64{}
	var all []float64
	for id := range cal {
		m, ok := d.c.Models[id]
		st := d.state[id]
		if !ok || st == nil || m.Gain0 <= 0 || faulty[id] || len(st.pairs) < 48 {
			continue
		}
		if g, n := calib.Gain(st.pairs); n >= 12 {
			rels[id] = g / m.Gain0
			all = append(all, rels[id])
		}
	}
	if len(all) >= 5 {
		typical := median(all)
		for id, r := range rels {
			st := d.state[id]
			rel := r / typical
			// Real gains wander with weather and aerosol mix, so a change
			// must last 24 hours before it counts.
			if rel > 1.5 || rel < 1/1.5 {
				st.outside++
			} else {
				st.outside = 0
			}
			if st.outside >= 24 {
				if !st.drifting {
					d.alert(id, Drift, hour, fmt.Sprintf("gain is %.2fx its training value for 24 h; other sensors %.2fx", r, typical))
				}
				st.drifting = true
				faulty[id] = true
			} else if rel > 1/1.2 && rel < 1.2 {
				st.drifting = false
			}
		}
	}
	// spike: compare with the other healthy sensors this hour
	var others []float64
	for id, v := range cal {
		if !faulty[id] {
			others = append(others, v)
		}
	}
	if len(others) >= 5 {
		med := median(others)
		for id, v := range cal {
			if !faulty[id] && v > math.Max(4*med, med+30) {
				d.alert(id, Spike, hour, fmt.Sprintf("%.1f µg/m³ vs city median %.1f", v, med))
				faulty[id] = true
			}
		}
	}
	d.sink.mu.Lock()
	d.sink.Hours++
	for id, v := range cal {
		_, has := d.c.Models[id]
		d.sink.Latest[id] = Value{Sensor: id, Hour: hour, Raw: hs[id].PM25, Calibrated: v, Trusted: has && !faulty[id]}
		d.sink.Values++
	}
	d.sink.mu.Unlock()
}

func median(v []float64) float64 {
	s := append([]float64(nil), v...)
	sort.Float64s(s)
	if len(s) == 0 {
		return 0
	}
	return s[len(s)/2]
}
