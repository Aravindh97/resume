package memory

import (
	"math"
	"strings"
	"time"
	"unicode"
)

type Action string

const (
	Add       Action = "add"
	Confirm   Action = "confirm"   // same fact seen again
	Supersede Action = "supersede" // same subject, new value: close old, insert new
	Ignore    Action = "ignore"
)

// Existing is a current memory in the same scope and owner as a candidate,
// with its similarity to the candidate.
type Existing struct {
	Memory     Memory
	Similarity float64
}

type Decision struct {
	Action Action
	Target *Memory // memory confirmed or superseded
	Reason string
}

type Policy struct {
	MinConfidence float64 // candidates below this are ignored
	Duplicate     float64 // similarity above which a different-subject memory is a duplicate
}

func DefaultPolicy() Policy { return Policy{MinConfidence: 0.5, Duplicate: 0.9} }

// Decide reconciles one candidate against existing current memories.
//
// The order matters: an exact subject match wins over a similar-content match,
// so "my manager is Priya" followed by "my manager is Arjun" supersedes the
// old manager instead of being treated as a near-duplicate and confirmed.
func (p Policy) Decide(c Candidate, existing []Existing) Decision {
	if c.Confidence < p.MinConfidence {
		return Decision{Action: Ignore, Reason: "low confidence"}
	}
	subj := NormSubject(c.Subject)
	for i := range existing {
		e := &existing[i]
		if e.Memory.Subject != subj {
			continue
		}
		// Embedding similarity is not used here on purpose: "Prefers Tamil for
		// voice calls" and "Prefers English for voice calls" embed almost
		// identically but are different values. Token containment treats
		// "manager is Arjun Mehta now" as the same value as "manager is Arjun
		// Mehta", and Tamil vs English as a change.
		if sameValue(e.Memory.Content, c.Content) {
			return Decision{Action: Confirm, Target: &e.Memory, Reason: "same subject, same content"}
		}
		// A late-arriving older statement (replayed or out-of-order history)
		// must not overwrite a newer value.
		if !c.ObservedAt.IsZero() && c.ObservedAt.Before(e.Memory.ValidFrom) {
			return Decision{Action: Ignore, Target: &e.Memory, Reason: "older than the current value"}
		}
		return Decision{Action: Supersede, Target: &e.Memory, Reason: "same subject, new content"}
	}
	var best *Existing
	for i := range existing {
		if best == nil || existing[i].Similarity > best.Similarity {
			best = &existing[i]
		}
	}
	if best != nil && best.Similarity >= p.Duplicate {
		return Decision{Action: Confirm, Target: &best.Memory, Reason: "near-duplicate of another subject"}
	}
	return Decision{Action: Add, Reason: "new fact"}
}

var filler = map[string]bool{"now": true, "currently": true, "still": true, "again": true, "these": true, "days": true,
	"from": true, "on": true, "the": true, "a": true, "an": true, "is": true, "are": true}

func valueTokens(s string) map[string]bool {
	out := map[string]bool{}
	for _, w := range strings.FieldsFunc(strings.ToLower(s), func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r) && !unicode.IsMark(r)
	}) {
		if !filler[w] {
			out[w] = true
		}
	}
	return out
}

// sameValue reports whether one content's tokens are a subset of the other's.
func sameValue(a, b string) bool {
	ta, tb := valueTokens(a), valueTokens(b)
	if len(ta) > len(tb) {
		ta, tb = tb, ta
	}
	if len(ta) == 0 {
		return false
	}
	for w := range ta {
		if !tb[w] {
			return false
		}
	}
	return true
}

// Weights for ranking recalled memories.
type Weights struct {
	Similarity, Recency, Confirmations float64
	HalfLife                           time.Duration
}

func DefaultWeights() Weights {
	return Weights{Similarity: 0.75, Recency: 0.15, Confirmations: 0.10, HalfLife: 30 * 24 * time.Hour}
}

// Score combines relevance with recency (exponential decay since the memory
// was last confirmed) and how many times it was confirmed (saturating at 10).
func (w Weights) Score(sim float64, m Memory, now time.Time) float64 {
	age := now.Sub(m.LastConfirmed)
	if age < 0 {
		age = 0
	}
	rec := math.Exp(-math.Ln2 * float64(age) / float64(w.HalfLife))
	conf := math.Min(1, math.Log1p(float64(m.Confirmations))/math.Log1p(10))
	return w.Similarity*sim + w.Recency*rec + w.Confirmations*conf
}
