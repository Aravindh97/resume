// Package sim runs a sale against the store with many concurrent buyers and
// reports latency and the audit.
//
// Buyer behaviour, chosen to hit every failure path:
//   - group size 1-6; groups of 3+ ask to sit together
//   - 12% abandon their hold (seats must come back when it expires)
//   - 5% of payments are declined
//   - 8% of payments time out once and the client retries with the same
//     idempotency key (must be charged and booked once)
//   - 3% double-click Confirm concurrently
package sim

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Aravindh97/fairdrop/internal/store"
)

type Config struct {
	Show        string
	Buyers      int
	Concurrency int
	Seed        uint64
	Naive       bool // run the unsafe baseline instead
}

// FakePayer records every charge by idempotency key so the report can prove
// nobody was charged twice.
type FakePayer struct {
	mu      sync.Mutex
	charges map[string]int
	refunds map[string]bool
	fail    map[string]bool
	timeout map[string]bool
}

func NewFakePayer() *FakePayer {
	return &FakePayer{charges: map[string]int{}, refunds: map[string]bool{}, fail: map[string]bool{}, timeout: map[string]bool{}}
}

var errTimeout = errors.New("payment provider timeout")

func (p *FakePayer) Charge(ctx context.Context, key, buyer string, amount int) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.fail[key] {
		return errors.New("card declined")
	}
	// The provider deduplicates by key, like Stripe or Razorpay do.
	if _, ok := p.charges[key]; !ok {
		p.charges[key] = amount
	}
	return nil
}

func (p *FakePayer) Refund(_ context.Context, key string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if _, ok := p.charges[key]; !ok {
		return errors.New("refund without charge")
	}
	p.refunds[key] = true
	return nil
}

type Report struct {
	Mode                 string
	Buyers, Concurrency  int
	Booked, SeatsBooked  int
	SoldOut, NoBlock     int
	Abandoned, Declined  int
	Retried, DoubleClick int
	Replays              int
	Errors               int
	FirstError           string
	Duration             time.Duration
	HoldP50, HoldP99     time.Duration
	ConfirmP99           time.Duration
	Audit                *store.Audit
	Freed                int64
	Charged, Refunded    int
	Refunds              int
}

func pct(d []time.Duration, p float64) time.Duration {
	if len(d) == 0 {
		return 0
	}
	sort.Slice(d, func(i, j int) bool { return d[i] < d[j] })
	return d[min(len(d)-1, int(float64(len(d))*p))]
}

