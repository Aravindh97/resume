//go:build integration

package store_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/Aravindh97/fairdrop/internal/sim"
	"github.com/Aravindh97/fairdrop/internal/store"
)

func open(t *testing.T, ttl time.Duration) (*store.Store, string) {
	url := os.Getenv("DATABASE_URL")
	if url == "" {
		t.Skip("DATABASE_URL not set")
	}
	s, err := store.Open(context.Background(), url, ttl)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.DB.Close)
	show := fmt.Sprintf("t%d", time.Now().UnixNano())
	if err := s.CreateShow(context.Background(), show, 2, 10); err != nil {
		t.Fatal(err)
	}
	return s, show
}

func TestOneSeatManyBuyers(t *testing.T) {
	s, show := open(t, time.Minute)
	var wg sync.WaitGroup
	var mu sync.Mutex
	won := 0
	for i := 0; i < 200; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			if _, err := s.HoldSeats(context.Background(), show, fmt.Sprint(i), []string{"A5"}); err == nil {
				mu.Lock()
				won++
				mu.Unlock()
			} else if !errors.Is(err, store.ErrUnavailable) {
				t.Error(err)
			}
		}(i)
	}
	wg.Wait()
	if won != 1 {
		t.Fatalf("%d buyers hold seat A5", won)
	}
}

func TestHoldIsAllOrNothing(t *testing.T) {
	s, show := open(t, time.Minute)
	ctx := context.Background()
	if _, err := s.HoldSeats(ctx, show, "a", []string{"A3"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.HoldSeats(ctx, show, "b", []string{"A2", "A3", "A4"}); !errors.Is(err, store.ErrUnavailable) {
		t.Fatalf("partial hold allowed: %v", err)
	}
	a, _ := s.Audit(ctx, show)
	if a.Held != 1 {
		t.Fatalf("held = %d, A2/A4 must have been rolled back", a.Held)
	}
}

func TestExpiredHoldIsReclaimedAndLateConfirmRefunds(t *testing.T) {
	s, show := open(t, 50*time.Millisecond)
	ctx := context.Background()
	h, err := s.HoldSeats(ctx, show, "slow", []string{"B1"})
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(80 * time.Millisecond)
	if _, err := s.HoldSeats(ctx, show, "fast", []string{"B1"}); err != nil {
		t.Fatalf("expired seat not reclaimable: %v", err)
	}
	pay := sim.NewFakePayer()
	r, err := s.Confirm(ctx, h.ID, "slow", "k1", pay, 100)
	if err != nil || r.Outcome != "expired" {
		t.Fatalf("late confirm: %+v %v", r, err)
	}
}

func TestConfirmIsIdempotent(t *testing.T) {
	s, show := open(t, time.Minute)
	ctx := context.Background()
	h, _ := s.HoldSeats(ctx, show, "u", []string{"A1", "A2"})
	pay := sim.NewFakePayer()
	var wg sync.WaitGroup
	results := make([]*store.Result, 10)
	for i := range results {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			r, err := s.Confirm(ctx, h.ID, "u", "same-key", pay, 100)
			if err != nil {
				t.Error(err)
			}
			results[i] = r
		}(i)
	}
	wg.Wait()
	first := 0
	for _, r := range results {
		if r.Outcome != "booked" {
			t.Fatalf("outcome %s", r.Outcome)
		}
		if !r.Replay {
			first++
		}
	}
	a, _ := s.Audit(ctx, show)
	if first != 1 || a.Bookings != 2 {
		t.Fatalf("first=%d bookings=%d", first, a.Bookings)
	}
	if _, err := s.Confirm(ctx, h.ID, "someone-else", "other-key", pay, 100); err != nil {
		t.Fatal(err)
	}
}

func TestFamilyGetsAdjacentSeats(t *testing.T) {
	s, show := open(t, time.Minute)
	ctx := context.Background()
	// Fragment row A so only B has 5 in a row.
	for _, id := range []string{"A3", "A6", "A9"} {
		s.HoldSeats(ctx, show, "x", []string{id})
	}
	h, err := s.HoldBest(ctx, show, "family", 5, true)
	if err != nil {
		t.Fatal(err)
	}
	if h.Seats[0][0] != 'B' {
		t.Fatalf("family split or squeezed: %v", h.Seats)
	}
	if _, err := s.HoldBest(ctx, show, "big", 11, true); !errors.Is(err, store.ErrNoBlock) {
		t.Fatalf("impossible block: %v", err)
	}
}
