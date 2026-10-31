// Package resilience wraps calls to external services (embedding API, OCR
// service) with bounded retries and a circuit breaker.
package resilience

import (
	"context"
	"errors"
	"math/rand/v2"
	"sync"
	"time"
)

// Permanent marks an error that must not be retried (e.g. HTTP 400).
type Permanent struct{ Err error }

func (p Permanent) Error() string { return p.Err.Error() }
func (p Permanent) Unwrap() error { return p.Err }

type Retry struct {
	Attempts int
	Base     time.Duration
	Max      time.Duration
	Sleep    func(context.Context, time.Duration) error // injectable for tests
}

func DefaultRetry() Retry {
	return Retry{Attempts: 4, Base: 200 * time.Millisecond, Max: 5 * time.Second}
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// Do runs fn with exponential backoff and full jitter.
func (r Retry) Do(ctx context.Context, fn func(context.Context) error) error {
	sleep := r.Sleep
	if sleep == nil {
		sleep = sleepCtx
	}
	var err error
	for i := 0; i < max(1, r.Attempts); i++ {
		if err = fn(ctx); err == nil {
			return nil
		}
		var p Permanent
		if errors.As(err, &p) || errors.Is(err, ErrOpen) || ctx.Err() != nil {
			return err
		}
		if i == r.Attempts-1 {
			break
		}
		d := min(r.Max, r.Base<<i)
		if d > 0 {
			d = time.Duration(rand.Int64N(int64(d)) + 1)
		}
		if serr := sleep(ctx, d); serr != nil {
			return serr
		}
	}
	return err
}

var ErrOpen = errors.New("circuit breaker open")

// Breaker opens after Threshold consecutive failures and lets a single probe
// through after Cooldown. While open, calls fail fast with ErrOpen so a dead
// dependency does not tie up every worker slot in timeouts.
type Breaker struct {
	Threshold int
	Cooldown  time.Duration
	Now       func() time.Time

	mu       sync.Mutex
	fails    int
	openedAt time.Time
	probing  bool
}

func NewBreaker(threshold int, cooldown time.Duration) *Breaker {
	return &Breaker{Threshold: threshold, Cooldown: cooldown, Now: time.Now}
}

func (b *Breaker) Do(ctx context.Context, fn func(context.Context) error) error {
	b.mu.Lock()
	if b.fails >= b.Threshold {
		if b.probing || b.Now().Sub(b.openedAt) < b.Cooldown {
			b.mu.Unlock()
			return ErrOpen
		}
		b.probing = true
	}
	b.mu.Unlock()

	err := fn(ctx)

	b.mu.Lock()
	defer b.mu.Unlock()
	b.probing = false
	var p Permanent
	if err == nil || errors.As(err, &p) {
		// A permanent error is the caller's fault, not a sign the dependency is down.
		b.fails = 0
		return err
	}
	b.fails++
	if b.fails >= b.Threshold {
		b.openedAt = b.Now()
	}
	return err
}
