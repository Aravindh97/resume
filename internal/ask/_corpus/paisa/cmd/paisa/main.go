// Command paisa runs the ledger's chaos test, audit and reconciliation.
//
//	paisa run      -payments 50000          # one chaos run, then audit + recon
//	paisa killtest -kills 20                # kill -9 the run repeatedly, then finish it
//	paisa audit                             # check invariants
package main

import (
	"context"
	"encoding/csv"
	"flag"
	"fmt"
	"math/rand/v2"
	"os"
	"os/exec"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"github.com/Aravindh97/paisa/internal/chaos"
	"github.com/Aravindh97/paisa/internal/ledger"
	"github.com/Aravindh97/paisa/internal/recon"
)

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: paisa run|killtest|audit")
		os.Exit(2)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	var err error
	switch os.Args[1] {
	case "run":
		err = run(ctx, os.Args[2:])
	case "killtest":
		err = killtest(ctx, os.Args[2:])
	case "audit":
		err = audit(ctx)
	default:
		err = fmt.Errorf("unknown command %q", os.Args[1])
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func open(ctx context.Context) (*ledger.Ledger, error) {
	u := os.Getenv("DATABASE_URL")
	if u == "" {
		return nil, fmt.Errorf("set DATABASE_URL")
	}
	return ledger.Open(ctx, u)
}

func reset(ctx context.Context, l *ledger.Ledger) error {
	_, err := l.DB.Exec(ctx, `TRUNCATE postings, entries, rejections, accounts RESTART IDENTITY CASCADE`)
	return err
}

func run(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("run", flag.ExitOnError)
	c := chaos.Default()
	fs.IntVar(&c.Payments, "payments", c.Payments, "payments")
	fs.IntVar(&c.Workers, "workers", c.Workers, "concurrent workers")
	fs.Uint64Var(&c.Seed, "seed", c.Seed, "seed")
	fs.IntVar(&c.FeeShards, "fee-shards", c.FeeShards, "split the fee account into N shards (1 = hot account)")
	fresh := fs.Bool("reset", true, "start from an empty ledger")
	fs.BoolVar(&c.Rich, "rich", false, "no insufficient-funds rejections (order-independent balances)")
	bankOut := fs.String("bank-file", "", "write the simulated bank settlement CSV here")
	fs.Parse(args)
	l, err := open(ctx)
	if err != nil {
		return err
	}
	defer l.DB.Close()
	if *fresh {
		if err := reset(ctx, l); err != nil {
			return err
		}
	}
	t := time.Now()
	res, err := chaos.Run(ctx, l, c)
	if err != nil {
		return err
	}
	fmt.Printf("payments=%d workers=%d fee-shards=%d total=%s payment-phase=%s (%.0f payments/s)\n", c.Payments, c.Workers, c.FeeShards,
		time.Since(t).Round(time.Millisecond), res.PaymentPhase.Round(time.Millisecond), float64(c.Payments)/res.PaymentPhase.Seconds())
	fmt.Printf("booked=%d rejected(insufficient funds)=%d injected-crashes=%d lost-responses=%d replays-served=%d refunds=%d double-refund-attempts=%d\n",
		res.Posted, res.Rejected, res.Crashes, res.LostResponses, res.Replays, res.Refunds, res.DoubleRefundTries)
	if *bankOut != "" {
		if err := writeBank(*bankOut, res.Bank); err != nil {
			return err
		}
	}
	return finish(ctx, l, res)
}

func writeBank(path string, lines []recon.Line) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()
	w := csv.NewWriter(f)
	w.Write([]string{"reference", "amount_paise", "merchant"})
	for _, b := range lines {
		w.Write([]string{b.Reference, strconv.FormatInt(b.Amount, 10), b.Merchant})
	}
	w.Flush()
	return w.Error()
}

