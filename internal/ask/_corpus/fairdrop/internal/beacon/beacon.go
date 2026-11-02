// Package beacon fetches public randomness from the drand network
// (https://drand.love), which publishes a new value every 3 seconds on its
// "quicknet" chain. A round number in the future is a value nobody knows yet.
//
// Limitation: this client trusts the HTTPS response from api.drand.sh and does
// not verify drand's BLS signature. Verifying it needs a BLS12-381 library;
// the `drand` CLI or any drand client can be used to cross-check a round.
package beacon

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"
)

// Quicknet is drand's 3-second unchained chain.
const Quicknet = "52db9ba70e0cc0f6eaf7803dd07447a1f5477735fd3f661792ba94600c84e971"

var (
	quicknetGenesis = time.Unix(1692803367, 0)
	quicknetPeriod  = 3 * time.Second
)

type Beacon interface {
	// RoundAt is the first round published at or after t.
	RoundAt(t time.Time) uint64
	// Randomness returns the value of a round, waiting for it if needed.
	Randomness(ctx context.Context, round uint64) ([]byte, error)
}

type Drand struct {
	BaseURL string
	Chain   string
	HTTP    *http.Client
}

func NewDrand() *Drand {
	return &Drand{BaseURL: "https://api.drand.sh", Chain: Quicknet, HTTP: &http.Client{Timeout: 10 * time.Second}}
}

func (d *Drand) RoundAt(t time.Time) uint64 {
	if t.Before(quicknetGenesis) {
		return 1
	}
	n := t.Sub(quicknetGenesis) / quicknetPeriod
	return uint64(n) + 2
}

var ErrNotYet = errors.New("round not published yet")

// transient marks network errors and 5xx responses, which are retried.
type transient struct{ error }

func (t transient) Unwrap() error { return t.error }

func (d *Drand) Randomness(ctx context.Context, round uint64) ([]byte, error) {
	for {
		b, err := d.fetch(ctx, round)
		var te transient
		if err == nil || !errors.Is(err, ErrNotYet) && !errors.As(err, &te) {
			return b, err
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(time.Second):
		}
	}
}

func (d *Drand) fetch(ctx context.Context, round uint64) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, fmt.Sprintf("%s/%s/public/%d", d.BaseURL, d.Chain, round), nil)
	if err != nil {
		return nil, err
	}
	resp, err := d.HTTP.Do(req)
	if err != nil {
		return nil, transient{err}
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound || resp.StatusCode == 425 {
		return nil, ErrNotYet
	}
	if resp.StatusCode >= 500 {
		return nil, transient{fmt.Errorf("drand: HTTP %d", resp.StatusCode)}
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("drand: HTTP %d", resp.StatusCode)
	}
	var r struct {
		Round      uint64 `json:"round"`
		Randomness string `json:"randomness"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&r); err != nil {
		return nil, err
	}
	if r.Round != round {
		return nil, fmt.Errorf("drand: asked for round %d, got %d", round, r.Round)
	}
	return hex.DecodeString(r.Randomness)
}

// Fixed is a beacon with known values, for tests and offline demos.
type Fixed map[uint64][]byte

func (f Fixed) RoundAt(time.Time) uint64 { return 1 }
func (f Fixed) Randomness(_ context.Context, r uint64) ([]byte, error) {
	if v, ok := f[r]; ok {
		return v, nil
	}
	return nil, ErrNotYet
}
