package crdt

import (
	"sync"
	"time"
)

// HLC is a hybrid logical clock timestamp (Kulkarni et al., 2014): physical
// milliseconds plus a logical counter. It never goes backwards, and after a
// device sees a timestamp from another device its own timestamps are later,
// so "last writer wins" follows real cause and effect even when a phone's
// clock is minutes wrong.
type HLC struct {
	Wall    int64  `json:"w"` // unix milliseconds
	Logical uint32 `json:"l"`
}

func (a HLC) Less(b HLC) bool {
	if a.Wall != b.Wall {
		return a.Wall < b.Wall
	}
	return a.Logical < b.Logical
}

type Clock struct {
	mu   sync.Mutex
	last HLC
	Phys func() int64 // physical clock in ms; replaced in tests to simulate skew
}

func NewClock(phys func() int64) *Clock {
	if phys == nil {
		phys = func() int64 { return time.Now().UnixMilli() }
	}
	return &Clock{Phys: phys}
}

func (c *Clock) Now() HLC {
	c.mu.Lock()
	defer c.mu.Unlock()
	p := c.Phys()
	if p > c.last.Wall {
		c.last = HLC{Wall: p}
	} else {
		c.last.Logical++
	}
	return c.last
}

// Observe moves the clock past a timestamp received from another device.
func (c *Clock) Observe(t HLC) {
	c.mu.Lock()
	defer c.mu.Unlock()
	p := c.Phys()
	switch {
	case p > c.last.Wall && p > t.Wall:
		c.last = HLC{Wall: p}
	case t.Wall > c.last.Wall:
		c.last = HLC{Wall: t.Wall, Logical: t.Logical + 1}
	case c.last.Wall > t.Wall:
		c.last.Logical++
	default:
		c.last.Logical = max(c.last.Logical, t.Logical) + 1
	}
}