func Run(ctx context.Context, s *store.Store, cfg Config) (*Report, error) {
	r := &Report{Mode: "safe", Buyers: cfg.Buyers, Concurrency: cfg.Concurrency}
	if cfg.Naive {
		r.Mode = "naive"
	}
	pay := NewFakePayer()
	var mu sync.Mutex
	var holdLat, confLat []time.Duration
	var refundedOutcomes atomic.Int64
	var booked, seatsBooked, soldOut, noBlock, abandoned, declined, retried, dbl, replays, errs atomic.Int64
	var firstErr atomic.Value
	jobs := make(chan int)
	var wg sync.WaitGroup
	start := time.Now()
	for w := 0; w < cfg.Concurrency; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range jobs {
				rng := rand.New(rand.NewPCG(cfg.Seed, uint64(i)))
				buyer := fmt.Sprintf("buyer-%06d", i)
				n := 1 + rng.IntN(6)
				together := n >= 3
				fail := func(err error) {
					errs.Add(1)
					firstErr.CompareAndSwap(nil, err.Error())
				}
				if cfg.Naive {
					t := time.Now()
					seats, err := s.NaiveBook(ctx, cfg.Show, buyer, n)
					mu.Lock()
					holdLat = append(holdLat, time.Since(t))
					mu.Unlock()
					switch {
					case errors.Is(err, store.ErrSoldOut):
						soldOut.Add(1)
					case err != nil:
						fail(err)
					default:
						booked.Add(1)
						seatsBooked.Add(int64(len(seats)))
					}
					continue
				}
				t := time.Now()
				h, err := s.HoldBest(ctx, cfg.Show, buyer, n, together)
				mu.Lock()
				holdLat = append(holdLat, time.Since(t))
				mu.Unlock()
				switch {
				case errors.Is(err, store.ErrSoldOut), errors.Is(err, store.ErrUnavailable):
					soldOut.Add(1)
					continue
				case errors.Is(err, store.ErrNoBlock):
					noBlock.Add(1)
					continue
				case err != nil:
					fail(err)
					continue
				}
				roll := rng.Float64()
				key := "pay-" + h.ID
				switch {
				case roll < 0.12: // walks away; hold must expire
					abandoned.Add(1)
					continue
				case roll < 0.17:
					pay.mu.Lock()
					pay.fail[key] = true
					pay.mu.Unlock()
					declined.Add(1)
				}
				t = time.Now()
				var res *store.Result
				switch {
				case roll >= 0.17 && roll < 0.25: // timeout, then retry with the same key
					retried.Add(1)
					if _, err := s.Confirm(ctx, h.ID, buyer, key, pay, 250); err != nil {
						fail(err)
						continue
					}
					res, err = s.Confirm(ctx, h.ID, buyer, key, pay, 250)
				case roll >= 0.25 && roll < 0.28: // double click: two concurrent confirms
					dbl.Add(1)
					var wg2 sync.WaitGroup
					results := make([]*store.Result, 2)
					errs2 := make([]error, 2)
					for k := 0; k < 2; k++ {
						wg2.Add(1)
						go func(k int) {
							defer wg2.Done()
							results[k], errs2[k] = s.Confirm(ctx, h.ID, buyer, key, pay, 250)
						}(k)
					}
					wg2.Wait()
					res, err = results[0], errors.Join(errs2...)
					if results[0] != nil && results[0].Replay {
						res = results[1]
					}
				default:
					res, err = s.Confirm(ctx, h.ID, buyer, key, pay, 250)
				}
				mu.Lock()
				confLat = append(confLat, time.Since(t))
				mu.Unlock()
				if err != nil {
					fail(err)
					continue
				}
				if res.Replay {
					replays.Add(1)
				}
				if res.Outcome == "refunded" {
					refundedOutcomes.Add(1)
				}
				if res.Outcome == "booked" {
					booked.Add(1)
					seatsBooked.Add(int64(len(res.Seats)))
				}
			}
		}()
	}
	for i := 0; i < cfg.Buyers; i++ {
		select {
		case jobs <- i:
		case <-ctx.Done():
		}
	}
	close(jobs)
	wg.Wait()
	r.Duration = time.Since(start)
	r.Booked, r.SeatsBooked = int(booked.Load()), int(seatsBooked.Load())
	r.SoldOut, r.NoBlock, r.Abandoned, r.Declined = int(soldOut.Load()), int(noBlock.Load()), int(abandoned.Load()), int(declined.Load())
	r.Retried, r.DoubleClick, r.Replays, r.Errors = int(retried.Load()), int(dbl.Load()), int(replays.Load()), int(errs.Load())
	if v, ok := firstErr.Load().(string); ok {
		r.FirstError = v
	}
	r.HoldP50, r.HoldP99, r.ConfirmP99 = pct(holdLat, 0.5), pct(holdLat, 0.99), pct(confLat, 0.99)
	r.Charged, r.Refunded, r.Refunds = len(pay.charges), len(pay.refunds), int(refundedOutcomes.Load())
	var err error
	if r.Audit, err = s.Audit(ctx, cfg.Show); err != nil {
		return r, err
	}
	return r, nil
}

func (r *Report) Print() string {
	a := r.Audit
	s := fmt.Sprintf(`mode=%s buyers=%d concurrency=%d duration=%s (%.0f buyers/s)
seats=%d sold=%d held=%d free=%d
booked=%d buyers (%d seats)  sold-out=%d  no-adjacent-block=%d
abandoned=%d declined=%d timeout+retry=%d double-click=%d replays-served=%d errors=%d
hold p50=%s p99=%s   confirm p99=%s
`, r.Mode, r.Buyers, r.Concurrency, r.Duration.Round(time.Millisecond), float64(r.Buyers)/r.Duration.Seconds(),
		a.Seats, a.Sold, a.Held, a.Free, r.Booked, r.SeatsBooked, r.SoldOut, r.NoBlock,
		r.Abandoned, r.Declined, r.Retried, r.DoubleClick, r.Replays, r.Errors,
		r.HoldP50.Round(time.Microsecond), r.HoldP99.Round(time.Microsecond), r.ConfirmP99.Round(time.Microsecond))
	if r.Mode == "naive" {
		s += fmt.Sprintf("NAIVE: %d booking rows for %d seats -> %d seats sold more than once\n", a.NaiveBookings, a.Sold, a.NaiveDupe)
	} else {
		s += fmt.Sprintf("AUDIT: double-sold=%d sold-without-booking=%d booking-on-unsold=%d groups-not-adjacent=%d\n",
			a.DoubleSold, a.SoldWithoutBooking, a.BookingsOnUnsoldSeat, a.TogetherBroken)
		s += fmt.Sprintf("MONEY: charged=%d = booked %d + refunded %d (%s)\n", r.Charged, r.Booked, r.Refunded,
			map[bool]string{true: "balanced", false: "MISMATCH"}[r.Charged == r.Booked+r.Refunded])
	}
	if r.FirstError != "" {
		s += "first error: " + r.FirstError + "\n"
	}
	return s
}
