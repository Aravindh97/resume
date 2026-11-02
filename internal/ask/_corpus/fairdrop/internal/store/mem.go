package store

import (
	"context"
	"errors"
	"fmt"
	"runtime"
	"sort"
	"sync"
	"time"
)

// Mem is an in-memory store with the same rules as the Postgres one, for
// the browser demo and fast tests:
//
//   - one mutex per show plays the part of the row locks, so a hold claims
//     exactly the requested seats or none;
//   - a booking is refused if the seat already has one (the primary key);
//   - the first Confirm with a key does the work; concurrent and later calls
//     with the key wait for and return its result.
//
// NaiveBook deliberately does not take the lock across its read and write,
// which reproduces the read-then-write bug exactly as it happens in a database.
type Mem struct {
	TTL time.Duration
	Now func() time.Time

	mu       sync.Mutex
	shows    map[string]*memShow
	payments map[string]*memPayment
}

type memSeat struct {
	SeatPos
	status  string // free | held | sold
	holdID  string
	expires time.Time
}

type memShow struct {
	mu       sync.Mutex
	seats    []*memSeat // row-major, front row first
	byID     map[string]*memSeat
	holds    map[string]*memHold
	bookings map[string][]string // seat -> buyers (more than one = double sale)
	naive    map[string][]string // naive baseline bookings
}

type memHold struct {
	id, buyer string
	seats     []string
	expires   time.Time
	state     string
}

type memPayment struct {
	done chan struct{}
	res  *Result
}

func NewMem(ttl time.Duration) *Mem {
	return &Mem{TTL: ttl, Now: time.Now, shows: map[string]*memShow{}, payments: map[string]*memPayment{}}
}

func (m *Mem) CreateShow(_ context.Context, show string, rows, perRow int) error {
	s := &memShow{byID: map[string]*memSeat{}, holds: map[string]*memHold{}, bookings: map[string][]string{}, naive: map[string][]string{}}
	for r := 0; r < rows; r++ {
		for n := 1; n <= perRow; n++ {
			x := &memSeat{SeatPos: SeatPos{ID: SeatID(r, n), Row: RowLabel(r), Num: n}, status: "free"}
			s.seats = append(s.seats, x)
			s.byID[x.ID] = x
		}
	}
	m.mu.Lock()
	m.shows[show] = s
	m.mu.Unlock()
	return nil
}

func (m *Mem) show(id string) (*memShow, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	s, ok := m.shows[id]
	if !ok {
		return nil, fmt.Errorf("no show %q", id)
	}
	return s, nil
}

func (x *memSeat) claimable(now time.Time) bool {
	return x.status == "free" || x.status == "held" && x.expires.Before(now)
}

func (m *Mem) HoldSeats(_ context.Context, show, buyer string, seats []string) (*Hold, error) {
	s, err := m.show(show)
	if err != nil {
		return nil, err
	}
	now := m.Now()
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, id := range seats {
		x, ok := s.byID[id]
		if !ok || !x.claimable(now) {
			return nil, ErrUnavailable
		}
	}
	h := &Hold{ID: newID(show), Seats: append([]string(nil), seats...), Expires: now.Add(m.TTL)}
	for _, id := range seats {
		x := s.byID[id]
		x.status, x.holdID, x.expires = "held", h.ID, h.Expires
	}
	s.holds[h.ID] = &memHold{id: h.ID, buyer: buyer, seats: h.Seats, expires: h.Expires, state: "active"}
	return h, nil
}

func (m *Mem) HoldBest(ctx context.Context, show, buyer string, n int, together bool) (*Hold, error) {
	s, err := m.show(show)
	if err != nil {
		return nil, err
	}
	for attempt := 0; attempt < 6; attempt++ {
		now := m.Now()
		s.mu.Lock()
		var free []SeatPos
		for _, x := range s.seats {
			if x.claimable(now) {
				free = append(free, x.SeatPos)
			}
		}
		s.mu.Unlock()
		blocks, singles := Candidates(free, n)
		var pick []string
		switch {
		case len(blocks) > 0:
			pick = blocks[min(attempt, len(blocks)-1)]
		case together && len(singles) > 0:
			return nil, ErrNoBlock
		case len(singles) >= n:
			pick = singles[:n]
		default:
			return nil, ErrSoldOut
		}
		runtime.Gosched() // another buyer may take the block in between, as in the database
		h, err := m.HoldSeats(ctx, show, buyer, pick)
		if !errors.Is(err, ErrUnavailable) {
			return h, err
		}
	}
	return nil, ErrUnavailable
}

