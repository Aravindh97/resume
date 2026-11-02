package store

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"time"
)

// Types and helpers shared by the Postgres store and the in-memory store.

func rowLabel(i int) string {
	s := ""
	for i++; i > 0; i = (i - 1) / 26 {
		s = string(rune('A'+(i-1)%26)) + s
	}
	return s
}

var (
	ErrUnavailable = errors.New("seats no longer available")
	ErrNoBlock     = errors.New("no block of adjacent seats that size is left")
	ErrHoldExpired = errors.New("hold expired")
	ErrNotYours    = errors.New("hold belongs to another buyer")
	ErrSoldOut     = errors.New("sold out")
)

type Hold struct {
	ID      string    `json:"hold_id"`
	Seats   []string  `json:"seats"`
	Expires time.Time `json:"expires_at"`
}

func newID(prefix string) string {
	b := make([]byte, 9)
	rand.Read(b)
	return prefix + "/" + hex.EncodeToString(b)
}

func abs(f float64) float64 {
	if f < 0 {
		return -f
	}
	return f
}

// Payer charges a buyer. It may fail, time out or succeed.
type Payer interface {
	Charge(ctx context.Context, idempotencyKey, buyer string, amount int) error
	Refund(ctx context.Context, idempotencyKey string) error
}

type Result struct {
	Outcome string   `json:"outcome"` // booked | payment_failed | expired | refunded | not_yours
	Seats   []string `json:"seats,omitempty"`
	Replay  bool     `json:"replay,omitempty"` // same key seen before; first result returned
	Detail  string   `json:"detail,omitempty"`
}

func showOf(holdID string) string {
	for i := 0; i < len(holdID); i++ {
		if holdID[i] == '/' {
			return holdID[:i]
		}
	}
	return holdID
}

// Audit checks the invariants after a sale.
type Audit struct {
	Seats, Sold, Held, Free  int
	Bookings                 int
	DoubleSold               int // seats with more than one booking (must be 0)
	SoldWithoutBooking       int // status sold but no booking row (must be 0)
	BookingsOnUnsoldSeat     int // booking row but seat not sold (must be 0)
	NaiveBookings, NaiveDupe int // naive baseline, for comparison
	TogetherBroken           int // holds that asked for adjacency but got a gap (must be 0)
}