func finish(ctx context.Context, l *ledger.Ledger, res *chaos.Result) error {
	a, err := l.Audit(ctx)
	if err != nil {
		return err
	}
	printAudit("after payments", a)
	rep, err := recon.Reconcile(ctx, l, res.Bank)
	if err != nil {
		return err
	}
	fmt.Printf("recon: matched=%d\n", rep.Matched)
	allFound := true
	for _, k := range []recon.Kind{recon.MissingInLedger, recon.MissingInBank, recon.AmountMismatch, recon.DuplicateInBank} {
		fmt.Printf("  %-18s planted=%-4d found=%d\n", k, res.Planted[k], rep.ByKind[k])
		if res.Planted[k] != rep.ByKind[k] {
			allFound = false
		}
	}
	first, err := recon.Resolve(ctx, l, rep)
	if err != nil {
		return err
	}
	second, err := recon.Resolve(ctx, l, rep) // must change nothing
	if err != nil {
		return err
	}
	fmt.Printf("resolved: %d breaks fixed; running resolve again applied %d new entries\n", sum(first), sum(second))
	if sum(first) != len(rep.Breaks) || sum(second) != 0 {
		return fmt.Errorf("resolution not exactly-once")
	}
	a, err = l.Audit(ctx)
	if err != nil {
		return err
	}
	printAudit("after reconciliation", a)
	if !a.OK() || !allFound {
		return fmt.Errorf("invariant violated or break missed")
	}
	return nil
}

func sum(m map[recon.Kind]int) int {
	n := 0
	for _, v := range m {
		n += v
	}
	return n
}

func printAudit(label string, a *ledger.Audit) {
	fmt.Printf("audit %s: entries=%d postings=%d unbalanced=%d cache-mismatch=%d total=%d negative=%d duplicate-refs=%d (%s) -> %s\n",
		label, a.Entries, a.Postings, a.UnbalancedEntry, a.CacheMismatch, a.TotalSum, a.Negative, a.DuplicateRef,
		a.Took.Round(time.Millisecond), map[bool]string{true: "OK", false: "FAIL"}[a.OK()])
}

func audit(ctx context.Context) error {
	l, err := open(ctx)
	if err != nil {
		return err
	}
	defer l.DB.Close()
	a, err := l.Audit(ctx)
	if err != nil {
		return err
	}
	printAudit("now", a)
	if !a.OK() {
		return fmt.Errorf("invariant violated")
	}
	return nil
}

// killtest starts `paisa run -reset=false` as a child process and kills it
// with SIGKILL at random moments, many times, auditing after every kill.
// Every run uses the same seed, so each restart retries the same payments
// with the same idempotency keys, exactly what a crashed payment service does.
func killtest(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("killtest", flag.ExitOnError)
	kills := fs.Int("kills", 20, "how many times to kill the process")
	payments := fs.Int("payments", 50000, "payments")
	rich := fs.Bool("rich", false, "pass -rich to every run")
	fs.Parse(args)
	extra := []string{}
	if *rich {
		extra = append(extra, "-rich")
	}
	l, err := open(ctx)
	if err != nil {
		return err
	}
	defer l.DB.Close()
	if err := reset(ctx, l); err != nil {
		return err
	}
	self, _ := os.Executable()
	rng := rand.New(rand.NewPCG(uint64(time.Now().UnixNano()), 1))
	for i := 1; i <= *kills; i++ {
		cmd := exec.CommandContext(ctx, self, append([]string{"run", "-reset=false", "-payments", strconv.Itoa(*payments)}, extra...)...)
		cmd.Env = os.Environ()
		if err := cmd.Start(); err != nil {
			return err
		}
		wait := time.Duration(300+rng.IntN(1500)) * time.Millisecond
		done := make(chan error, 1)
		go func() { done <- cmd.Wait() }()
		select {
		case <-time.After(wait):
			cmd.Process.Signal(syscall.SIGKILL)
			<-done
		case err := <-done:
			if err != nil {
				return fmt.Errorf("run %d finished with error before kill: %w", i, err)
			}
		}
		a, err := l.Audit(ctx)
		if err != nil {
			return err
		}
		fmt.Printf("kill %2d after %4dms: entries=%-6d unbalanced=%d cache-mismatch=%d total=%d negative=%d duplicate-refs=%d -> %s\n",
			i, wait.Milliseconds(), a.Entries, a.UnbalancedEntry, a.CacheMismatch, a.TotalSum, a.Negative, a.DuplicateRef,
			map[bool]string{true: "OK", false: "FAIL"}[a.OK()])
		if !a.OK() {
			return fmt.Errorf("invariant violated after kill %d", i)
		}
	}
	fmt.Println("final run to completion (same keys):")
	cmd := exec.CommandContext(ctx, self, append([]string{"run", "-reset=false", "-payments", strconv.Itoa(*payments)}, extra...)...)
	cmd.Env, cmd.Stdout, cmd.Stderr = os.Environ(), os.Stdout, os.Stderr
	return cmd.Run()
}
