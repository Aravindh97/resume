// Package normalize cleans the identifiers and names used for entity
// resolution of Indian businesses: GSTIN (with checksum), PAN, phone numbers,
// email domains and company names, including a phonetic key that folds common
// transliteration variants (Shree/Sri, Lakshmi/Laxmi, Thirumalai/Tirumalai).
package normalize

import (
	"regexp"
	"sort"
	"strings"
	"unicode"
)

const gstAlphabet = "0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZ"

var (
	gstinRe = regexp.MustCompile(`^[0-9]{2}[A-Z]{5}[0-9]{4}[A-Z][1-9A-Z]Z[0-9A-Z]$`)
	panRe   = regexp.MustCompile(`^[A-Z]{5}[0-9]{4}[A-Z]$`)
)

// GSTINChecksum computes the 15th character of a GSTIN from the first 14.
func GSTINChecksum(first14 string) byte {
	sum := 0
	for i := 0; i < 14; i++ {
		v := strings.IndexByte(gstAlphabet, first14[i])
		f := 1
		if i%2 == 1 {
			f = 2
		}
		p := v * f
		sum += p/36 + p%36
	}
	return gstAlphabet[(36-sum%36)%36]
}

// GSTIN returns the canonical GSTIN and whether it is well-formed with a
// valid checksum. Invalid values are still returned (cleaned) so they can be
// shown, but must not be used as a matching key.
func GSTIN(s string) (string, bool) {
	g := strings.ToUpper(strings.Map(func(r rune) rune {
		if unicode.IsSpace(r) || r == '-' {
			return -1
		}
		return r
	}, s))
	if !gstinRe.MatchString(g) {
		return g, false
	}
	return g, GSTINChecksum(g[:14]) == g[14]
}

// PANFromGSTIN extracts the PAN embedded in characters 3-12 of a GSTIN. One
// legal entity has one PAN but a separate GSTIN in every state it is
// registered in, so PAN is the stronger company identifier.
func PANFromGSTIN(g string) string {
	if len(g) != 15 {
		return ""
	}
	return g[2:12]
}

func PAN(s string) (string, bool) {
	p := strings.ToUpper(strings.TrimSpace(s))
	return p, panRe.MatchString(p)
}

// Phone returns +91XXXXXXXXXX for Indian mobile numbers and the bare digits
// (with STD code) for anything else; "" if there are too few digits.
func Phone(s string) string {
	var d strings.Builder
	for _, r := range s {
		if r >= '0' && r <= '9' {
			d.WriteRune(r)
		}
	}
	n := d.String()
	n = strings.TrimLeft(n, "0")
	if len(n) == 12 && strings.HasPrefix(n, "91") {
		n = n[2:]
	}
	if len(n) == 10 && n[0] >= '6' && n[0] <= '9' {
		return "+91" + n
	}
	if len(n) < 8 {
		return ""
	}
	return "0" + n
}

var freeMail = map[string]bool{"gmail.com": true, "yahoo.com": true, "yahoo.co.in": true, "hotmail.com": true,
	"outlook.com": true, "rediffmail.com": true, "icloud.com": true, "proton.me": true}

// EmailDomain returns the domain of a company email address, or "" for
// free-mail providers, which say nothing about which company someone is.
func EmailDomain(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	at := strings.LastIndexByte(s, '@')
	if at < 0 || at == len(s)-1 {
		return ""
	}
	d := s[at+1:]
	if freeMail[d] {
		return ""
	}
	return d
}

var legal = map[string]bool{"pvt": true, "private": true, "ltd": true, "limited": true, "llp": true, "inc": true,
	"co": true, "company": true, "corp": true, "corporation": true, "ms": true, "the": true, "and": true, "india": true}

// NameTokens lowercases, drops punctuation and legal-form words.
func NameTokens(s string) []string {
	s = strings.ToLower(strings.ReplaceAll(s, "&", " and "))
	s = strings.ReplaceAll(s, "m/s", " ")
	f := strings.FieldsFunc(s, func(r rune) bool { return !unicode.IsLetter(r) && !unicode.IsDigit(r) })
	out := f[:0]
	for _, t := range f {
		if !legal[t] && !looksLegal(t) {
			out = append(out, t)
		}
	}
	return out
}

var legalLong = []string{"private", "limited", "corporation", "company", "pvt"}

// looksLegal catches misspelt or split legal suffixes from manual data entry
// ("Privat eLimited", "Limitted", "PvtL td") and stray 1-2 letter fragments.
func looksLegal(t string) bool {
	if len([]rune(t)) <= 2 {
		return true
	}
	for _, l := range legalLong {
		if len(t) >= 4 && JaroWinkler(t, l) >= 0.88 {
			return true
		}
	}
	return false
}

