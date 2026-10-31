// Package resolve groups source records that describe the same real-world
// entity and builds a golden record whose every value is traced to the
// source record it came from.
//
// Steps:
//  1. Normalise identifiers (GSTIN with checksum, PAN, phone, email domain)
//     and names (legal suffixes dropped, transliteration folded).
//  2. Block: only compare records sharing a blocking key, so the work is
//     near-linear instead of quadratic.
//  3. Score each candidate pair with explainable rules. A PAN conflict is
//     negative evidence: the two records can never end up together.
//  4. Cluster by merging the highest-scoring pairs first, refusing any merge
//     that would put two different PANs in one cluster.
//  5. Pairs in the review band go to a human review queue instead of being
//     guessed.
package resolve

import (
	"crypto/sha256"
	"encoding/hex"
	"sort"
	"strings"
	"time"

	"github.com/Aravindh97/sutra/internal/normalize"
	"github.com/Aravindh97/sutra/internal/ontology"
)

type Config struct {
	Match    float64 // pairs at or above this score are merged
	Review   float64 // pairs in [Review, Match) go to the review queue
	MaxBlock int     // blocks larger than this are skipped (too generic a key)
	// Previous maps record key -> entity id from the last run. Clusters reuse
	// the id most of their members already had, so ids agents have stored
	// stay valid when new records arrive or clusters grow.
	Previous map[string]string
}

func DefaultConfig() Config { return Config{Match: 0.85, Review: 0.65, MaxBlock: 200} }

// norm is the matching view of a record.
type norm struct {
	r        ontology.Record
	gstin    string // only if checksum-valid
	pan      string // explicit or derived from a valid GSTIN
	phone    string
	domain   string
	name     string
	nameKey  string
	city     string
	firstTok string
}

func prep(r ontology.Record) norm {
	n := norm{r: r, name: r.Attrs["name"], city: strings.ToLower(strings.TrimSpace(r.Attrs["city"]))}
	if g, ok := normalize.GSTIN(r.Attrs["gstin"]); ok {
		n.gstin = g
		n.pan = normalize.PANFromGSTIN(g)
	}
	if p, ok := normalize.PAN(r.Attrs["pan"]); ok {
		n.pan = p
	}
	n.phone = normalize.Phone(r.Attrs["phone"])
	n.domain = normalize.EmailDomain(r.Attrs["email"])
	n.nameKey = normalize.Key(n.name)
	if f := strings.Fields(n.nameKey); len(f) > 0 {
		n.firstTok = f[0]
	}
	return n
}

type Pair struct {
	A      string  `json:"a"` // record keys
	B      string  `json:"b"`
	Score  float64 `json:"score"`
	Reason string  `json:"reason"`
}

// Score compares two records. Exported for tests and the review UI.
func Score(a, b ontology.Record) Pair { return score(prep(a), prep(b)) }

// ambiguous holds name|city keys seen with more than one PAN: two different
// legal entities trade under that name in that city, so a name-only match
// cannot be trusted and goes to review.
type ambiguous map[string]bool

func ambiguity(ns []norm) ambiguous {
	pans := map[string]map[string]bool{}
	for _, n := range ns {
		if n.pan == "" {
			continue
		}
		k := n.nameKey + "|" + n.city
		if pans[k] == nil {
			pans[k] = map[string]bool{}
		}
		pans[k][n.pan] = true
	}
	out := ambiguous{}
	for k, v := range pans {
		if len(v) > 1 {
			out[k] = true
		}
	}
	return out
}

func score(a, b norm) Pair { return scoreWith(a, b, nil) }

func scoreWith(a, b norm, amb ambiguous) Pair {
	p := Pair{A: a.r.Key(), B: b.r.Key()}
	if a.pan != "" && b.pan != "" && a.pan != b.pan {
		p.Reason = "pan conflict"
		return p
	}
	sim := normalize.NameSimilarity(a.name, b.name)
	sameCity := a.city != "" && a.city == b.city
	switch {
	case a.gstin != "" && a.gstin == b.gstin:
		p.Score, p.Reason = 1, "same GSTIN"
	case a.pan != "" && a.pan == b.pan:
		p.Score, p.Reason = 0.97, "same PAN"
	case a.phone != "" && a.phone == b.phone && sim >= 0.75:
		p.Score, p.Reason = 0.92, "same phone, similar name"
	case a.domain != "" && a.domain == b.domain && sim >= 0.8:
		p.Score, p.Reason = 0.9, "same email domain, similar name"
	case sim >= 0.94 && sameCity:
		p.Score, p.Reason = 0.87, "near-identical name, same city"
	case sim >= 0.94:
		p.Score, p.Reason = 0.7, "near-identical name, city differs or missing"
	case sim >= 0.88 && sameCity:
		p.Score, p.Reason = 0.72, "similar name, same city"
	default:
		p.Score, p.Reason = sim*0.6, "weak name similarity"
	}
	nameOnly := p.Score < 0.9
	if nameOnly && (amb[a.nameKey+"|"+a.city] || amb[b.nameKey+"|"+b.city]) && p.Score >= 0.65 {
		p.Score, p.Reason = min(p.Score, 0.7), p.Reason+"; name shared by several companies here"
	}
	// A strong ID present on both sides that disagrees lowers confidence in a
	// name-only match (two branches with different GSTINs are still caught
	// by PAN above).
	if p.Score < 0.97 && a.gstin != "" && b.gstin != "" && a.gstin != b.gstin {
		p.Score *= 0.8
		p.Reason += "; GSTINs differ"
	}
	return p
}

