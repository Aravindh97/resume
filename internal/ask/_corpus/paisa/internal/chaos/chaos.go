// Package chaos drives the ledger the way a payment app does on a bad day,
// and checks it still balances.
//
// Each customer payment goes through a simulated UPI flow:
//
//  1. the bank debits the customer and settles to us (it appears in the
//     bank's settlement file)
//  2. our service books the payment in the ledger
//
// and the things that go wrong in production are injected:
//
//   - the ledger write crashes before commit (FaultHook) and is retried
//   - the response is lost after commit, and the client retries with the
//     same idempotency key
//   - two retries race each other
//   - the service gives up entirely after the bank succeeded: the payment
//     is in the bank file but not the ledger (recon must find it)
//   - the bank file is missing a payment we booked, has a wrong amount, or
//     has a duplicate line (recon must find each)
//   - customers request refunds, some twice
package chaos

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Aravindh97/paisa/internal/ledger"
	"github.com/Aravindh97/paisa/internal/recon"
)

type Config struct {
	Customers, Merchants int
	Payments             int
	Workers              int
	Seed                 uint64
	CrashRate            float64 // ledger commit aborted, retried
	LostResponseRate     float64 // committed, response lost, retried
	GiveUpRate           float64 // service never books it (bank did)
	BankDropRate         float64 // booked, missing from the bank file
	BankAmountRate       float64 // wrong amount in the bank file
	BankDupRate          float64 // duplicate line in the bank file
	RefundRate           float64
	// FeeShards splits the platform fee account into N accounts. Every
	// payment credits the fee account, so with one account every payment
	// waits for the same row lock ("hot account"). Sharding by reference
	// spreads the lock; the total fee is the sum of the shards.
	FeeShards int
	// Rich gives every customer enough money that no payment is rejected,
	// which makes final balances independent of the order workers run in.
	// Used to compare a crashed run with a clean one balance for balance.
	Rich bool
}

func Default() Config {
	return Config{Customers: 2000, Merchants: 50, Payments: 50000, Workers: 32, Seed: 1,
		CrashRate: 0.05, LostResponseRate: 0.05, GiveUpRate: 0.004, BankDropRate: 0.003, BankAmountRate: 0.002,
		BankDupRate: 0.002, RefundRate: 0.03, FeeShards: 16}
}

type Result struct {
	Posted, Rejected, Replays  int64
	Crashes, LostResponses     int64
	Refunds, DoubleRefundTries int64
	Planted                    map[recon.Kind]int
	Bank                       []recon.Line
	Customers                  []string
	PaymentPhase               time.Duration
}

func feeAccount(i int) string { return fmt.Sprintf("revenue:fees:%02d", i) }