func (m *Mem) Confirm(ctx context.Context, holdID, buyer, key string, pay Payer, price int) (*Result, error) {
	m.mu.Lock()
	if p, ok := m.payments[key]; ok {
		m.mu.Unlock()
		select {
		case <-p.done:
			r := *p.res
			r.Replay = true
			return &r, nil
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	p := &memPayment{done: make(chan struct{})}
	m.payments[key] = p
	m.mu.Unlock()
	finish := func(r *Result) (*Result, error) {
		p.res = r
		close(p.done)
		return r, nil
	}
	s, err := m.show(showOf(holdID))
	if err != nil {
		return finish(&Result{Outcome: "not_yours"})
	}
	s.mu.Lock()
	h, ok := s.holds[holdID]
	if !ok || h.buyer != buyer {
		s.mu.Unlock()
		return finish(&Result{Outcome: "not_yours"})
	}
	if h.state != "active" || !m.Now().Before(h.expires) {
		s.mu.Unlock()
		return finish(&Result{Outcome: "expired"})
	}
	seats := append([]string(nil), h.seats...)
	s.mu.Unlock()

	if err := pay.Charge(ctx, key, buyer, price*len(seats)); err != nil {
		m.Release(ctx, holdID)
		return finish(&Result{Outcome: "payment_failed", Detail: err.Error()})
	}
	runtime.Gosched() // the payment provider takes time

	s.mu.Lock()
	defer s.mu.Unlock()
	for _, id := range seats {
		x := s.byID[id]
		if x.holdID != holdID || x.status != "held" {
			if err := pay.Refund(ctx, key); err != nil {
				return nil, err
			}
			return finish(&Result{Outcome: "refunded", Detail: "hold lapsed during payment"})
		}
	}
	for _, id := range seats {
		if len(s.bookings[id]) > 0 {
			return nil, fmt.Errorf("double sale prevented: seat %s already booked", id) // the primary key
		}
	}
	for _, id := range seats {
		x := s.byID[id]
		x.status, x.expires = "sold", time.Time{}
		s.bookings[id] = append(s.bookings[id], buyer)
	}
	h.state = "confirmed"
	return finish(&Result{Outcome: "booked", Seats: seats})
}

func (m *Mem) Release(_ context.Context, holdID string) error {
	s, err := m.show(showOf(holdID))
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if h, ok := s.holds[holdID]; ok && h.state == "active" {
		h.state = "released"
		for _, id := range h.seats {
			if x := s.byID[id]; x.holdID == holdID && x.status == "held" {
				x.status, x.holdID = "free", ""
			}
		}
	}
	return nil
}

// NaiveBook reads the free seats, waits as an app would for the payment
// call, then marks them sold, without holding the lock across the two.
func (m *Mem) NaiveBook(_ context.Context, show, buyer string, n int) ([]string, error) {
	s, err := m.show(show)
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	var seats []string
	for _, x := range s.seats {
		if x.status == "free" && len(seats) < n {
			seats = append(seats, x.ID)
		}
	}
	s.mu.Unlock()
	if len(seats) < n {
		return nil, ErrSoldOut
	}
	time.Sleep(time.Millisecond) // the payment call; other buyers read the same free seats meanwhile
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, id := range seats {
		s.byID[id].status = "sold"
		s.naive[id] = append(s.naive[id], buyer)
	}
	return seats, nil
}

func (m *Mem) Audit(_ context.Context, show string) (*Audit, error) {
	s, err := m.show(show)
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	a := &Audit{Seats: len(s.seats)}
	for _, x := range s.seats {
		switch x.status {
		case "sold":
			a.Sold++
			if len(s.bookings[x.ID]) == 0 && len(s.naive[x.ID]) == 0 {
				a.SoldWithoutBooking++
			}
		case "held":
			a.Held++
		default:
			a.Free++
		}
	}
	for id, bs := range s.bookings {
		a.Bookings += len(bs)
		if len(bs) > 1 {
			a.DoubleSold++
		}
		if s.byID[id].status != "sold" {
			a.BookingsOnUnsoldSeat++
		}
	}
	for _, bs := range s.naive {
		a.NaiveBookings += len(bs)
		if len(bs) > 1 {
			a.NaiveDupe += len(bs) - 1
		}
	}
	for _, h := range s.holds {
		if h.state != "confirmed" || len(h.seats) < 3 {
			continue
		}
		var nums []int
		row := ""
		for _, id := range h.seats {
			x := s.byID[id]
			if row != "" && x.Row != row {
				a.TogetherBroken++
				nums = nil
				break
			}
			row = x.Row
			nums = append(nums, x.Num)
		}
		sort.Ints(nums)
		if len(nums) > 0 && nums[len(nums)-1]-nums[0] != len(nums)-1 {
			a.TogetherBroken++
		}
	}
	return a, nil
}

// SeatState is one seat for drawing the hall.
type SeatState struct {
	ID       string `json:"id"`
	Row      string `json:"row"`
	Num      int    `json:"num"`
	Status   string `json:"status"`
	Bookings int    `json:"bookings"` // > 1 means sold more than once
}

func (m *Mem) Seats(show string) ([]SeatState, error) {
	s, err := m.show(show)
	if err != nil {
		return nil, err
	}
	now := m.Now()
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]SeatState, len(s.seats))
	for i, x := range s.seats {
		st := x.status
		if st == "held" && x.expires.Before(now) {
			st = "free"
		}
		out[i] = SeatState{x.ID, x.Row, x.Num, st, len(s.bookings[x.ID]) + len(s.naive[x.ID])}
	}
	return out, nil
}
