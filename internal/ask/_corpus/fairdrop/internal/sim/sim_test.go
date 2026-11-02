package sim

import (
	"context"
	"testing"
	"time"

	"github.com/Aravindh97/fairdrop/internal/store"
)

func TestMemStoreSameGuarantees(t *testing.T) {
	ctx := context.Background()
	m := store.NewMem(200 * time.Millisecond)
	m.CreateShow(ctx, "s1", 10, 20)
	r, err := Run(ctx, m, Config{Show: "s1", Buyers: 3000, Concurrency: 64, Seed: 3})
	if err != nil {
		t.Fatal(err)
	}
	a := r.Audit
	if a.DoubleSold != 0 || a.SoldWithoutBooking != 0 || a.BookingsOnUnsoldSeat != 0 || a.TogetherBroken != 0 || r.Errors != 0 {
		t.Fatalf("%s", r.Print())
	}
	if r.Charged != r.Booked+r.Refunded {
		t.Fatalf("money: %s", r.Print())
	}
	if a.Sold == 0 || r.Replays == 0 {
		t.Fatalf("test did not exercise sales and retries: %s", r.Print())
	}
}

func TestMemNaiveDoubleSells(t *testing.T) {
	ctx := context.Background()
	m := store.NewMem(time.Minute)
	m.CreateShow(ctx, "s2", 5, 10)
	r, err := Run(ctx, m, Config{Show: "s2", Buyers: 500, Concurrency: 64, Seed: 1, Naive: true})
	if err != nil {
		t.Fatal(err)
	}
	if r.Audit.NaiveDupe == 0 {
		t.Fatalf("naive baseline did not double-sell: %s", r.Print())
	}
}