func Run(ctx context.Context, l *ledger.Ledger, c Config) (*Result, error) {
	rng := rand.New(rand.NewPCG(c.Seed, 99))
	accts := []ledger.Account{{ID: "bank:clearing", Kind: "asset", AllowNegative: true}, {ID: "suspense", Kind: "asset", AllowNegative: true},
		{ID: "topups", Kind: "asset", AllowNegative: true}}
	if c.FeeShards < 1 {
		c.FeeShards = 1
	}
	for i := 0; i < c.FeeShards; i++ {
		accts = append(accts, ledger.Account{ID: feeAccount(i), Kind: "revenue", AllowNegative: true})
	}
	res := &Result{Planted: map[recon.Kind]int{}}
	for i := 0; i < c.Customers; i++ {
		id := fmt.Sprintf("customer:%05d", i)
		res.Customers = append(res.Customers, id)
		accts = append(accts, ledger.Account{ID: id, Kind: "liability"})
	}
	for i := 0; i < c.Merchants; i++ {
		accts = append(accts, ledger.Account{ID: fmt.Sprintf("merchant:%03d", i), Kind: "liability"})
	}
	if err := l.CreateAccounts(ctx, accts...); err != nil {
		return nil, err
	}
	// Opening balances via top-ups, in parallel. Each customer's top-up has
	// its own key, so a restarted run replays them instantly.
	amts := make([]int64, len(res.Customers))
	for i := range amts {
		amts[i] = int64(50_000 + rng.IntN(500_000)) // ₹500 - ₹5,500
		if c.Rich {
			amts[i] = 100_000_000 // ₹10 lakh
		}
	}
	{
		var wg sync.WaitGroup
		var mu sync.Mutex
		var terr error
		idx := make(chan int)
		for w := 0; w < c.Workers; w++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for i := range idx {
					cu := res.Customers[i]
					if _, err := l.Post(ctx, ledger.Request{Key: "topup:" + cu, Kind: "topup",
						Postings: []ledger.Posting{ledger.P("topups", -amts[i]), ledger.P(cu, amts[i])}}); err != nil {
						mu.Lock()
						terr = err
						mu.Unlock()
					}
				}
			}()
		}
		for i := range res.Customers {
			idx <- i
		}
		close(idx)
		wg.Wait()
		if terr != nil {
			return nil, terr
		}
	}

	// Crash injection: abort the commit for keys chosen up front, once each.
	crashOnce := sync.Map{}
	ledger.FaultHook = func(key string) error {
		if _, ok := crashOnce.LoadAndDelete(key); ok {
			return errors.New("injected crash before commit")
		}
		return nil
	}
	defer func() { ledger.FaultHook = nil }()

	type pay struct {
		i                 int
		cust, merch, ref  string
		amount, fee       int64
		crash, lost, give bool
		refund, refund2   bool
	}
	jobs := make([]pay, c.Payments)
	for i := range jobs {
		p := pay{i: i, cust: res.Customers[rng.IntN(len(res.Customers))], merch: fmt.Sprintf("merchant:%03d", rng.IntN(c.Merchants)),
			ref: fmt.Sprintf("UPI%012d", 400000000000+i), amount: int64(1_000 + rng.IntN(20_000))}
		p.fee = p.amount / 100 // 1% merchant fee
		p.crash = rng.Float64() < c.CrashRate
		p.lost = rng.Float64() < c.LostResponseRate
		p.give = rng.Float64() < c.GiveUpRate
		p.refund = rng.Float64() < c.RefundRate
		p.refund2 = p.refund && rng.IntN(3) == 0
		jobs[i] = p
	}

	var mu sync.Mutex
	booked := map[string]int64{} // reference -> merchant credit, for building the bank file
	bookedMerch := map[string]string{}
	ch := make(chan pay)
	var wg sync.WaitGroup
	var firstErr error
	var errOnce sync.Once
	for w := 0; w < c.Workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for p := range ch {
				fee := feeAccount(p.i % c.FeeShards)
				req := ledger.Request{Key: "pay:" + p.ref, Kind: "payment", Reference: p.ref, Postings: []ledger.Posting{
					ledger.P(p.cust, -p.amount), ledger.P(p.merch, p.amount-p.fee), ledger.P(fee, p.fee)}}
				if p.give {
					// The bank settled it, our service never booked it.
					mu.Lock()
					booked[p.ref], bookedMerch[p.ref] = -(p.amount - p.fee), p.merch // negative = only in bank
					mu.Unlock()
					continue
				}
				if p.crash {
					crashOnce.Store(req.Key, true)
					atomic.AddInt64(&res.Crashes, 1)
				}
				var out *ledger.Outcome
				var err error
				for attempt := 0; attempt < 5; attempt++ {
					out, err = l.Post(ctx, req)
					if err == nil {
						break
					}
				}
				if err != nil {
					errOnce.Do(func() { firstErr = err })
					continue
				}
				if p.lost {
					// Response lost: the client retries, sometimes twice at once.
					atomic.AddInt64(&res.LostResponses, 1)
					var w2 sync.WaitGroup
					for k := 0; k < 2; k++ {
						w2.Add(1)
						go func() {
							defer w2.Done()
							if o, err := l.Post(ctx, req); err != nil {
								errOnce.Do(func() { firstErr = err })
							} else if o.Replay {
								atomic.AddInt64(&res.Replays, 1)
							}
						}()
					}
					w2.Wait()
				}
				if out.Rejected != "" {
					atomic.AddInt64(&res.Rejected, 1)
					continue
				}
				atomic.AddInt64(&res.Posted, 1)
				mu.Lock()
				booked[p.ref], bookedMerch[p.ref] = p.amount-p.fee, p.merch
				mu.Unlock()
				if p.refund {
					// Merchant refunds the customer in full; the fee is kept.
					rr := ledger.Request{Key: "refund:" + p.ref, Kind: "refund", Reference: "refund-" + p.ref,
						Postings: []ledger.Posting{ledger.P(p.merch, -(p.amount - p.fee)), ledger.P(fee, -p.fee), ledger.P(p.cust, p.amount)}}
					tries := 1
					if p.refund2 {
						tries = 2
						atomic.AddInt64(&res.DoubleRefundTries, 1)
					}
					for k := 0; k < tries; k++ {
						o, err := l.Post(ctx, rr)
						if err != nil {
							errOnce.Do(func() { firstErr = err })
						} else if !o.Replay && o.Rejected == "" {
							atomic.AddInt64(&res.Refunds, 1)
						}
					}
				}
			}
		}()
	}
	start := time.Now()
	for _, p := range jobs {
		ch <- p
	}
	close(ch)
	wg.Wait()
	res.PaymentPhase = time.Since(start)
	if firstErr != nil {
		return res, firstErr
	}

	// Build the bank's settlement file, with planted breaks.
	refs := make([]string, 0, len(booked))
	for r := range booked {
		refs = append(refs, r)
	}
	sort.Strings(refs)
	for _, r := range refs {
		amt, merch := booked[r], bookedMerch[r]
		if amt < 0 { // service gave up; only the bank has it
			res.Bank = append(res.Bank, recon.Line{Reference: r, Amount: -amt, Merchant: merch})
			res.Planted[recon.MissingInLedger]++
			continue
		}
		switch x := rng.Float64(); {
		case x < c.BankDropRate:
			res.Planted[recon.MissingInBank]++
			continue
		case x < c.BankDropRate+c.BankAmountRate:
			res.Bank = append(res.Bank, recon.Line{Reference: r, Amount: amt - 100, Merchant: merch})
			res.Planted[recon.AmountMismatch]++
			continue
		case x < c.BankDropRate+c.BankAmountRate+c.BankDupRate:
			res.Bank = append(res.Bank, recon.Line{Reference: r, Amount: amt, Merchant: merch})
			res.Planted[recon.DuplicateInBank]++
		}
		res.Bank = append(res.Bank, recon.Line{Reference: r, Amount: amt, Merchant: merch})
	}
	return res, nil
}