var fold = strings.NewReplacer(
	"shree", "sri", "shri", "sri", "sree", "sri",
	"ksh", "x", "aa", "a", "ee", "i", "ii", "i", "oo", "u", "uu", "u",
	"th", "t", "dh", "d", "bh", "b", "ph", "f", "kh", "k", "gh", "g", "sh", "s", "ch", "c",
	"w", "v", "z", "j", "q", "k", "ck", "k",
)

// Phonetic folds one token so common romanisations of the same Indian word
// compare equal: "Thirumalai"/"Tirumalai" -> "tirumalai", "Lakshmi"/"Laxmi"
// -> "laxmi", "Shree"/"Sri" -> "sri".
func Phonetic(tok string) string {
	t := fold.Replace(tok)
	var b strings.Builder
	var prev rune
	for _, r := range t {
		if r != prev {
			b.WriteRune(r)
		}
		prev = r
	}
	t = b.String()
	if len(t) > 4 && strings.HasSuffix(t, "a") {
		t = t[:len(t)-1]
	}
	return t
}

// Key returns the folded, sorted token string used for name comparison.
func Key(name string) string {
	toks := NameTokens(name)
	for i, t := range toks {
		toks[i] = Phonetic(t)
	}
	sort.Strings(toks)
	return strings.Join(toks, " ")
}

// generic words carry little identity in a company name: "Lakshmi Knit
// Fabrics" and "Selvam Knit Fabrics" share two of three tokens but are
// different businesses. They count for half of a distinctive token.
var generic = map[string]bool{}

const GenericWeight = 0.5

func init() {
	for _, w := range strings.Fields(`textile textiles fabric fabrics knit knits knitting garment garments export exports
	exporter exporters yarn yarns trader traders trading mill mills spinning dyeing dyers works processor processors
	logistic logistics packaging pack trims printer printers print prints industry industries enterprise enterprises
	group sons brothers international global apparel apparels clothing`) {
		generic[Phonetic(w)] = true
	}
}

// NameSimilarity is a weighted Monge-Elkan score: every token is matched to
// its most similar token on the other side (Jaro-Winkler on phonetic keys),
// the matches are averaged with generic words down-weighted, and the score is
// the minimum of the two directions so extra distinctive words are penalised.
func NameSimilarity(a, b string) float64 {
	ta, tb := tokens(a), tokens(b)
	if len(ta) == 0 || len(tb) == 0 {
		return 0
	}
	return min(monge(ta, tb), monge(tb, ta))
}

func tokens(name string) []string {
	t := NameTokens(name)
	for i := range t {
		t[i] = Phonetic(t[i])
	}
	return t
}

func monge(a, b []string) float64 {
	var sum, wsum float64
	for _, x := range a {
		best := 0.0
		for _, y := range b {
			best = max(best, JaroWinkler(x, y))
		}
		w := 1.0
		if generic[x] {
			w = GenericWeight
		}
		sum += w * best
		wsum += w
	}
	return sum / wsum
}

// IsGeneric reports whether a phonetic token is a generic industry word.
func IsGeneric(tok string) bool { return generic[tok] }

// JaroWinkler similarity in [0,1] with the standard prefix scale 0.1.
func JaroWinkler(a, b string) float64 {
	ra, rb := []rune(a), []rune(b)
	if len(ra) == 0 && len(rb) == 0 {
		return 1
	}
	if len(ra) == 0 || len(rb) == 0 {
		return 0
	}
	win := max(len(ra), len(rb))/2 - 1
	if win < 0 {
		win = 0
	}
	ma, mb := make([]bool, len(ra)), make([]bool, len(rb))
	m := 0
	for i := range ra {
		lo, hi := max(0, i-win), min(len(rb)-1, i+win)
		for j := lo; j <= hi; j++ {
			if !mb[j] && ra[i] == rb[j] {
				ma[i], mb[j] = true, true
				m++
				break
			}
		}
	}
	if m == 0 {
		return 0
	}
	t, k := 0, 0
	for i := range ra {
		if !ma[i] {
			continue
		}
		for !mb[k] {
			k++
		}
		if ra[i] != rb[k] {
			t++
		}
		k++
	}
	fm := float64(m)
	jaro := (fm/float64(len(ra)) + fm/float64(len(rb)) + (fm-float64(t)/2)/fm) / 3
	p := 0
	for p < min(4, len(ra), len(rb)) && ra[p] == rb[p] {
		p++
	}
	return jaro + float64(p)*0.1*(1-jaro)
}