func blockKeys(n norm) []string {
	var k []string
	add := func(prefix, v string) {
		if v != "" {
			k = append(k, prefix+":"+v)
		}
	}
	add("gstin", n.gstin)
	add("pan", n.pan)
	add("phone", n.phone)
	add("domain", n.domain)
	if n.firstTok != "" {
		add("tokcity", n.firstTok+"|"+n.city)
	}
	if len(n.nameKey) >= 4 {
		add("prefix", n.nameKey[:4])
	}
	return k
}

type Entity struct {
	ID      string            `json:"id"`
	Type    string            `json:"type"`
	Members []string          `json:"members"`
	Golden  map[string]string `json:"golden"`
	Lineage []Lineage         `json:"lineage"`
	Reasons []Pair            `json:"reasons"` // pairs that linked the members
}

type Lineage struct {
	Attribute string    `json:"attribute"`
	Value     string    `json:"value"`
	Source    string    `json:"source"`
	RecordID  string    `json:"record_id"`
	UpdatedAt time.Time `json:"updated_at"`
}

type Result struct {
	Entities []Entity `json:"entities"`
	Review   []Pair   `json:"review"`
	Compared int      `json:"compared"` // pairs scored
	Skipped  int      `json:"skipped_blocks"`
}

// Resolve clusters records of one entity type. priority maps source name to
// survivorship priority (lower wins).
func Resolve(recs []ontology.Record, priority map[string]int, cfg Config) Result {
	ns := make([]norm, 0, len(recs))
	for _, r := range recs {
		if !r.Deleted {
			ns = append(ns, prep(r))
		}
	}
	sort.Slice(ns, func(i, j int) bool { return ns[i].r.Key() < ns[j].r.Key() })
	idx := map[string]int{}
	for i, n := range ns {
		idx[n.r.Key()] = i
	}

	blocks := map[string][]int{}
	for i, n := range ns {
		for _, k := range blockKeys(n) {
			blocks[k] = append(blocks[k], i)
		}
	}
	amb := ambiguity(ns)
	var res Result
	seen := map[[2]int]bool{}
	var pairs []Pair
	for _, members := range blocks {
		if len(members) > cfg.MaxBlock {
			res.Skipped++
			continue
		}
		for x := 0; x < len(members); x++ {
			for y := x + 1; y < len(members); y++ {
				i, j := members[x], members[y]
				if i > j {
					i, j = j, i
				}
				if seen[[2]int{i, j}] {
					continue
				}
				seen[[2]int{i, j}] = true
				res.Compared++
				if p := scoreWith(ns[i], ns[j], amb); p.Score >= cfg.Review {
					pairs = append(pairs, p)
				}
			}
		}
	}
	sort.Slice(pairs, func(i, j int) bool {
		if pairs[i].Score != pairs[j].Score {
			return pairs[i].Score > pairs[j].Score
		}
		return pairs[i].A+pairs[i].B < pairs[j].A+pairs[j].B
	})

	// Union-find with a cannot-link constraint on PAN.
	parent := make([]int, len(ns))
	pans := make([]map[string]bool, len(ns))
	for i := range parent {
		parent[i] = i
		pans[i] = map[string]bool{}
		if ns[i].pan != "" {
			pans[i][ns[i].pan] = true
		}
	}
	var find func(int) int
	find = func(i int) int {
		for parent[i] != i {
			parent[i] = parent[parent[i]]
			i = parent[i]
		}
		return i
	}
	linkReasons := map[int][]Pair{}
	for _, p := range pairs {
		ra, rb := find(idx[p.A]), find(idx[p.B])
		if ra == rb {
			continue
		}
		if p.Score < cfg.Match {
			res.Review = append(res.Review, p)
			continue
		}
		if conflict(pans[ra], pans[rb]) {
			res.Review = append(res.Review, Pair{A: p.A, B: p.B, Score: p.Score, Reason: p.Reason + "; blocked: clusters have different PANs"})
			continue
		}
		parent[rb] = ra
		for k := range pans[rb] {
			pans[ra][k] = true
		}
		linkReasons[ra] = append(append(linkReasons[ra], linkReasons[rb]...), p)
		delete(linkReasons, rb)
	}

	groups := map[int][]int{}
	for i := range ns {
		r := find(i)
		groups[r] = append(groups[r], i)
	}
	roots := make([]int, 0, len(groups))
	for r := range groups {
		roots = append(roots, r)
	}
	// Larger clusters claim previous ids first, so a split keeps the id on
	// the bigger half.
	sort.Slice(roots, func(i, j int) bool {
		if len(groups[roots[i]]) != len(groups[roots[j]]) {
			return len(groups[roots[i]]) > len(groups[roots[j]])
		}
		return ns[groups[roots[i]][0]].r.Key() < ns[groups[roots[j]][0]].r.Key()
	})
	taken := map[string]bool{}
	for _, root := range roots {
		g := groups[root]
		e := Entity{Type: ns[g[0]].r.Entity, Reasons: linkReasons[root]}
		var rs []ontology.Record
		votes := map[string]int{}
		for _, i := range g {
			k := ns[i].r.Key()
			e.Members = append(e.Members, k)
			rs = append(rs, ns[i].r)
			if id := cfg.Previous[k]; id != "" {
				votes[id]++
			}
		}
		sort.Strings(e.Members)
		best := ""
		for id, v := range votes {
			if !taken[id] && (best == "" || v > votes[best] || v == votes[best] && id < best) {
				best = id
			}
		}
		if best == "" {
			best = EntityID(e.Type, e.Members)
			for taken[best] {
				best = EntityID(e.Type, append([]string{best}, e.Members...))
			}
		}
		taken[best] = true
		e.ID = best
		e.Golden, e.Lineage = golden(rs, priority)
		res.Entities = append(res.Entities, e)
	}
	sort.Slice(res.Entities, func(i, j int) bool { return res.Entities[i].ID < res.Entities[j].ID })
	return res
}

