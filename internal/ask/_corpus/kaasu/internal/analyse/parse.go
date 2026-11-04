// Package analyse reads bank statements and finds things worth a look:
// recurring payments and price rises, spending spikes, bank charges and
// possible duplicates. It is plain Go with no I/O beyond the reader it is
// given, so the same code runs on the server and, compiled to WebAssembly,
// in the browser (privacy mode).
//
// Everything it reports comes from fixed rules. It is not financial advice.
package analyse

import (
	"encoding/csv"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/Aravindh97/kaasu/internal/money"
)

// Txn is one statement line. Amounts are paise; negative is money out.
type Txn struct {
	Line        int    `json:"line"` // line number in the file, for pointing back
	Date        string `json:"date"` // YYYY-MM-DD
	Description string `json:"description"`
	Merchant    string `json:"merchant"` // normalised name used for grouping and learning
	Amount      int64  `json:"amount"`
	Category    string `json:"category"`
	Learned     bool   `json:"learned"` // category came from the user's own correction
}

// Skip is a line that was not a transaction, and why.
type Skip struct {
	Line   int    `json:"line"`
	Reason string `json:"reason"`
}

type Parsed struct {
	Txns    []Txn  `json:"txns"`
	Skipped []Skip `json:"skipped"`
	Columns string `json:"columns"` // which columns were used, for the user to check
}

const MaxRows = 20000

var (
	ErrNoHeader = errors.New("couldn't find the column headings: the file needs a date column, a description column, and debit/credit or amount columns")
	ErrTooMany  = fmt.Errorf("more than %d rows; split the statement into smaller files", MaxRows)
)

// Column names banks use, in order of preference.
var (
	dateNames   = []string{"txn date", "transaction date", "tran date", "date", "posting date", "value date"}
	descNames   = []string{"narration", "description", "particulars", "transaction details", "details", "remarks"}
	debitNames  = []string{"withdrawal amt", "withdrawal amount", "withdrawal", "withdrawals", "debit amount", "debit", "dr", "money out"}
	creditNames = []string{"deposit amt", "deposit amount", "deposit", "deposits", "credit amount", "credit", "cr", "money in"}
	amountNames = []string{"amount", "amount (inr)", "transaction amount", "amount(inr)"}
	typeNames   = []string{"dr/cr", "cr/dr", "type", "debit/credit", "transaction type"}
)

func norm(h string) string {
	h = strings.ToLower(strings.TrimSpace(h))
	h = strings.ReplaceAll(h, ".", "")
	return strings.Join(strings.Fields(h), " ")
}

func find(row []string, names []string) int {
	for _, n := range names {
		for i, h := range row {
			if norm(h) == n {
				return i
			}
		}
	}
	return -1
}

var dateLayouts = []string{"02/01/2006", "2/1/2006", "02-01-2006", "2-1-2006", "2006-01-02", "02 Jan 2006", "2 Jan 2006", "02-Jan-2006", "2-Jan-2006",
	"02 Jan 06", "02-Jan-06", "02/01/06", "02-01-06", "Jan 2, 2006", "02.01.2006"}

// ParseDate reads a statement date. Indian statements put the day first, so
// 03/04/2026 is 3 April.
func ParseDate(s string) (time.Time, error) {
	s = strings.TrimSpace(s)
	if len(s) > 10 && (s[10] == ' ' || s[10] == 'T') && s[4] == '-' { // 2026-04-03 10:20:00
		s = s[:10]
	}
	for _, l := range dateLayouts {
		if t, err := time.Parse(l, s); err == nil {
			return t, nil
		}
	}
	return time.Time{}, fmt.Errorf("%q is not a date", s)
}

