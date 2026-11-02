package store

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"sort"
	"sync"
	"time"

	"github.com/Aravindh97/smriti/internal/embed"
	"github.com/Aravindh97/smriti/internal/memory"
)

// Mem is an in-memory store with the Postgres store's rules: writes for one
// (org, scope, owner) are serialised (the advisory lock), each (source,
// subject) is processed once (memory_writes), at most one current value per
// subject, rows are closed and never edited, and every read applies the
// same visibility rule.
type Mem struct {
	Embedder embed.Embedder
	Policy   memory.Policy
	Weights  memory.Weights
	Now      func() time.Time

	mu     sync.Mutex // guards rows and writes
	rows   []*memory.Memory
	writes map[string]memory.Action // org|source|subject
	owners sync.Map                 // org|scope|owner -> *sync.Mutex
}

func NewMem(e embed.Embedder) *Mem {
	return &Mem{Embedder: e, Policy: memory.DefaultPolicy(), Weights: memory.DefaultWeights(), Now: time.Now, writes: map[string]memory.Action{}}
}

func (m *Mem) SetClock(now func() time.Time) { m.Now = now }
func (m *Mem) Embeds() embed.Embedder        { return m.Embedder }

func newID() string {
	b := make([]byte, 16)
	rand.Read(b)
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	h := hex.EncodeToString(b)
	return h[:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:]
}

func sim(a, b []float32) float64 { return float64(embed.Cosine(a, b)) }

func (m *Mem) Write(ctx context.Context, p memory.Principal, cands []memory.Candidate) ([]WriteResult, error) {
	texts := make([]string, len(cands))
	for i, c := range cands {
		texts[i] = c.Subject + ": " + c.Content
	}
	vecs, err := m.Embedder.Embed(ctx, texts)
	if err != nil {
		return nil, err
	}
	out := make([]WriteResult, 0, len(cands))
	for i, c := range cands {
		out = append(out, m.writeOne(p, c, vecs[i]))
	}
	return out, nil
}

func (m *Mem) writeOne(p memory.Principal, c memory.Candidate, v []float32) WriteResult {
	res := WriteResult{Candidate: c}
	owner, err := p.Owner(c.Scope, c.Team)
	if err != nil {
		res.Action, res.Reason = Denied, err.Error()
		return res
	}
	subj := memory.NormSubject(c.Subject)
	now := m.Now()
	if c.ObservedAt.IsZero() || c.ObservedAt.After(now) {
		c.ObservedAt = now
	}
	at := c.ObservedAt

	l, _ := m.owners.LoadOrStore(p.OrgID+"|"+string(c.Scope)+"|"+owner, &sync.Mutex{})
	l.(*sync.Mutex).Lock()
	defer l.(*sync.Mutex).Unlock()
	m.mu.Lock()
	defer m.mu.Unlock()

	wk := p.OrgID + "|" + c.SourceRef + "|" + subj
	if _, done := m.writes[wk]; done {
		res.Action, res.Reason = Duplicate, "source already processed"
		return res
	}

	type cand struct {
		row *memory.Memory
		sim float64
	}
	var near []cand
	for _, r := range m.rows {
		if r.OrgID != p.OrgID || r.Scope != c.Scope || r.OwnerID != owner || r.ValidTo != nil {
			continue
		}
		s := sim(r.Embedding, v)
		if r.Subject == subj || s >= m.Policy.Duplicate {
			near = append(near, cand{r, s})
		}
	}
	sort.SliceStable(near, func(i, j int) bool {
		if (near[i].row.Subject == subj) != (near[j].row.Subject == subj) {
			return near[i].row.Subject == subj
		}
		return near[i].sim > near[j].sim
	})
	if len(near) > 5 {
		near = near[:5]
	}
	existing := make([]memory.Existing, len(near))
	for i, n := range near {
		existing[i] = memory.Existing{Memory: *n.row, Similarity: n.sim}
	}

	d := m.Policy.Decide(c, existing)
	res.Action, res.Reason = d.Action, d.Reason
	var expires *time.Time
	if c.TTL > 0 {
		t := at.Add(c.TTL)
		expires = &t
	}
	byID := func(id string) *memory.Memory {
		for _, r := range m.rows {
			if r.ID == id {
				return r
			}
		}
		return nil
	}
	switch d.Action {
	case memory.Confirm:
		t := byID(d.Target.ID)
		res.MemoryID = t.ID
		t.Confirmations++
		t.LastConfirmed = maxTime(at, t.LastConfirmed)
		t.Confidence = max(t.Confidence, c.Confidence)
		if expires != nil {
			t.ExpiresAt = expires
		}
	case memory.Supersede:
		t := byID(d.Target.ID)
		res.Replaced = t.ID
		closed := at
		t.ValidTo = &closed
		n := m.insert(p.OrgID, c, owner, subj, v, at, expires)
		t.SupersededBy = n.ID
		res.MemoryID = n.ID
	case memory.Add:
		res.MemoryID = m.insert(p.OrgID, c, owner, subj, v, at, expires).ID
	}
	m.writes[wk] = res.Action
	return res
}

func (m *Mem) insert(org string, c memory.Candidate, owner, subj string, v []float32, at time.Time, exp *time.Time) *memory.Memory {
	r := &memory.Memory{ID: newID(), OrgID: org, Scope: c.Scope, OwnerID: owner, Subject: subj, Content: c.Content,
		Confidence: float64(float32(c.Confidence)), Confirmations: 1, SourceRef: c.SourceRef, ValidFrom: at,
		RecordedAt: time.Now(), LastConfirmed: at, ExpiresAt: exp, Embedding: v}
	m.rows = append(m.rows, r)
	return r
}

