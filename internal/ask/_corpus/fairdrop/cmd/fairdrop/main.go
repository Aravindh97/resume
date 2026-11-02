// Command fairdrop runs the ticketing load test and the verifiable queue.
//
//	fairdrop loadtest -buyers 50000 -rows 40 -per-row 50   # needs DATABASE_URL
//	fairdrop loadtest -naive ...                           # the unsafe baseline
//	fairdrop lottery -entries 50000 -out sale/             # real drand round
//	fairdrop verify sale/                                   # anyone can run this
package main

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"math/rand/v2"
	"os"
	"os/signal"
	"path/filepath"
	"time"

	"github.com/Aravindh97/fairdrop/internal/beacon"
	"github.com/Aravindh97/fairdrop/internal/lottery"
	"github.com/Aravindh97/fairdrop/internal/sim"
	"github.com/Aravindh97/fairdrop/internal/store"
)

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: fairdrop loadtest|lottery|verify")
		os.Exit(2)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	var err error
	switch os.Args[1] {
	case "loadtest":
		err = loadtest(ctx, os.Args[2:])
	case "lottery":
		err = runLottery(ctx, os.Args[2:])
	case "verify":
		err = verify(os.Args[2:])
	default:
		err = fmt.Errorf("unknown command %q", os.Args[1])
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func loadtest(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("loadtest", flag.ExitOnError)
	buyers := fs.Int("buyers", 50000, "simulated buyers")
	conc := fs.Int("concurrency", 200, "buyers in flight at once")
	rows := fs.Int("rows", 40, "rows")
	perRow := fs.Int("per-row", 50, "seats per row")
	ttl := fs.Duration("hold", 2*time.Second, "hold lifetime (short so abandoned seats come back during the test)")
	naive := fs.Bool("naive", false, "run the unsafe read-then-write baseline")
	seed := fs.Uint64("seed", 1, "random seed")
	fs.Parse(args)
	url := os.Getenv("DATABASE_URL")
	if url == "" {
		return fmt.Errorf("set DATABASE_URL")
	}
	s, err := store.Open(ctx, url, *ttl)
	if err != nil {
		return err
	}
	defer s.DB.Close()
	show := fmt.Sprintf("show-%d", time.Now().UnixNano())
	if err := s.CreateShow(ctx, show, *rows, *perRow); err != nil {
		return err
	}
	// Sweep expired holds in the background, as a production cron would.
	sweepCtx, cancel := context.WithCancel(ctx)
	var freed int64
	go func() {
		t := time.NewTicker(500 * time.Millisecond)
		defer t.Stop()
		for {
			select {
			case <-sweepCtx.Done():
				return
			case <-t.C:
				n, _ := s.Sweep(sweepCtx)
				freed += n
			}
		}
	}()
	r, err := sim.Run(ctx, s, sim.Config{Show: show, Buyers: *buyers, Concurrency: *conc, Seed: *seed, Naive: *naive})
	cancel()
	if err != nil {
		return err
	}
	r.Freed = freed
	fmt.Print(r.Print())
	if !*naive && (r.Audit.DoubleSold > 0 || r.Audit.SoldWithoutBooking > 0 || r.Audit.BookingsOnUnsoldSeat > 0 ||
		r.Errors > 0 || r.Audit.TogetherBroken > 0 || r.Charged != r.Booked+r.Refunded) {
		return fmt.Errorf("invariant violated")
	}
	return nil
}

type published struct {
	Commitment lottery.Commitment `json:"commitment"`
	Randomness string             `json:"randomness"`
	Order      []string           `json:"order"`
}

func runLottery(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("lottery", flag.ExitOnError)
	n := fs.Int("entries", 50000, "registrations")
	out := fs.String("out", "sale", "output directory")
	fs.Parse(args)
	es := make([]lottery.Entry, *n)
	r := rand.New(rand.NewPCG(uint64(time.Now().UnixNano()), 7))
	for i := range es {
		id := make([]byte, 8)
		for j := range id {
			id[j] = byte(r.IntN(256))
		}
		es[i] = lottery.Entry{ID: hex.EncodeToString(id), AccountID: fmt.Sprintf("acct-%06d", i), Seats: 1 + r.IntN(6),
			Together: r.IntN(2) == 0, At: time.Now()}
	}
	b := beacon.NewDrand()
	// Commit to a round ~10 seconds in the future: its value does not exist yet.
	round := b.RoundAt(time.Now().Add(10 * time.Second))
	c := lottery.Commit("demo-sale", es, round, time.Now().UTC())
	if err := os.MkdirAll(*out, 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(*out, "entries.jsonl"), lottery.Canonical(es), 0o644); err != nil {
		return err
	}
	cb, _ := json.MarshalIndent(c, "", "  ")
	os.WriteFile(filepath.Join(*out, "commitment.json"), cb, 0o644)
	fmt.Printf("committed %d entries, sha256 %s…, waiting for drand round %d\n", c.Entries, c.ListSHA256[:16], round)
	wctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	rnd, err := b.Randomness(wctx, round)
	if err != nil {
		return err
	}
	p := published{Commitment: c, Randomness: hex.EncodeToString(rnd), Order: lottery.Order(es, rnd)}
	pb, _ := json.Marshal(p)
	if err := os.WriteFile(filepath.Join(*out, "result.json"), pb, 0o644); err != nil {
		return err
	}
	fmt.Printf("round %d randomness %s…\nfirst in queue: %s\nwrote %s\n", round, p.Randomness[:16], p.Order[0], *out)
	return nil
}

// verify is what a buyer runs: it needs only the published files and the
// public drand value (fetched again, independently of the operator).
func verify(args []string) error {
	if len(args) < 1 {
		return fmt.Errorf("usage: fairdrop verify DIR")
	}
	dir := args[0]
	var p published
	b, err := os.ReadFile(filepath.Join(dir, "result.json"))
	if err != nil {
		return err
	}
	if err := json.Unmarshal(b, &p); err != nil {
		return err
	}
	es, err := readEntries(filepath.Join(dir, "entries.jsonl"))
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	rnd, err := beacon.NewDrand().Randomness(ctx, p.Commitment.Round)
	if err != nil {
		return fmt.Errorf("fetch drand round %d: %w", p.Commitment.Round, err)
	}
	if hex.EncodeToString(rnd) != p.Randomness {
		return fmt.Errorf("operator published randomness that is not drand round %d", p.Commitment.Round)
	}
	if err := lottery.Verify(p.Commitment, es, rnd, p.Order); err != nil {
		return err
	}
	fmt.Printf("OK: %d entries match commitment %s…, order matches drand round %d\n", len(es), p.Commitment.ListSHA256[:16], p.Commitment.Round)
	return nil
}

func readEntries(path string) ([]lottery.Entry, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var es []lottery.Entry
	for _, line := range splitLines(b) {
		var e lottery.Entry
		if err := json.Unmarshal(line, &e); err != nil {
			return nil, err
		}
		es = append(es, e)
	}
	return es, nil
}

func splitLines(b []byte) [][]byte {
	var out [][]byte
	start := 0
	for i, c := range b {
		if c == '\n' {
			if i > start {
				out = append(out, b[start:i])
			}
			start = i + 1
		}
	}
	if start < len(b) {
		out = append(out, b[start:])
	}
	return out
}
