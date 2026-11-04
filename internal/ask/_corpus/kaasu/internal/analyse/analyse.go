package analyse

import (
	"fmt"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/Aravindh97/kaasu/internal/money"
)

// Finding is one thing worth a look, with the statement lines behind it.
type Finding struct {
	Kind     string  `json:"kind"` // recurring, price_rise, spike, charges, duplicate, subscriptions
	Title    string  `json:"title"`
	Detail   string  `json:"detail"`
	Amount   int64   `json:"amount"` // paise, positive
	Merchant string  `json:"merchant,omitempty"`
	Category string  `json:"category,omitempty"`
	Lines    []int   `json:"lines"`
	Monthly  bool    `json:"monthly,omitempty"`
	Next     *string `json:"next,omitempty"` // expected next date for a recurring payment
}

type Month struct {
	Month      string           `json:"month"` // YYYY-MM
	In         int64            `json:"in"`
	Out        int64            `json:"out"` // positive
	ByCategory map[string]int64 `json:"by_category"`
	Partial    bool             `json:"partial"` // the statement covers only part of it
}

type CategoryTotal struct {
	Category string `json:"category"`
	Out      int64  `json:"out"`
	Count    int    `json:"count"`
}

// Summary is everything the analysis shows except the transactions
// themselves. In privacy mode this is all that leaves the browser.
type Summary struct {
	From          string          `json:"from"`
	To            string          `json:"to"`
	Count         int             `json:"count"`
	MoneyIn       int64           `json:"money_in"`
	MoneyOut      int64           `json:"money_out"` // positive
	Months        []Month         `json:"months"`
	Categories    []CategoryTotal `json:"categories"`
	Findings      []Finding       `json:"findings"`
	Subscriptions int64           `json:"subscriptions_monthly"` // detected subscriptions, per month
}

// Analyse categorises the transactions (in place) and summarises them.
func Analyse(txns []Txn, learned map[string]string) Summary {
	for i := range txns {
		txns[i].Category, txns[i].Learned = Categorise(txns[i], learned)
	}
	sort.SliceStable(txns, func(i, j int) bool { return txns[i].Date < txns[j].Date })
	s := Summary{Count: len(txns), Months: []Month{}, Categories: []CategoryTotal{}, Findings: []Finding{}}
	if len(txns) == 0 {
		return s
	}
	s.From, s.To = txns[0].Date, txns[len(txns)-1].Date
	months := map[string]*Month{}
	cats := map[string]*CategoryTotal{}
	for _, t := range txns {
		m := months[t.Date[:7]]
		if m == nil {
			m = &Month{Month: t.Date[:7], ByCategory: map[string]int64{}}
			months[t.Date[:7]] = m
		}
		if t.Amount > 0 {
			s.MoneyIn += t.Amount
			m.In += t.Amount
			continue
		}
		s.MoneyOut -= t.Amount
		m.Out -= t.Amount
		m.ByCategory[t.Category] -= t.Amount
		c := cats[t.Category]
		if c == nil {
			c = &CategoryTotal{Category: t.Category}
			cats[t.Category] = c
		}
		c.Out -= t.Amount
		c.Count++
	}
	for _, m := range months {
		s.Months = append(s.Months, *m)
	}
	sort.Slice(s.Months, func(i, j int) bool { return s.Months[i].Month < s.Months[j].Month })
	first, _ := time.Parse(time.DateOnly, s.From)
	last, _ := time.Parse(time.DateOnly, s.To)
	if first.Day() > 5 {
		s.Months[0].Partial = true
	}
	if last.AddDate(0, 0, 6).Month() == last.Month() { // ends more than 6 days before the month does
		s.Months[len(s.Months)-1].Partial = true
	}
	for _, c := range cats {
		s.Categories = append(s.Categories, *c)
	}
	sort.Slice(s.Categories, func(i, j int) bool {
		if s.Categories[i].Out != s.Categories[j].Out {
			return s.Categories[i].Out > s.Categories[j].Out
		}
		return s.Categories[i].Category < s.Categories[j].Category
	})
	rec, subs := recurring(txns)
	s.Findings = append(s.Findings, rec...)
	s.Subscriptions = subs
	s.Findings = append(s.Findings, spikes(s.Months)...)
	s.Findings = append(s.Findings, charges(txns)...)
	s.Findings = append(s.Findings, duplicates(txns)...)
	return s
}

func days(a, b string) int {
	x, _ := time.Parse(time.DateOnly, a)
	y, _ := time.Parse(time.DateOnly, b)
	return int(y.Sub(x).Hours() / 24)
}

