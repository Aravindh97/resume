package money

import (
	"math/rand/v2"
	"testing"
)

func TestParse(t *testing.T) {
	ok := map[string]int64{
		"1,234.5": 123450, "₹ 1,23,456.78": 12345678, "Rs. 50": 5000, "-200.00": -20000, "(75.25)": -7525,
		"0.07": 7, ".5": 50, "INR 10": 1000, "  99 ": 9900,
	}
	for in, want := range ok {
		if got, err := Parse(in); err != nil || got != want {
			t.Errorf("Parse(%q) = %d, %v; want %d", in, got, err, want)
		}
	}
	for _, bad := range []string{"", "1.234", "abc", "1.2.3", "₹", "12a"} {
		if _, err := Parse(bad); err == nil {
			t.Errorf("Parse(%q) should fail", bad)
		}
	}
}

func TestFormat(t *testing.T) {
	cases := map[int64]string{0: "₹0.00", 5: "₹0.05", 99900: "₹999.00", 100000: "₹1,000.00", 12345678: "₹1,23,456.78", 1000000000: "₹1,00,00,000.00", -20050: "-₹200.50"}
	for in, want := range cases {
		if got := Format(in); got != want {
			t.Errorf("Format(%d) = %q, want %q", in, got, want)
		}
	}
}

func TestFormatParseRoundTrip(t *testing.T) {
	r := rand.New(rand.NewPCG(1, 2))
	for range 5000 {
		p := r.Int64N(1e12) - 5e11
		if got, err := Parse(Format(p)); err != nil || got != p {
			t.Fatalf("round trip %d: %d, %v", p, got, err)
		}
	}
}

func TestSplitAddsUpExactly(t *testing.T) {
	r := rand.New(rand.NewPCG(3, 4))
	for range 5000 {
		total, n := r.Int64N(1e8), 1+r.IntN(12)
		s := Split(total, n)
		var sum, lo, hi int64 = 0, s[0], s[0]
		for _, x := range s {
			sum += x
			lo, hi = min(lo, x), max(hi, x)
		}
		if sum != total || hi-lo > 1 {
			t.Fatalf("Split(%d, %d) = %v", total, n, s)
		}
	}
	if got := Split(1000, 3); got[0] != 334 || got[1] != 333 || got[2] != 333 {
		t.Fatalf("₹10 three ways: %v", got)
	}
}
