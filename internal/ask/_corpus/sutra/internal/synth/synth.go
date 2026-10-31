// Package synth generates labelled ERP vendor and CRM account data for
// entity-resolution tests and evals. The generator is deterministic for a
// seed and plants the cases that break naive matching:
//
//   - transliteration variants (Sri/Shree, Lakshmi/Laxmi, Thirumalai/Tirumalai)
//   - typos, legal-suffix and word-order differences
//   - one company with GST registrations in two states (same PAN, two GSTINs)
//   - different companies with the same trading name in different cities
//   - duplicate vendor rows inside the ERP itself
//   - missing GSTINs and differently formatted phone numbers in the CRM
package synth

import (
	"fmt"
	"math/rand/v2"
	"strings"
	"time"

	"github.com/Aravindh97/sutra/internal/normalize"
)

type Company struct {
	ID       string
	Name     string
	PAN      string
	GSTINs   []string // one per state registration
	Phone    string
	Domain   string
	City     string
	Category string
}

type Row struct {
	Fields  map[string]string
	TruthID string // company id
}

type Dataset struct {
	Companies []Company
	ERP       []Row // fields: vendor_id, vendor_name, gst_no, contact_phone, city, category
	CRM       []Row // fields: id, account_name, gstin, pan, phone, email, billing_city, updated_at
}

var (
	firsts = []string{"Sri", "Lakshmi", "Murugan", "Kaveri", "Annai", "Ganesh", "Velan", "Balaji", "Thirumalai",
		"Kongu", "Senthil", "Amman", "Saravana", "Vinayaka", "Meenakshi", "Bhavani", "Shanmuga", "Karpagam", "Kumaran", "Selvam"}
	cores = []string{"Textiles", "Knit Fabrics", "Dyeing Works", "Garments", "Yarn Traders", "Processors", "Exports",
		"Spinning Mills", "Logistics", "Packaging", "Trims", "Printers"}
	suffixes = []string{"Pvt Ltd", "Private Limited", "LLP", "& Co", ""}
	cities   = []string{"Tiruppur", "Coimbatore", "Erode", "Karur", "Salem", "Bengaluru", "Chennai"}
	variants = map[string][]string{"Sri": {"Shree", "Shri", "Sree"}, "Lakshmi": {"Laxmi", "Lakshmy"},
		"Thirumalai": {"Tirumalai"}, "Shanmuga": {"Shanmugha", "Sanmuga"}, "Meenakshi": {"Minakshi"},
		"Bhavani": {"Bavani"}, "Vinayaka": {"Vinayagar", "Vinayak"}, "Saravana": {"Sharavana"}}
	letters = "ABCDEFGHIJKLMNOPQRSTUVWXYZ"
)

func pan(r *rand.Rand, holder byte) string {
	b := make([]byte, 10)
	for i := 0; i < 5; i++ {
		b[i] = letters[r.IntN(26)]
	}
	b[3] = holder
	for i := 5; i < 9; i++ {
		b[i] = byte('0' + r.IntN(10))
	}
	b[9] = letters[r.IntN(26)]
	return string(b)
}

func gstin(state, pan string, entity int) string {
	s := fmt.Sprintf("%s%s%dZ", state, pan, entity)
	return s + string(normalize.GSTINChecksum(s))
}

func mobile(r *rand.Rand) string { return fmt.Sprintf("9%09d", r.IntN(1_000_000_000)) }

func formatPhone(r *rand.Rand, m string) string {
	switch r.IntN(4) {
	case 0:
		return "+91 " + m[:5] + " " + m[5:]
	case 1:
		return "0" + m
	case 2:
		return m[:3] + "-" + m[3:6] + "-" + m[6:]
	}
	return m
}

func typo(r *rand.Rand, s string) string {
	rs := []rune(s)
	if len(rs) < 6 {
		return s
	}
	i := 1 + r.IntN(len(rs)-2)
	switch r.IntN(3) {
	case 0: // swap
		rs[i], rs[i+1] = rs[i+1], rs[i]
	case 1: // drop
		rs = append(rs[:i], rs[i+1:]...)
	default: // double
		rs = append(rs[:i+1], rs[i:]...)
	}
	return string(rs)
}