func conflict(a, b map[string]bool) bool {
	if len(a) == 0 || len(b) == 0 {
		return false
	}
	for k := range a {
		if b[k] {
			return false
		}
	}
	return true
}

// EntityID derives a new id for a cluster that has no previous id.
func EntityID(typ string, sortedMembers []string) string {
	h := sha256.Sum256([]byte(typ + "|" + sortedMembers[0]))
	return strings.ToLower(typ[:3]) + "_" + hex.EncodeToString(h[:8])
}

// golden picks one value per attribute: from the highest-priority source,
// then the most recently updated record. Identifier values that fail
// validation lose to valid ones.
func golden(rs []ontology.Record, priority map[string]int) (map[string]string, []Lineage) {
	type cand struct {
		r     ontology.Record
		v     string
		valid bool
	}
	by := map[string][]cand{}
	for _, r := range rs {
		for a, v := range r.Attrs {
			valid := true
			switch a {
			case "gstin":
				v, valid = normalize.GSTIN(v)
			case "pan":
				v, valid = normalize.PAN(v)
			case "phone":
				if p := normalize.Phone(v); p != "" {
					v = p
				}
			}
			by[a] = append(by[a], cand{r, v, valid})
		}
	}
	g := map[string]string{}
	var lin []Lineage
	attrs := make([]string, 0, len(by))
	for a := range by {
		attrs = append(attrs, a)
	}
	sort.Strings(attrs)
	for _, a := range attrs {
		cs := by[a]
		sort.SliceStable(cs, func(i, j int) bool {
			if cs[i].valid != cs[j].valid {
				return cs[i].valid
			}
			pi, pj := priority[cs[i].r.Source], priority[cs[j].r.Source]
			if pi != pj {
				return pi < pj
			}
			if !cs[i].r.UpdatedAt.Equal(cs[j].r.UpdatedAt) {
				return cs[i].r.UpdatedAt.After(cs[j].r.UpdatedAt)
			}
			return cs[i].r.Key() < cs[j].r.Key()
		})
		w := cs[0]
		g[a] = w.v
		lin = append(lin, Lineage{Attribute: a, Value: w.v, Source: w.r.Source, RecordID: w.r.ID, UpdatedAt: w.r.UpdatedAt})
	}
	if pan := g["pan"]; pan == "" {
		if gst, ok := normalize.GSTIN(g["gstin"]); ok {
			// Derived value: lineage points at the record that supplied the GSTIN.
			for _, l := range lin {
				if l.Attribute == "gstin" {
					g["pan"] = normalize.PANFromGSTIN(gst)
					lin = append(lin, Lineage{Attribute: "pan", Value: g["pan"], Source: l.Source, RecordID: l.RecordID, UpdatedAt: l.UpdatedAt})
				}
			}
		}
	}
	return g, lin
}