// visibleTo is the access rule applied on every read.
func visibleTo(r *memory.Memory, p memory.Principal) bool {
	if r.OrgID != p.OrgID {
		return false
	}
	switch r.Scope {
	case memory.User:
		return r.OwnerID == p.UserID
	case memory.Team:
		for _, t := range p.Teams {
			if t == r.OwnerID {
				return true
			}
		}
		return false
	}
	return r.Scope == memory.Org
}

func (m *Mem) Recall(ctx context.Context, p memory.Principal, query string, o RecallOptions) ([]Recalled, error) {
	if o.K <= 0 {
		o.K = 5
	}
	v, err := m.Embedder.Embed(ctx, []string{query})
	if err != nil {
		return nil, err
	}
	at := m.Now()
	if o.AsOf != nil {
		at = *o.AsOf
	}
	scopes := map[memory.Scope]bool{memory.User: true, memory.Team: true, memory.Org: true}
	if len(o.Scopes) > 0 {
		scopes = map[memory.Scope]bool{}
		for _, s := range o.Scopes {
			scopes[s] = true
		}
	}
	m.mu.Lock()
	var out []Recalled
	for _, r := range m.rows {
		if !visibleTo(r, p) || !scopes[r.Scope] || r.ValidFrom.After(at) ||
			r.ValidTo != nil && !r.ValidTo.After(at) || r.ExpiresAt != nil && !r.ExpiresAt.After(at) {
			continue
		}
		out = append(out, Recalled{Memory: *r, Similarity: sim(r.Embedding, v[0])})
	}
	m.mu.Unlock()
	sort.SliceStable(out, func(i, j int) bool { return out[i].Similarity > out[j].Similarity })
	if n := max(o.K*5, 20); len(out) > n {
		out = out[:n]
	}
	for i := range out {
		out[i].Score = m.Weights.Score(out[i].Similarity, out[i].Memory, at)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Score > out[j].Score })
	if len(out) > o.K {
		out = out[:o.K]
	}
	return out, nil
}

func (m *Mem) History(_ context.Context, p memory.Principal, subject string) ([]memory.Memory, error) {
	subj := memory.NormSubject(subject)
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []memory.Memory
	for _, r := range m.rows {
		if visibleTo(r, p) && r.Subject == subj {
			out = append(out, *r)
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		if !out[i].ValidFrom.Equal(out[j].ValidFrom) {
			return out[i].ValidFrom.Before(out[j].ValidFrom)
		}
		return out[i].RecordedAt.Before(out[j].RecordedAt)
	})
	return out, nil
}

// Forget hard-deletes a user's own memory and every version of its subject.
func (m *Mem) Forget(_ context.Context, p memory.Principal, id string) (int64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var subj string
	for _, r := range m.rows {
		if r.ID == id && r.OrgID == p.OrgID && r.Scope == memory.User && r.OwnerID == p.UserID {
			subj = r.Subject
		}
	}
	if subj == "" {
		return 0, ErrNotFound
	}
	kept := m.rows[:0]
	var n int64
	for _, r := range m.rows {
		if r.OrgID == p.OrgID && r.Scope == memory.User && r.OwnerID == p.UserID && r.Subject == subj {
			n++
			continue
		}
		kept = append(kept, r)
	}
	m.rows = kept
	return n, nil
}

func (m *Mem) ExpireDue(context.Context) (int64, error) {
	now := m.Now()
	m.mu.Lock()
	defer m.mu.Unlock()
	var n int64
	for _, r := range m.rows {
		if r.ValidTo == nil && r.ExpiresAt != nil && !r.ExpiresAt.After(now) {
			t := *r.ExpiresAt
			r.ValidTo = &t
			n++
		}
	}
	return n, nil
}

// Consolidate merges near-duplicate current memories of one owner stored
// under different subjects; the one with more confirmations survives.
func (m *Mem) Consolidate(_ context.Context, threshold float64, limit int) (int, error) {
	now := m.Now()
	m.mu.Lock()
	defer m.mu.Unlock()
	type pair struct{ keep, drop *memory.Memory }
	var pairs []pair
	for _, a := range m.rows {
		for _, b := range m.rows {
			if len(pairs) >= limit {
				break
			}
			if a.ID >= b.ID || a.OrgID != b.OrgID || a.Scope != b.Scope || a.OwnerID != b.OwnerID ||
				a.ValidTo != nil || b.ValidTo != nil || a.Subject == b.Subject || sim(a.Embedding, b.Embedding) < threshold {
				continue
			}
			if b.Confirmations > a.Confirmations {
				pairs = append(pairs, pair{b, a})
			} else {
				pairs = append(pairs, pair{a, b})
			}
		}
	}
	seen := map[string]bool{}
	merged := 0
	for _, pr := range pairs {
		if seen[pr.keep.ID] || seen[pr.drop.ID] || pr.drop.ValidTo != nil {
			continue
		}
		seen[pr.keep.ID], seen[pr.drop.ID] = true, true
		t := now
		pr.drop.ValidTo, pr.drop.SupersededBy = &t, pr.keep.ID
		pr.keep.Confirmations += pr.drop.Confirmations
		merged++
	}
	return merged, nil
}

// All returns every row in the org (current and closed), for display.
func (m *Mem) All(org string) []memory.Memory {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []memory.Memory
	for _, r := range m.rows {
		if r.OrgID == org {
			out = append(out, *r)
		}
	}
	return out
}
