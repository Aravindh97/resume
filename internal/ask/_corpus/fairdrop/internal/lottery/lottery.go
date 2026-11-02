// Package lottery decides the queue order for a high-demand sale in a way
// anyone can check afterwards.
//
// The protocol:
//
//  1. Registration is open for a window (say 10 minutes). Registering in the
//     first second or the last gives the same chance, so bots gain nothing
//     from speed.
//  2. When registration closes, the operator publishes the full entry list
//     and its commitment (sha256 of the canonical list) together with a
//     drand round number whose randomness does not exist yet.
//  3. When that round is published by the drand network, the queue position
//     of every entry is sha256(randomness || entry_id), sorted ascending.
//
// Because the list is fixed before the randomness exists, the operator cannot
// pick an order, and because drand is produced by an independent network, it
// cannot pick the randomness either. Any buyer can rerun Verify with the
// published list and the public drand value.
//
// What it does not prevent: one person registering many accounts. That is
// handled by one entry per verified account, outside this package.
package lottery

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"time"
)

type Entry struct {
	ID        string    `json:"id"`         // random, given to the buyer as their receipt
	AccountID string    `json:"account_id"` // one entry per account
	Seats     int       `json:"seats"`      // seats wanted, 1..MaxSeats
	Together  bool      `json:"together"`   // must be seated side by side
	At        time.Time `json:"at"`
}

// Commitment is what the operator publishes when registration closes.
type Commitment struct {
	SaleID     string    `json:"sale_id"`
	Entries    int       `json:"entries"`
	ListSHA256 string    `json:"list_sha256"`
	Round      uint64    `json:"drand_round"` // future round that fixes the order
	ClosedAt   time.Time `json:"closed_at"`
}

// Canonical is the byte form that is hashed: entries sorted by ID, one JSON
// object per line, fields in a fixed order.
func Canonical(entries []Entry) []byte {
	es := append([]Entry(nil), entries...)
	sort.Slice(es, func(i, j int) bool { return es[i].ID < es[j].ID })
	var b bytes.Buffer
	for _, e := range es {
		line, _ := json.Marshal(struct {
			ID        string `json:"id"`
			AccountID string `json:"account_id"`
			Seats     int    `json:"seats"`
			Together  bool   `json:"together"`
		}{e.ID, e.AccountID, e.Seats, e.Together})
		b.Write(line)
		b.WriteByte('\n')
	}
	return b.Bytes()
}

func Commit(saleID string, entries []Entry, round uint64, closed time.Time) Commitment {
	h := sha256.Sum256(Canonical(entries))
	return Commitment{SaleID: saleID, Entries: len(entries), ListSHA256: hex.EncodeToString(h[:]), Round: round, ClosedAt: closed}
}

// Order returns entry IDs in queue order for the given beacon randomness.
func Order(entries []Entry, randomness []byte) []string {
	type k struct {
		id  string
		key [32]byte
	}
	ks := make([]k, len(entries))
	for i, e := range entries {
		h := sha256.New()
		h.Write(randomness)
		h.Write([]byte(e.ID))
		copy(ks[i].key[:], h.Sum(nil))
		ks[i].id = e.ID
	}
	sort.Slice(ks, func(i, j int) bool {
		if c := bytes.Compare(ks[i].key[:], ks[j].key[:]); c != 0 {
			return c < 0
		}
		return ks[i].id < ks[j].id
	})
	out := make([]string, len(ks))
	for i, x := range ks {
		out[i] = x.id
	}
	return out
}

var (
	ErrCommitment = errors.New("entry list does not match the published commitment")
	ErrOrder      = errors.New("published order does not match the order recomputed from the beacon")
)

// Verify recomputes everything a buyer can check: the list matches the
// commitment, and the published order is the one the randomness implies.
func Verify(c Commitment, entries []Entry, randomness []byte, published []string) error {
	h := sha256.Sum256(Canonical(entries))
	if hex.EncodeToString(h[:]) != c.ListSHA256 || len(entries) != c.Entries {
		return ErrCommitment
	}
	want := Order(entries, randomness)
	if len(want) != len(published) {
		return fmt.Errorf("%w: %d positions published, %d expected", ErrOrder, len(published), len(want))
	}
	for i := range want {
		if want[i] != published[i] {
			return fmt.Errorf("%w: first difference at position %d", ErrOrder, i+1)
		}
	}
	return nil
}