func variant(r *rand.Rand, c Company) string {
	words := strings.Fields(c.Name)
	for i, w := range words {
		if v, ok := variants[w]; ok && r.Float64() < 0.5 {
			words[i] = v[r.IntN(len(v))]
		}
	}
	name := strings.Join(words, " ")
	// swap legal suffix
	for _, s := range suffixes {
		if s != "" && strings.HasSuffix(name, " "+s) {
			name = strings.TrimSuffix(name, " "+s)
			break
		}
	}
	if suf := suffixes[r.IntN(len(suffixes))]; suf != "" {
		name += " " + suf
	}
	if r.Float64() < 0.25 {
		name = typo(r, name)
	}
	if r.Float64() < 0.2 {
		name = strings.ToUpper(name)
	}
	return name
}

// Generate makes n companies and their ERP and CRM rows.
func Generate(seed uint64, n int) Dataset {
	r := rand.New(rand.NewPCG(seed, seed^0x9e3779b97f4a7c15))
	var d Dataset
	used := map[string]bool{}
	for i := 0; i < n; i++ {
		var name string
		for {
			name = firsts[r.IntN(len(firsts))] + " " + cores[r.IntN(len(cores))]
			// Plant same-name different-company pairs: allow a repeat ~8% of the time.
			if !used[name] || r.Float64() < 0.08 {
				break
			}
		}
		used[name] = true
		if s := suffixes[r.IntN(len(suffixes))]; s != "" {
			name += " " + s
		}
		holder := byte('C')
		if strings.HasSuffix(name, "& Co") || strings.HasSuffix(name, "LLP") {
			holder = 'F'
		}
		c := Company{ID: fmt.Sprintf("C%03d", i), Name: name, PAN: pan(r, holder), Phone: mobile(r),
			City: cities[r.IntN(len(cities))], Category: cores[r.IntN(len(cores))],
			Domain: strings.ToLower(strings.ReplaceAll(strings.Fields(name)[0]+strings.Fields(name)[1], " ", "")) + ".in"}
		c.GSTINs = []string{gstin("33", c.PAN, 1)}
		if r.Float64() < 0.12 { // also registered in Karnataka
			c.GSTINs = append(c.GSTINs, gstin("29", c.PAN, 1))
		}
		d.Companies = append(d.Companies, c)
	}
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	vid, aid := 1000, 5000
	for _, c := range d.Companies {
		if r.Float64() < 0.85 {
			copies := 1
			if r.Float64() < 0.1 {
				copies = 2 // duplicate vendor master entry
			}
			for k := 0; k < copies; k++ {
				g := c.GSTINs[r.IntN(len(c.GSTINs))]
				name := c.Name
				if k > 0 {
					name = variant(r, c)
				}
				if r.Float64() < 0.05 {
					g = g[:14] + "0" // keying error: checksum fails
				}
				d.ERP = append(d.ERP, Row{TruthID: c.ID, Fields: map[string]string{
					"vendor_id": fmt.Sprint(vid), "vendor_name": name, "gst_no": g,
					"contact_phone": formatPhone(r, c.Phone), "city": c.City, "category": c.Category}})
				vid++
			}
		}
		if r.Float64() < 0.75 {
			f := map[string]string{"id": fmt.Sprintf("ACC-%d", aid), "account_name": variant(r, c),
				"phone": formatPhone(r, c.Phone), "billing_city": c.City,
				"updated_at": base.Add(time.Duration(r.IntN(150*24)) * time.Hour).Format(time.RFC3339)}
			aid++
			if r.Float64() < 0.6 {
				f["gstin"] = c.GSTINs[len(c.GSTINs)-1]
			}
			if r.Float64() < 0.3 {
				f["pan"] = c.PAN
			}
			if r.Float64() < 0.5 {
				f["email"] = "accounts@" + c.Domain
			} else if r.Float64() < 0.5 {
				f["email"] = strings.ToLower(strings.Fields(c.Name)[0]) + "@gmail.com"
			}
			if r.Float64() < 0.15 {
				delete(f, "phone")
			}
			d.CRM = append(d.CRM, Row{TruthID: c.ID, Fields: f})
		}
	}
	return d
}
