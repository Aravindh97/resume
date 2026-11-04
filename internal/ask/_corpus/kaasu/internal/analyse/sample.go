package analyse

import (
	"fmt"
	"math/rand/v2"
	"sort"
	"strings"
	"time"

	"github.com/Aravindh97/kaasu/internal/money"
)

// SampleCSV writes a made-up bank statement in the layout many Indian banks
// use (account details on top, Withdrawal and Deposit columns), covering
// the four full months before `end` and the first days of end's month.
// It has known things to find, which the tests check:
//   - Netflix ₹649, Hotstar ₹299 and Spotify ₹119 monthly; Spotify rises to ₹139
//   - a ₹5,000 SIP and a ₹8,450 EMI every month
//   - food delivery roughly doubles in the last full month
//   - SMS, minimum-balance and ATM charges
//   - one Amazon order paid twice on the same day
func SampleCSV(seed uint64, end time.Time) string {
	r := rand.New(rand.NewPCG(seed, 11))
	start := time.Date(end.Year(), end.Month()-4, 1, 0, 0, 0, 0, time.UTC)
	stop := time.Date(end.Year(), end.Month(), 4, 0, 0, 0, 0, time.UTC)
	type row struct {
		d    time.Time
		desc string
		amt  int64
	}
	var rows []row
	add := func(d time.Time, desc string, amt int64) {
		if !d.Before(start) && d.Before(stop) {
			rows = append(rows, row{d, desc, amt})
		}
	}
	ref := func() string { return fmt.Sprintf("%012d", r.Int64N(1e12)) }
	people := []string{"RAVI KUMAR/ravi.k@oksbi", "PRIYA S/priya92@okaxis", "MURUGAN STORES/murugan@ybl", "AMMA/9876501234@ibl"}
	lastFull := time.Date(end.Year(), end.Month()-1, 1, 0, 0, 0, 0, time.UTC)
	for m := start; m.Before(stop); m = m.AddDate(0, 1, 0) {
		day := func(d int) time.Time { return time.Date(m.Year(), m.Month(), d, 0, 0, 0, 0, time.UTC) }
		add(day(1), "NEFT CR-HDFC0000240-ACME TECHNOLOGIES PVT LTD-SALARY "+strings.ToUpper(m.Format("Jan")), 7250000)
		add(day(3), "UPI/"+ref()+"/LAKSHMI RENTALS/rent.lakshmi@okicici/RENT", -1800000)
		add(day(5), "UPI-NETFLIX COM-netflixupi@hdfcbank-HDFC0000499-"+ref()+"-NETFLIX", -64900)
		add(day(7), "ACH D- TP ACH ICICI PRU MF-SIP"+ref()[:6], -500000)
		add(day(10), "EMI 4471023 CHQ S4471023 BAJAJ FIN", -845000)
		add(day(12), "UPI/"+ref()+"/HOTSTAR/hotstar@paytm/Subscription", -29900)
		spot := int64(-11900)
		if !m.Before(lastFull) {
			spot = -13900
		}
		add(day(18), "UPI/"+ref()+"/SPOTIFY/spotify.bdsi@icici/Payment", spot)
		add(day(20), "UPI/"+ref()+"/TANGEDCO/tneb@sbi/Electricity bill", -int64(140000+r.IntN(60000)))
		add(day(22), "UPI/"+ref()+"/AIRTEL/airtel@paytm/Recharge", -59900)
		orders := 5 + r.IntN(3)
		if m.Equal(lastFull) {
			orders = 14 // the spike
		}
		for range orders {
			add(day(1+r.IntN(27)), "UPI/"+ref()+"/SWIGGY/swiggy@icici/Payment", -int64(25000+r.IntN(35000)))
		}
		for range 4 + r.IntN(3) {
			add(day(1+r.IntN(27)), "POS 416021XXXXXX4407 DMART AVINASHI ROAD", -int64(80000+r.IntN(220000)))
		}
		for range 2 + r.IntN(2) {
			add(day(1+r.IntN(27)), "UPI/"+ref()+"/HPCL FUEL/hpcl.fuel@sbi/Fuel", -int64(150000+r.IntN(100000)))
		}
		for range 3 {
			add(day(1+r.IntN(27)), "UPI/"+ref()+"/"+people[r.IntN(len(people))]+"/Sent", -int64(20000+r.IntN(300000)))
		}
		add(day(15), "ATM WDL-ATM CASH 9921 TIRUPPUR MAIN", -500000)
		if m.Month()%3 == 0 {
			add(day(28), "SMS CHARGES FOR QTR", -1770)
		}
		if m.Equal(start.AddDate(0, 1, 0)) {
			add(day(26), "MIN BAL CHGS NON MAINT OF AMB", -59000)
			add(day(27), "ATM CHG 5TH TXN OTHER BANK", -2360)
			add(day(14), "POS 416021XXXXXX4407 AMAZON PAY INDIA", -129900)
			add(day(14), "POS 416021XXXXXX4407 AMAZON PAY INDIA", -129900) // paid twice
		}
	}
	sort.SliceStable(rows, func(i, j int) bool { return rows[i].d.Before(rows[j].d) })
	var b strings.Builder
	b.WriteString("Sample Bank Ltd,,,,,,\n")
	b.WriteString("Statement of account,,,,,,\n")
	b.WriteString("Account holder,ANBU SELVAN (sample data),,,,,\n")
	b.WriteString(fmt.Sprintf("Period,%s to %s,,,,,\n", start.Format("02/01/2006"), stop.AddDate(0, 0, -1).Format("02/01/2006")))
	b.WriteString(",,,,,,\n")
	b.WriteString("Date,Narration,Chq./Ref.No.,Value Dt,Withdrawal Amt.,Deposit Amt.,Closing Balance\n")
	bal := int64(4500000)
	plain := func(p int64) string { return strings.NewReplacer("₹", "").Replace(money.Format(p)) }
	for _, x := range rows {
		bal += x.amt
		out, in := "", ""
		if x.amt < 0 {
			out = plain(-x.amt)
		} else {
			in = plain(x.amt)
		}
		b.WriteString(fmt.Sprintf("%s,%q,%s,%s,%q,%q,%q\n", x.d.Format("02/01/06"), x.desc, ref()[:10], x.d.Format("02/01/06"), out, in, plain(bal)))
	}
	b.WriteString(",,,,,,\n")
	b.WriteString("STATEMENT SUMMARY,,,,,,\n")
	b.WriteString("This is a made-up statement for trying Kaasu. Not a real account.,,,,,,\n")
	return b.String()
}
