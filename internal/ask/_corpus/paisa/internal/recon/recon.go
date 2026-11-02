// Package recon reconciles a bank or UPI settlement file against the ledger.
//
// Each settlement line says "the bank moved this amount for this UPI
// reference". Each ledger payment has the same reference. Recon matches them
// and classifies every difference:
//
//	missing_in_ledger   the bank settled a payment we never recorded
//	                    (our request timed out after the bank succeeded)
//	missing_in_bank     we recorded a payment the bank never settled
//	                    (the bank failed after we booked, or settles late)
//	amount_mismatch     both sides have it, amounts differ
//	duplicate_in_bank   the bank settled the same reference twice
//
// Each class has a resolution, applied through the ledger with its own
// idempotency key, so running recon twice never fixes anything twice.
package recon

import (
	"context"
	"encoding/csv"
	"fmt"
	"io"
	"sort"
	"strconv"

	"github.com/Aravindh97/paisa/internal/ledger"
)

type Line struct {
	Reference string
	Amount    int64 // paise
	Merchant  string
}

// ReadCSV reads "reference,amount_paise,merchant" with a header row.
func ReadCSV(r io.Reader) ([]Line, error) {
	cr := csv.NewReader(r)
	rows, err := cr.ReadAll()
	if err != nil {
		return nil, err
	}
	var out []Line
	for i, row := range rows {
		if i == 0 {
			continue
		}
		if len(row) < 3 {
			return nil, fmt.Errorf("line %d: want 3 columns", i+1)
		}
		amt, err := strconv.ParseInt(row[1], 10, 64)
		if err != nil {
			return nil, fmt.Errorf("line %d: amount: %w", i+1, err)
		}
		out = append(out, Line{Reference: row[0], Amount: amt, Merchant: row[2]})
	}
	return out, nil
}

type Kind string

const (
	MissingInLedger Kind = "missing_in_ledger"
	MissingInBank   Kind = "missing_in_bank"
	AmountMismatch  Kind = "amount_mismatch"
	DuplicateInBank Kind = "duplicate_in_bank"
)

type Break struct {
	Kind      Kind   `json:"kind"`
	Reference string `json:"reference"`
	Bank      int64  `json:"bank_paise"`
	Ledger    int64  `json:"ledger_paise"`
	Merchant  string `json:"merchant"`
}

type Report struct {
	Matched int          `json:"matched"`
	Breaks  []Break      `json:"breaks"`
	ByKind  map[Kind]int `json:"by_kind"`
	Fixed   map[Kind]int `json:"fixed,omitempty"`
}

type ledgerPayment struct {
	amount   int64
	merchant string
}

// Payments loads every payment's reference, amount and merchant. The amount
// is what the merchant account was credited.
func Payments(ctx context.Context, l *ledger.Ledger) (map[string]ledgerPayment, error) {
	rows, err := l.DB.Query(ctx, `SELECT e.reference, p.amount, p.account_id FROM entries e
		JOIN postings p ON p.entry_id = e.id
		WHERE e.kind='payment' AND e.reference <> '' AND p.account_id LIKE 'merchant:%' AND p.amount > 0`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]ledgerPayment{}
	for rows.Next() {
		var ref, acct string
		var amt int64
		if err := rows.Scan(&ref, &amt, &acct); err != nil {
			return nil, err
		}
		out[ref] = ledgerPayment{amt, acct}
	}
	return out, rows.Err()
}

func Reconcile(ctx context.Context, l *ledger.Ledger, bank []Line) (*Report, error) {
	led, err := Payments(ctx, l)
	if err != nil {
		return nil, err
	}
	rep := &Report{ByKind: map[Kind]int{}}
	seen := map[string]int{}
	for _, b := range bank {
		seen[b.Reference]++
		if seen[b.Reference] > 1 {
			rep.Breaks = append(rep.Breaks, Break{DuplicateInBank, b.Reference, b.Amount, led[b.Reference].amount, b.Merchant})
			continue
		}
		p, ok := led[b.Reference]
		switch {
		case !ok:
			rep.Breaks = append(rep.Breaks, Break{MissingInLedger, b.Reference, b.Amount, 0, b.Merchant})
		case p.amount != b.Amount:
			rep.Breaks = append(rep.Breaks, Break{AmountMismatch, b.Reference, b.Amount, p.amount, p.merchant})
		default:
			rep.Matched++
		}
	}
	for ref, p := range led {
		if seen[ref] == 0 {
			rep.Breaks = append(rep.Breaks, Break{MissingInBank, ref, 0, p.amount, p.merchant})
		}
	}
	sort.Slice(rep.Breaks, func(i, j int) bool {
		if rep.Breaks[i].Kind != rep.Breaks[j].Kind {
			return rep.Breaks[i].Kind < rep.Breaks[j].Kind
		}
		return rep.Breaks[i].Reference < rep.Breaks[j].Reference
	})
	for _, b := range rep.Breaks {
		rep.ByKind[b.Kind]++
	}
	return rep, nil
}

// Resolve applies the standard fix for each break, idempotently:
//
//	missing_in_ledger -> book the payment from the bank's clearing account
//	                     (the bank holds the money; we owe it to the merchant)
//	missing_in_bank   -> reverse our payment into a suspense account for
//	                     investigation (never silently delete history)
//	amount_mismatch   -> post the difference between merchant and suspense
//	duplicate_in_bank -> book the extra settlement to suspense, to be
//	                     returned to the bank
func Resolve(ctx context.Context, l *ledger.Ledger, rep *Report) (map[Kind]int, error) {
	fixed := map[Kind]int{}
	for _, b := range rep.Breaks {
		merchant := b.Merchant
		if merchant == "" {
			merchant = "merchant:unknown"
		}
		var req ledger.Request
		switch b.Kind {
		case MissingInLedger:
			req = ledger.Request{Key: "recon:add:" + b.Reference, Kind: "payment", Reference: b.Reference,
				Postings: []ledger.Posting{ledger.P("bank:clearing", -b.Bank), ledger.P(merchant, b.Bank)}}
		case MissingInBank:
			req = ledger.Request{Key: "recon:suspend:" + b.Reference, Kind: "recon",
				Postings: []ledger.Posting{ledger.P(merchant, -b.Ledger), ledger.P("suspense", b.Ledger)}}
		case AmountMismatch:
			d := b.Bank - b.Ledger
			req = ledger.Request{Key: "recon:adjust:" + b.Reference, Kind: "recon",
				Postings: []ledger.Posting{ledger.P(merchant, d), ledger.P("suspense", -d)}}
		case DuplicateInBank:
			req = ledger.Request{Key: "recon:dup:" + b.Reference, Kind: "recon",
				Postings: []ledger.Posting{ledger.P("bank:clearing", -b.Bank), ledger.P("suspense", b.Bank)}}
		}
		out, err := l.Post(ctx, req)
		if err != nil {
			return fixed, fmt.Errorf("%s %s: %w", b.Kind, b.Reference, err)
		}
		if out.Rejected != "" {
			return fixed, fmt.Errorf("%s %s rejected: %s", b.Kind, b.Reference, out.Rejected)
		}
		if !out.Replay {
			fixed[b.Kind]++
		}
	}
	return fixed, nil
}
