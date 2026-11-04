package analyse

import (
	"math/rand/v2"
	"strings"
	"testing"
	"time"
)

func TestParseBankLayouts(t *testing.T) {
	cases := map[string]struct {
		csv  string
		want []int64
		date string
	}{
		"withdrawal and deposit columns, preamble and footer": {
			"My Bank\nAccount,1234\n\nDate,Narration,Chq./Ref.No.,Value Dt,Withdrawal Amt.,Deposit Amt.,Closing Balance\n" +
				"03/04/26,UPI/1/SWIGGY/swiggy@icici,123,03/04/26,450.00,,1000.00\n04/04/26,SALARY,,04/04/26,,\"52,000.00\",53000.00\n\nTotals,,,,450.00,52000.00,\n",
			[]int64{-45000, 5200000}, "2026-04-03"},
		"debit and credit, long dates": {
			"Txn Date,Value Date,Description,Ref No./Cheque No.,Debit,Credit,Balance\n1 Apr 2026,1 Apr 2026,ATM WDL,,\"2,000.00\",,\n",
			[]int64{-200000}, "2026-04-01"},
		"one amount column with a Dr/Cr column": {
			"Transaction Date,Particulars,Amount,Dr/Cr\n2026-04-05,NETFLIX,649.00,DR\n2026-04-06,REFUND AMAZON,1299,CR\n",
			[]int64{-64900, 129900}, "2026-04-05"},
		"signed amounts": {
			"Date,Description,Amount (INR)\n05-04-2026,SPOTIFY,-119\n06-04-2026,INTEREST,12.5\n",
			[]int64{-11900, 1250}, "2026-04-05"},
		"amount with Dr suffix": {
			"Date,Remarks,Amount\n05/04/2026,HOTSTAR,299.00 Dr\n",
			[]int64{-29900}, "2026-04-05"},
	}
	for name, c := range cases {
		p, err := Parse(strings.NewReader(c.csv))
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if len(p.Txns) != len(c.want) {
			t.Fatalf("%s: %d txns %+v", name, len(p.Txns), p.Txns)
		}
		for i, w := range c.want {
			if p.Txns[i].Amount != w {
				t.Errorf("%s: txn %d amount %d, want %d", name, i, p.Txns[i].Amount, w)
			}
		}
		if p.Txns[0].Date != c.date {
			t.Errorf("%s: date %s, want %s (day first)", name, p.Txns[0].Date, c.date)
		}
	}
}

func TestParseReportsWhatItSkipped(t *testing.T) {
	p, err := Parse(strings.NewReader("Date,Narration,Debit,Credit\n03/04/2026,A,10,\nOpening balance,,,\n04/04/2026,B,abc,\n05/04/2026,C,0,\n"))
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Txns) != 1 || len(p.Skipped) != 3 || p.Skipped[0].Line != 3 {
		t.Fatalf("txns %+v skipped %+v", p.Txns, p.Skipped)
	}
	if _, err := Parse(strings.NewReader("a,b,c\n1,2,3\n")); err != ErrNoHeader {
		t.Fatalf("no header: %v", err)
	}
	if _, err := Parse(strings.NewReader("Date,Narration,Amount\n")); err == nil {
		t.Fatal("header without rows should fail")
	}
}