// recurring finds payments to the same merchant at a steady interval and a
// steady amount: at least three, 26 to 35 days apart (monthly) or 6 to 8
// (weekly), each within 15% of the median amount.
func recurring(txns []Txn) ([]Finding, int64) {
	by := map[string][]Txn{}
	for _, t := range txns {
		if t.Amount < 0 && t.Merchant != "UNKNOWN" && t.Category != "Cash" && t.Category != BankCharges {
			by[t.Merchant] = append(by[t.Merchant], t)
		}
	}
	var out []Finding
	var subsMonthly int64
	var subNames []string
	var subLines []int
	keys := make([]string, 0, len(by))
	for k := range by {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, merchant := range keys {
		ts := by[merchant]
		if len(ts) < 3 {
			continue
		}
		lo, hi := 10000, 0
		for i := 1; i < len(ts); i++ {
			d := days(ts[i-1].Date, ts[i].Date)
			lo, hi = min(lo, d), max(hi, d)
		}
		monthly := lo >= 26 && hi <= 35
		weekly := lo >= 6 && hi <= 8
		if !monthly && !weekly {
			continue
		}
		amts := make([]int64, len(ts))
		for i, t := range ts {
			amts[i] = -t.Amount
		}
		// Steady: every earlier payment within 15% of their median. The latest
		// may differ by up to half, so a price change doesn't hide the pattern.
		earlier := amts[:len(amts)-1]
		med := slices.Clone(earlier)
		slices.Sort(med)
		m := med[len(med)/2]
		steady := abs(amts[len(amts)-1]-m)*2 <= m
		fixed := true // every earlier payment the same amount, like a subscription
		for _, a := range earlier {
			if abs(a-m)*100 > m*15 {
				steady = false
			}
			fixed = fixed && a == earlier[0]
		}
		if !steady {
			continue
		}
		lines := make([]int, len(ts))
		for i, t := range ts {
			lines[i] = t.Line
		}
		lastT, _ := time.Parse(time.DateOnly, ts[len(ts)-1].Date)
		every, per := "every month", lastT.AddDate(0, 1, 0)
		if weekly {
			every, per = "every week", lastT.AddDate(0, 0, 7)
		}
		next := per.Format(time.DateOnly)
		last := amts[len(amts)-1]
		cat := ts[len(ts)-1].Category
		out = append(out, Finding{Kind: "recurring", Title: fmt.Sprintf("%s: %s %s", merchant, money.Format(last), every),
			Detail: fmt.Sprintf("%d payments since %s. The next is likely around %s.", len(ts), ts[0].Date, next),
			Amount: last, Merchant: merchant, Category: cat, Lines: lines, Monthly: monthly, Next: &next})
		// A price rise only means something for a fixed price; bills vary anyway.
		if prev := amts[len(amts)-2]; fixed && last > prev && last-prev >= 1000 {
			out = append(out, Finding{Kind: "price_rise", Title: fmt.Sprintf("%s went up from %s to %s", merchant, money.Format(prev), money.Format(last)),
				Detail: fmt.Sprintf("A rise of %s %s, %s a year.", money.Format(last-prev), every, money.Format((last-prev)*map[bool]int64{true: 12, false: 52}[monthly])),
				Amount: last - prev, Merchant: merchant, Category: cat, Lines: lines[len(lines)-2:]})
		}
		if cat == "Subscriptions" {
			if monthly {
				subsMonthly += last
			} else {
				subsMonthly += last * 52 / 12
			}
			subNames = append(subNames, merchant)
			subLines = append(subLines, lines[len(lines)-1])
		}
	}
	if len(subNames) > 0 {
		out = append(out, Finding{Kind: "subscriptions", Title: fmt.Sprintf("Subscriptions: %s a month across %d services", money.Format(subsMonthly), len(subNames)),
			Detail: fmt.Sprintf("%s. Together they cost %s a year. Kaasu can't tell which ones you still use, so check each one.", strings.Join(subNames, ", "), money.Format(subsMonthly*12)),
			Amount: subsMonthly, Category: "Subscriptions", Lines: subLines, Monthly: true})
	}
	return out, subsMonthly
}

// spikes compares each category's spending in the latest full month with
// its average over the earlier months: at least 1.5 times and ₹1,000 more.
func spikes(months []Month) []Finding {
	full := []Month{}
	for _, m := range months {
		if !m.Partial {
			full = append(full, m)
		}
	}
	if len(full) < 3 {
		return nil
	}
	latest, before := full[len(full)-1], full[:len(full)-1]
	var out []Finding
	cats := make([]string, 0, len(latest.ByCategory))
	for c := range latest.ByCategory {
		cats = append(cats, c)
	}
	sort.Strings(cats)
	for _, c := range cats {
		if c == "Investments" || c == "Transfers" || c == "Credit card bill" {
			continue // moving money, not spending it
		}
		var sum int64
		for _, m := range before {
			sum += m.ByCategory[c]
		}
		avg := sum / int64(len(before))
		now := latest.ByCategory[c]
		if now*2 >= avg*3 && now-avg >= 100000 {
			out = append(out, Finding{Kind: "spike", Title: fmt.Sprintf("%s: %s in %s, against %s a month before", c, money.Format(now), monthName(latest.Month), money.Format(avg)),
				Detail: fmt.Sprintf("%s more than your average over the previous %d months.", money.Format(now-avg), len(before)),
				Amount: now - avg, Category: c, Lines: []int{}})
		}
	}
	return out
}

func monthName(ym string) string {
	t, err := time.Parse("2006-01", ym)
	if err != nil {
		return ym
	}
	return t.Format("January 2006")
}

// What each kind of bank charge is and how people usually avoid it. These
// are general rules; the bank's own terms decide.
var chargeKinds = []struct {
	kind, tip string
	words     []string
}{
	{"Minimum balance", "Charged when the average balance falls below the account's minimum. Keeping the minimum, or moving to a zero-balance account, avoids it.", []string{"MIN BAL", "NON MAINT", "AMB", "MAB"}},
	{"Bounced payment", "Charged when an EMI, SIP or cheque bounces for low balance. Keeping enough in the account before those dates avoids it.", []string{"RTN", "RETURN", "BOUNCE", "DISHONOUR"}},
	{"ATM", "Charged after the free monthly ATM withdrawals. Fewer, larger withdrawals, or your own bank's ATMs, avoid it.", []string{"ATM"}},
	{"SMS alerts", "A fee for SMS alerts. Ask whether your bank offers app or email alerts without it.", []string{"SMS"}},
	{"Card annual fee", "Some cards waive the annual fee above a yearly spend; check your card's terms.", []string{"ANNUAL FEE", "ANNUAL", "AMC"}},
	{"Late payment", "Paying the full bill by the due date avoids it, and the interest that comes with it.", []string{"LATE"}},
}

func charges(txns []Txn) []Finding {
	type agg struct {
		total int64
		lines []int
		tip   string
	}
	by := map[string]*agg{}
	order := []string{}
	for _, t := range txns {
		if t.Category != BankCharges || t.Amount >= 0 {
			continue
		}
		kind, tip := "Other bank charges", "Ask the bank what it is for; some charges are reversed on request."
		w := words(t.Description)
		for _, k := range chargeKinds {
			for _, x := range k.words {
				if strings.Contains(w, " "+x+" ") {
					kind, tip = k.kind, k.tip
					goto found
				}
			}
		}
	found:
		a := by[kind]
		if a == nil {
			a = &agg{tip: tip}
			by[kind] = a
			order = append(order, kind)
		}
		a.total -= t.Amount
		a.lines = append(a.lines, t.Line)
	}
	sort.SliceStable(order, func(i, j int) bool { return by[order[i]].total > by[order[j]].total })
	var out []Finding
	for _, k := range order {
		a := by[k]
		n := "once"
		if len(a.lines) > 1 {
			n = fmt.Sprintf("%d times", len(a.lines))
		}
		out = append(out, Finding{Kind: "charges", Title: fmt.Sprintf("%s: %s, charged %s", k, money.Format(a.total), n), Detail: a.tip, Amount: a.total, Category: BankCharges, Lines: a.lines})
	}
	return out
}

// duplicates: the same amount to the same merchant within a day, ₹100 or
// more, not cash. Often a retried payment that went through twice.
func duplicates(txns []Txn) []Finding {
	var out []Finding
	used := map[int]bool{}
	for i, a := range txns {
		if a.Amount > -10000 || a.Category == "Cash" || a.Merchant == "UNKNOWN" || used[i] {
			continue
		}
		for j := i + 1; j < len(txns) && days(a.Date, txns[j].Date) <= 1; j++ {
			b := txns[j]
			if !used[j] && b.Amount == a.Amount && b.Merchant == a.Merchant {
				used[i], used[j] = true, true
				out = append(out, Finding{Kind: "duplicate", Title: fmt.Sprintf("Possible double payment: %s to %s", money.Format(-a.Amount), a.Merchant),
					Detail: fmt.Sprintf("Paid on %s and again on %s. If you meant to pay once, ask the merchant or bank for a refund.", a.Date, b.Date),
					Amount: -a.Amount, Merchant: a.Merchant, Category: a.Category, Lines: []int{a.Line, b.Line}})
				break
			}
		}
	}
	return out
}