// Parse reads a CSV statement. Banks put account details above the header
// and totals below the transactions, so it looks for the header row and
// skips (and reports) lines that are not transactions.
func Parse(r io.Reader) (*Parsed, error) {
	cr := csv.NewReader(r)
	cr.FieldsPerRecord = -1
	cr.LazyQuotes = true
	cr.TrimLeadingSpace = true
	var rows [][]string
	for {
		rec, err := cr.Read()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("not a readable CSV file: %w", err)
		}
		rows = append(rows, rec)
		if len(rows) > MaxRows+50 {
			return nil, ErrTooMany
		}
	}
	hdr := -1
	var di, ni, dbi, cri, ai, ti int
	for i := 0; i < len(rows) && i < 40; i++ {
		di, ni = find(rows[i], dateNames), find(rows[i], descNames)
		dbi, cri, ai, ti = find(rows[i], debitNames), find(rows[i], creditNames), find(rows[i], amountNames), find(rows[i], typeNames)
		if di >= 0 && ni >= 0 && (dbi >= 0 && cri >= 0 || ai >= 0) {
			hdr = i
			break
		}
	}
	if hdr < 0 {
		return nil, ErrNoHeader
	}
	h := rows[hdr]
	p := &Parsed{}
	if dbi >= 0 && cri >= 0 {
		p.Columns = fmt.Sprintf("date: %q, description: %q, money out: %q, money in: %q", h[di], h[ni], h[dbi], h[cri])
		ai = -1
	} else {
		p.Columns = fmt.Sprintf("date: %q, description: %q, amount: %q", h[di], h[ni], h[ai])
		if ti >= 0 {
			p.Columns += fmt.Sprintf(", type: %q", h[ti])
		}
	}
	cell := func(row []string, i int) string {
		if i < 0 || i >= len(row) {
			return ""
		}
		return strings.TrimSpace(row[i])
	}
	for i := hdr + 1; i < len(rows); i++ {
		row, line := rows[i], i+1
		empty := true
		for _, c := range row {
			if strings.TrimSpace(c) != "" {
				empty = false
				break
			}
		}
		if empty {
			continue
		}
		d, err := ParseDate(cell(row, di))
		if err != nil {
			p.Skipped = append(p.Skipped, Skip{line, "no date: not a transaction"})
			continue
		}
		var amt int64
		if ai < 0 {
			out, in := cell(row, dbi), cell(row, cri)
			var o, n int64
			if out != "" {
				if o, err = money.Parse(out); err != nil {
					p.Skipped = append(p.Skipped, Skip{line, "unreadable money-out amount " + out})
					continue
				}
			}
			if in != "" {
				if n, err = money.Parse(in); err != nil {
					p.Skipped = append(p.Skipped, Skip{line, "unreadable money-in amount " + in})
					continue
				}
			}
			amt = n - abs(o)
		} else {
			a := cell(row, ai)
			kind := strings.ToUpper(cell(row, ti))
			// "1,250.00 Dr" in a single column
			if f := strings.Fields(a); len(f) == 2 && (strings.EqualFold(f[1], "dr") || strings.EqualFold(f[1], "cr")) {
				a, kind = f[0], strings.ToUpper(f[1])
			}
			if amt, err = money.Parse(a); err != nil {
				p.Skipped = append(p.Skipped, Skip{line, "unreadable amount " + a})
				continue
			}
			switch {
			case strings.HasPrefix(kind, "D"):
				amt = -abs(amt)
			case strings.HasPrefix(kind, "C"):
				amt = abs(amt)
			}
		}
		if amt == 0 {
			p.Skipped = append(p.Skipped, Skip{line, "zero amount"})
			continue
		}
		desc := cell(row, ni)
		p.Txns = append(p.Txns, Txn{Line: line, Date: d.Format(time.DateOnly), Description: desc, Merchant: Merchant(desc), Amount: amt})
		if len(p.Txns) > MaxRows {
			return nil, ErrTooMany
		}
	}
	if len(p.Txns) == 0 {
		return nil, errors.New("found the column headings but no transactions under them")
	}
	return p, nil
}

func abs(x int64) int64 {
	if x < 0 {
		return -x
	}
	return x
}