func TestMerchant(t *testing.T) {
	cases := map[string]string{
		"UPI/312345678901/SWIGGY/swiggy@icici/Payment":                         "SWIGGY",
		"UPI-NETFLIX COM-netflixupi@hdfcbank-HDFC0000499-312345678901-NETFLIX": "NETFLIX",
		"POS 416021XXXXXX4407 AMAZON PAY INDIA":                                "AMAZON",
		"NEFT CR-HDFC0000240-ACME TECHNOLOGIES PVT LTD-SALARY OCT":             "ACME TECHNOLOGIES",
		"ACH D- TP ACH ICICI PRU MF-SIP123456":                                 "ICICI PRU",
		"UPI/312345678901/HOTSTAR/hotstar@paytm/Subscription":                  "HOTSTAR",
		"//// 1234567 ////": "UNKNOWN",
	}
	for in, want := range cases {
		if got := Merchant(in); got != want {
			t.Errorf("Merchant(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestCategorise(t *testing.T) {
	cases := []struct {
		desc string
		amt  int64
		want string
	}{
		{"ATM CHG 5TH TXN", -2360, BankCharges},
		{"ATM WDL-ATM CASH", -500000, "Cash"},
		{"NEFT CR-ACME-SALARY OCT", 7250000, "Income"},
		{"SALARY ADVANCE REPAYMENT", -100000, Other}, // "salary" on money out is not income
		{"AMAZON PRIME", -149900, "Subscriptions"},
		{"POS AMAZON PAY INDIA", -129900, "Shopping"},
		{"UPI/1/SWIGGY/swiggy@icici", -45000, "Food & dining"},
		{"UPI/1/RAVI KUMAR/ravi@oksbi", -50000, Other},
		{"UPI/1/RAVI KUMAR/ravi@oksbi", 50000, MoneyIn},
		{"VIJAY SALES", -100000, Other}, // VI (Vodafone) must not match inside a word
	}
	for _, c := range cases {
		tx := Txn{Description: c.desc, Merchant: Merchant(c.desc), Amount: c.amt}
		if got, _ := Categorise(tx, nil); got != c.want {
			t.Errorf("%q: %s, want %s", c.desc, got, c.want)
		}
	}
	// a correction is learned for that merchant and wins over the keywords
	tx := Txn{Description: "UPI/1/RAVI KUMAR/ravi@oksbi", Merchant: "RAVI KUMAR", Amount: -50000}
	if got, learned := Categorise(tx, map[string]string{"RAVI KUMAR": "Rent"}); got != "Rent" || !learned {
		t.Errorf("learned rule: %s %v", got, learned)
	}
}

func sample(t *testing.T) ([]Txn, Summary) {
	t.Helper()
	p, err := Parse(strings.NewReader(SampleCSV(1, time.Date(2026, 11, 4, 0, 0, 0, 0, time.UTC))))
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Skipped) != 2 { // the two footer lines
		t.Fatalf("skipped %+v", p.Skipped)
	}
	s := Analyse(p.Txns, nil)
	return p.Txns, s
}

func findings(s Summary, kind string) []Finding {
	var out []Finding
	for _, f := range s.Findings {
		if f.Kind == kind {
			out = append(out, f)
		}
	}
	return out
}

func TestSampleStatementFindings(t *testing.T) {
	_, s := sample(t)
	if s.From != "2026-07-01" || s.To[:7] != "2026-11" || len(s.Months) != 5 || !s.Months[4].Partial || s.Months[0].Partial {
		t.Fatalf("period %s..%s months %+v", s.From, s.To, s.Months)
	}
	rec := map[string]Finding{}
	for _, f := range findings(s, "recurring") {
		rec[f.Merchant] = f
	}
	for _, m := range []string{"NETFLIX", "HOTSTAR", "SPOTIFY", "ICICI PRU", "LAKSHMI RENTALS", "AIRTEL"} {
		if f, ok := rec[m]; !ok || !f.Monthly {
			t.Errorf("recurring %s not found: %+v", m, rec)
		}
	}
	if _, ok := rec["SWIGGY"]; ok {
		t.Error("irregular food orders are not a recurring payment")
	}
	rise := findings(s, "price_rise")
	if len(rise) != 1 || rise[0].Merchant != "SPOTIFY" || rise[0].Amount != 2000 {
		t.Errorf("price rise: %+v", rise)
	}
	subs := findings(s, "subscriptions")
	if len(subs) != 1 || s.Subscriptions != 64900+29900+13900 {
		t.Errorf("subscriptions %d: %+v", s.Subscriptions, subs)
	}
	sp := findings(s, "spike")
	if len(sp) != 1 || sp[0].Category != "Food & dining" || !strings.Contains(sp[0].Title, "October 2026") {
		t.Errorf("spike: %+v", sp)
	}
	ch := map[string]bool{}
	for _, f := range findings(s, "charges") {
		ch[strings.SplitN(f.Title, ":", 2)[0]] = true
	}
	for _, k := range []string{"Minimum balance", "ATM", "SMS alerts"} {
		if !ch[k] {
			t.Errorf("charge %s not found: %v", k, ch)
		}
	}
	dup := findings(s, "duplicate")
	if len(dup) != 1 || dup[0].Merchant != "AMAZON" || dup[0].Amount != 129900 {
		t.Errorf("duplicates: %+v", dup)
	}
}

// Whatever the statement, the summary's totals agree with its rows, and
// every row lands in exactly one month and one category.
func TestSummaryAddsUp(t *testing.T) {
	for seed := range uint64(200) {
		r := rand.New(rand.NewPCG(seed, 5))
		end := time.Date(2025+r.IntN(3), time.Month(1+r.IntN(12)), 1+r.IntN(28), 0, 0, 0, 0, time.UTC)
		p, err := Parse(strings.NewReader(SampleCSV(seed, end)))
		if err != nil {
			t.Fatal(err)
		}
		var in, out int64
		for _, x := range p.Txns {
			if x.Amount > 0 {
				in += x.Amount
			} else {
				out -= x.Amount
			}
		}
		s := Analyse(p.Txns, nil)
		var mIn, mOut, mCat, cats int64
		for _, m := range s.Months {
			mIn += m.In
			mOut += m.Out
			for _, v := range m.ByCategory {
				mCat += v
			}
		}
		for _, c := range s.Categories {
			cats += c.Out
		}
		if s.MoneyIn != in || s.MoneyOut != out || mIn != in || mOut != out || mCat != out || cats != out || s.Count != len(p.Txns) {
			t.Fatalf("seed %d: in %d/%d/%d out %d/%d/%d/%d/%d", seed, in, s.MoneyIn, mIn, out, s.MoneyOut, mOut, mCat, cats)
		}
	}
}

func TestLearnedRuleRecategorises(t *testing.T) {
	txns, _ := sample(t)
	s := Analyse(txns, map[string]string{"RAVI KUMAR": "Rent"})
	n := 0
	for _, x := range txns {
		if x.Merchant == "RAVI KUMAR" {
			n++
			if x.Category != "Rent" || !x.Learned {
				t.Fatalf("%+v", x)
			}
		}
	}
	if n == 0 {
		t.Fatal("sample has no payments to RAVI KUMAR")
	}
	_ = s
}
