// Package money holds amounts as whole paise (int64), never floats, so sums
// are exact. ₹1 = 100 paise.
package money

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// Parse reads a rupee amount as people and banks write it: "1,234.5",
// "₹ 1,23,456.78", "Rs. 50", "-200.00". At most two decimal places.
func Parse(s string) (int64, error) {
	s = strings.TrimSpace(s)
	neg := false
	for range 2 { // the sign may come before or after the currency: -₹5, ₹-5
		if strings.HasPrefix(s, "-") {
			neg, s = true, strings.TrimSpace(s[1:])
		} else if strings.HasPrefix(s, "(") && strings.HasSuffix(s, ")") { // accounting style
			neg, s = true, strings.TrimSpace(s[1:len(s)-1])
		}
		for _, p := range []string{"₹", "Rs.", "Rs", "INR"} {
			s = strings.TrimSpace(strings.TrimPrefix(s, p))
		}
	}
	s = strings.ReplaceAll(s, ",", "")
	if s == "" {
		return 0, errors.New("empty amount")
	}
	whole, frac, _ := strings.Cut(s, ".")
	if len(frac) > 2 {
		return 0, fmt.Errorf("%q: more than two decimal places", s)
	}
	frac += strings.Repeat("0", 2-len(frac))
	if whole == "" {
		whole = "0"
	}
	w, err1 := strconv.ParseUint(whole, 10, 40) // up to about ₹1 lakh crore
	f, err2 := strconv.ParseUint(frac, 10, 8)
	if err1 != nil || err2 != nil {
		return 0, fmt.Errorf("%q is not an amount", s)
	}
	p := int64(w)*100 + int64(f)
	if neg {
		p = -p
	}
	return p, nil
}

// Format writes paise as rupees with Indian digit grouping: ₹1,23,456.78.
func Format(p int64) string {
	sign := ""
	if p < 0 {
		sign, p = "-", -p
	}
	r := strconv.FormatInt(p/100, 10)
	if len(r) > 3 {
		head, tail := r[:len(r)-3], r[len(r)-3:]
		var groups []string
		for len(head) > 2 {
			groups = append([]string{head[len(head)-2:]}, groups...)
			head = head[:len(head)-2]
		}
		if head != "" {
			groups = append([]string{head}, groups...)
		}
		r = strings.Join(groups, ",") + "," + tail
	}
	return fmt.Sprintf("%s₹%s.%02d", sign, r, p%100)
}

// Split divides total into n shares that add up to exactly total. The
// leftover paise go one each to the first shares, so no paisa is lost or
// invented.
func Split(total int64, n int) []int64 {
	if n <= 0 {
		return nil
	}
	out := make([]int64, n)
	base, rem := total/int64(n), total%int64(n)
	for i := range out {
		out[i] = base
		if int64(i) < rem {
			out[i]++
		}
	}
	return out
}
