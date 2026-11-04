package analyse

import (
	"strings"
	"unicode"
)

// Words in bank narrations that say how money moved, not who to.
var noise = map[string]bool{
	"UPI": true, "POS": true, "NEFT": true, "IMPS": true, "RTGS": true, "ACH": true, "NACH": true, "ECS": true, "DR": true, "CR": true,
	"TO": true, "FROM": true, "BY": true, "TRANSFER": true, "TRF": true, "PAYMENT": true, "PAY": true, "REF": true, "TXN": true, "INB": true,
	"MB": true, "IB": true, "BIL": true, "BILLPAY": true, "ONL": true, "VPS": true, "VIN": true, "MMT": true, "P2A": true, "P2M": true,
	"CARD": true, "DEBIT": true, "CREDIT": true, "PVT": true, "LTD": true, "LIMITED": true, "INDIA": true, "PRIVATE": true, "COM": true,
	"WWW": true, "IN": true, "TP": true, "SI": true, "AUTOPAY": true, "MANDATE": true, "COLLECT": true, "REQ": true, "SENT": true, "VIA": true,
	"YBL": true, "OKICICI": true, "OKAXIS": true, "OKHDFCBANK": true, "OKSBI": true, "IBL": true, "AXL": true,
}

// Merchant reduces a narration to a short name for grouping: up to two
// words that aren't payment jargon, reference numbers or bank codes, taken
// from the first part of the narration that has any. Narrations are split
// into parts at "/" and "-", so the payee's name isn't mixed with the note:
// "UPI/312345678901/HOTSTAR/hotstar@paytm/Subscription" becomes "HOTSTAR".
func Merchant(desc string) string {
	for _, part := range strings.FieldsFunc(strings.ToUpper(desc), func(r rune) bool { return r == '/' || r == '-' || r == '|' || r == ':' }) {
		var words []string
		for _, f := range strings.FieldsFunc(part, func(r rune) bool {
			return !(unicode.IsLetter(r) || unicode.IsDigit(r) || r == '@' || r == '&')
		}) {
			if at := strings.IndexByte(f, '@'); at >= 0 { // a UPI handle: name@bank
				f = strings.TrimRight(f[:at], "0123456789")
			}
			digits := 0
			for _, r := range f {
				if unicode.IsDigit(r) {
					digits++
				}
			}
			if len(f) < 2 || noise[f] || digits >= 3 || digits*2 > len(f) || strings.Contains(f, "XXXX") {
				continue
			}
			words = append(words, f)
			if len(words) == 2 {
				break
			}
		}
		if len(words) > 0 {
			return strings.Join(words, " ")
		}
	}
	return "UNKNOWN"
}

// Categories, checked in order: the first match wins. Bank charges come
// first so "ATM CHG" is a charge, not cash; subscriptions come before
// shopping so "AMAZON PRIME" is a subscription.
var categoryRules = []struct {
	cat   string
	words []string
}{
	{BankCharges, []string{"CHARGES", "CHARGE", "CHGS", "CHG", "PENALTY", "MIN BAL", "NON MAINT", "ANNUAL FEE", "LATE FEE", "LATE PAYMENT", "RTN CHG", "BOUNCE"}},
	{"Income", []string{"SALARY", "SAL", "PAYROLL", "INT PD", "INTEREST", "DIVIDEND"}},
	{"Refunds", []string{"REFUND", "REVERSAL", "CASHBACK"}},
	{"Investments", []string{"SIP", "MUTUAL", "MF", "ZERODHA", "GROWW", "NPS", "PPF", "KUVERA"}},
	{"Loans & EMI", []string{"EMI", "LOAN", "BAJAJ FIN", "HOME LOAN"}},
	{"Credit card bill", []string{"CREDIT CARD", "CC PAYMENT", "CARD PAYMENT", "CRED"}},
	{"Insurance", []string{"LIC", "INSURANCE", "POLICY", "PREMIUM", "STAR HEALTH", "HDFC LIFE"}},
	{"Subscriptions", []string{"NETFLIX", "SPOTIFY", "HOTSTAR", "PRIME", "YOUTUBE", "APPLE", "JIOCINEMA", "SONYLIV", "ZEE5", "AUDIBLE", "GOOGLE ONE"}},
	{"Food & dining", []string{"SWIGGY", "ZOMATO", "DOMINOS", "MCDONALDS", "KFC", "RESTAURANT", "CAFE", "BAKERY", "BIRYANI", "SARAVANA BHAVAN", "ANNAPOORNA"}},
	{"Groceries", []string{"BIGBASKET", "BLINKIT", "ZEPTO", "DMART", "RELIANCE FRESH", "GROCERY", "SUPERMARKET", "MORE RETAIL", "NILGIRIS"}},
	{"Shopping", []string{"AMAZON", "FLIPKART", "MYNTRA", "AJIO", "MEESHO", "NYKAA", "CROMA", "DECATHLON"}},
	{"Travel & fuel", []string{"UBER", "OLA", "RAPIDO", "IRCTC", "REDBUS", "MAKEMYTRIP", "INDIGO", "FASTAG", "PETROL", "FUEL", "HPCL", "BPCL", "IOCL", "METRO"}},
	{"Bills & recharges", []string{"ELECTRICITY", "TNEB", "TANGEDCO", "BESCOM", "AIRTEL", "JIO", "BSNL", "BROADBAND", "FIBERNET", "INDANE", "GAS", "WATER", "DTH", "TATA PLAY", "RECHARGE"}},
	{"Rent", []string{"RENT"}},
	{"Health", []string{"PHARMACY", "APOLLO", "MEDPLUS", "HOSPITAL", "CLINIC", "1MG", "PHARMEASY", "DIAGNOSTICS"}},
	{"Education", []string{"SCHOOL", "COLLEGE", "TUITION", "UDEMY", "COURSERA"}},
	{"Cash", []string{"ATM", "CASH WDL", "CASH WITHDRAWAL"}},
}

const (
	BankCharges = "Bank charges"
	Other       = "Other"
	MoneyIn     = "Money in"
)

// Categories lists every category the rules can give, for the correction menu.
func Categories() []string {
	out := []string{}
	for _, r := range categoryRules {
		out = append(out, r.cat)
	}
	return append(out, "Transfers", MoneyIn, Other)
}

func words(s string) string {
	return " " + strings.Join(strings.FieldsFunc(strings.ToUpper(s), func(r rune) bool { return !(unicode.IsLetter(r) || unicode.IsDigit(r)) }), " ") + " "
}

// Categorise gives a category by rule. Rules the user taught (merchant ->
// category, from their corrections) win over the built-in keywords.
func Categorise(t Txn, learned map[string]string) (string, bool) {
	if c, ok := learned[t.Merchant]; ok && t.Merchant != "UNKNOWN" {
		return c, true
	}
	w := words(t.Description)
	for _, r := range categoryRules {
		for _, k := range r.words {
			if strings.Contains(w, " "+k+" ") {
				// income words only make sense for money in, charges only for money out
				if r.cat == "Income" && t.Amount < 0 || r.cat == BankCharges && t.Amount > 0 {
					continue
				}
				return r.cat, false
			}
		}
	}
	if t.Amount > 0 {
		return MoneyIn, false
	}
	return Other, false
}
