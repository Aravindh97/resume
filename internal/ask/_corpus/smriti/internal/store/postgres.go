//go:build !(js && wasm)

package store

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Aravindh97/smriti/internal/embed"
	"github.com/Aravindh97/smriti/internal/memory"
)

type Store struct {
	DB       *pgxpool.Pool
	Embedder embed.Embedder
	Policy   memory.Policy
	Weights  memory.Weights
	Now      func() time.Time
}

func schema(dim int) string {
	return fmt.Sprintf(`
CREATE EXTENSION IF NOT EXISTS vector;
CREATE EXTENSION IF NOT EXISTS pgcrypto;

CREATE TABLE IF NOT EXISTS memories (
    id             uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    org_id         text NOT NULL,
    scope          text NOT NULL CHECK (scope IN ('user','team','org')),
    owner_id       text NOT NULL,
    subject        text NOT NULL,
    content        text NOT NULL,
    embedding      vector(%d) NOT NULL,
    confidence     real NOT NULL,
    confirmations  int  NOT NULL DEFAULT 1,
    source_ref     text NOT NULL,
    valid_from     timestamptz NOT NULL,
    valid_to       timestamptz,
    recorded_at    timestamptz NOT NULL DEFAULT now(),
    last_confirmed timestamptz NOT NULL,
    expires_at     timestamptz,
    superseded_by  uuid REFERENCES memories(id)
);
-- At most one current memory per subject per owner.
CREATE UNIQUE INDEX IF NOT EXISTS memories_current_subject
    ON memories (org_id, scope, owner_id, subject) WHERE valid_to IS NULL;
CREATE INDEX IF NOT EXISTS memories_owner ON memories (org_id, scope, owner_id) WHERE valid_to IS NULL;
CREATE INDEX IF NOT EXISTS memories_embedding ON memories USING hnsw (embedding vector_cosine_ops);

-- Every message that produced a candidate, so replaying a conversation
-- (a retried job, a duplicate webhook) does not confirm a fact twice.
CREATE TABLE IF NOT EXISTS memory_writes (
    org_id     text NOT NULL,
    source_ref text NOT NULL,
    subject    text NOT NULL,
    memory_id  uuid,
    action     text NOT NULL,
    written_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (org_id, source_ref, subject)
);`, dim)
}

func Open(ctx context.Context, url string, e embed.Embedder) (*Store, error) {
	db, err := pgxpool.New(ctx, url)
	if err != nil {
		return nil, err
	}
	if _, err := db.Exec(ctx, schema(e.Dim())); err != nil {
		db.Close()
		return nil, err
	}
	return &Store{DB: db, Embedder: e, Policy: memory.DefaultPolicy(), Weights: memory.DefaultWeights(), Now: time.Now}, nil
}

func vec(v []float32) string {
	var b strings.Builder
	b.WriteByte('[')
	for i, x := range v {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(strconv.FormatFloat(float64(x), 'f', 6, 32))
	}
	b.WriteByte(']')
	return b.String()
}

// Write reconciles and stores candidates for principal p. Each candidate is
// handled in its own transaction under an advisory lock on (org, scope,
// owner), so two concurrent writers for the same owner cannot both insert a
// "current" value for the same subject.
func (s *Store) Write(ctx context.Context, p memory.Principal, cands []memory.Candidate) ([]WriteResult, error) {
	out := make([]WriteResult, 0, len(cands))
	texts := make([]string, len(cands))
	for i, c := range cands {
		texts[i] = c.Subject + ": " + c.Content
	}
	vecs, err := s.Embedder.Embed(ctx, texts)
	if err != nil {
		return nil, err
	}
	for i, c := range cands {
		r, err := s.writeOne(ctx, p, c, vecs[i])
		if err != nil {
			return out, err
		}
		out = append(out, r)
	}
	return out, nil
}

func (s *Store) writeOne(ctx context.Context, p memory.Principal, c memory.Candidate, v []float32) (WriteResult, error) {
	res := WriteResult{Candidate: c}
	owner, err := p.Owner(c.Scope, c.Team)
	if err != nil {
		res.Action, res.Reason = Denied, err.Error()
		return res, nil
	}
	subj := memory.NormSubject(c.Subject)
	now := s.Now()
	if c.ObservedAt.IsZero() || c.ObservedAt.After(now) {
		c.ObservedAt = now
	}
	at := c.ObservedAt

	tx, err := s.DB.Begin(ctx)
	if err != nil {
		return res, err
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1, 0))`,
		p.OrgID+"|"+string(c.Scope)+"|"+owner); err != nil {
		return res, err
	}
	tag, err := tx.Exec(ctx, `INSERT INTO memory_writes (org_id, source_ref, subject, action) VALUES ($1,$2,$3,'pending')
		ON CONFLICT DO NOTHING`, p.OrgID, c.SourceRef, subj)
	if err != nil {
		return res, err
	}
	if tag.RowsAffected() == 0 {
		res.Action, res.Reason = Duplicate, "source already processed"
		return res, tx.Commit(ctx)
	}

	rows, err := tx.Query(ctx, `
		SELECT `+cols+`, 1 - (embedding <=> $4::vector) AS sim
		FROM memories
		WHERE org_id=$1 AND scope=$2 AND owner_id=$3 AND valid_to IS NULL
		  AND (subject = $5 OR 1 - (embedding <=> $4::vector) >= $6)
		ORDER BY (subject = $5) DESC, embedding <=> $4::vector
		LIMIT 5`, p.OrgID, c.Scope, owner, vec(v), subj, s.Policy.Duplicate)
	if err != nil {
		return res, err
	}
	var existing []memory.Existing
	for rows.Next() {
		var e memory.Existing
		if err := scanInto(rows, &e.Memory, &e.Similarity); err != nil {
			rows.Close()
			return res, err
		}
		existing = append(existing, e)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return res, err
	}

	d := s.Policy.Decide(c, existing)
	res.Action, res.Reason = d.Action, d.Reason
	var expires *time.Time
	if c.TTL > 0 {
		t := at.Add(c.TTL)
		expires = &t
	}
	switch d.Action {
	case memory.Confirm:
		res.MemoryID = d.Target.ID
		_, err = tx.Exec(ctx, `UPDATE memories SET confirmations = confirmations + 1, last_confirmed = $2,
			confidence = GREATEST(confidence, $3), expires_at = COALESCE($4, expires_at) WHERE id = $1`,
			d.Target.ID, maxTime(at, d.Target.LastConfirmed), c.Confidence, expires)
	case memory.Supersede:
		res.Replaced = d.Target.ID
		if _, err = tx.Exec(ctx, `UPDATE memories SET valid_to = $2 WHERE id = $1`, d.Target.ID, at); err != nil {
			return res, err
		}
		res.MemoryID, err = insert(ctx, tx, p.OrgID, c, owner, subj, v, at, expires)
		if err == nil {
			_, err = tx.Exec(ctx, `UPDATE memories SET superseded_by = $2 WHERE id = $1`, d.Target.ID, res.MemoryID)
		}
	case memory.Add:
		res.MemoryID, err = insert(ctx, tx, p.OrgID, c, owner, subj, v, at, expires)
	}
	if err != nil {
		return res, err
	}
	if _, err := tx.Exec(ctx, `UPDATE memory_writes SET action=$4, memory_id=NULLIF($5,'')::uuid
		WHERE org_id=$1 AND source_ref=$2 AND subject=$3`, p.OrgID, c.SourceRef, subj, string(res.Action), res.MemoryID); err != nil {
		return res, err
	}
	return res, tx.Commit(ctx)
}

// insert writes a new current memory valid from `at`; recorded_at defaults to now().
func insert(ctx context.Context, tx pgx.Tx, org string, c memory.Candidate, owner, subj string, v []float32, at time.Time, exp *time.Time) (string, error) {
	var id string
	err := tx.QueryRow(ctx, `INSERT INTO memories (org_id, scope, owner_id, subject, content, embedding, confidence,
			source_ref, valid_from, last_confirmed, expires_at)
		VALUES ($1,$2,$3,$4,$5,$6::vector,$7,$8,$9,$9,$10) RETURNING id::text`,
		org, c.Scope, owner, subj, c.Content, vec(v), c.Confidence, c.SourceRef, at, exp).Scan(&id)
	return id, err
}

const cols = `id::text, org_id, scope, owner_id, subject, content, confidence, confirmations, source_ref,
	valid_from, valid_to, recorded_at, coalesce(superseded_by::text,''), expires_at, last_confirmed`

func scanInto(row pgx.Row, m *memory.Memory, extra ...any) error {
	dest := []any{&m.ID, &m.OrgID, &m.Scope, &m.OwnerID, &m.Subject, &m.Content, &m.Confidence, &m.Confirmations,
		&m.SourceRef, &m.ValidFrom, &m.ValidTo, &m.RecordedAt, &m.SupersededBy, &m.ExpiresAt, &m.LastConfirmed}
	return row.Scan(append(dest, extra...)...)
}

// visible is the access rule, applied in every read: a principal sees its own
// user memories, memories of teams it belongs to, and its org's memories.
const visible = `org_id = $1 AND ((scope = 'user' AND owner_id = $2) OR (scope = 'team' AND owner_id = ANY($3)) OR scope = 'org')`

func (s *Store) Recall(ctx context.Context, p memory.Principal, query string, o RecallOptions) ([]Recalled, error) {
	if o.K <= 0 {
		o.K = 5
	}
	v, err := s.Embedder.Embed(ctx, []string{query})
	if err != nil {
		return nil, err
	}
	now := s.Now()
	at := now
	if o.AsOf != nil {
		at = *o.AsOf
	}
	scopes := []string{"user", "team", "org"}
	if len(o.Scopes) > 0 {
		scopes = scopes[:0]
		for _, sc := range o.Scopes {
			scopes = append(scopes, string(sc))
		}
	}
	teams := p.Teams
	if teams == nil {
		teams = []string{}
	}
	rows, err := s.DB.Query(ctx, `
		SELECT `+cols+`, 1 - (embedding <=> $4::vector) AS sim
		FROM memories
		WHERE `+visible+` AND scope = ANY($5)
		  AND valid_from <= $6 AND (valid_to IS NULL OR valid_to > $6)
		  AND (expires_at IS NULL OR expires_at > $6)
		ORDER BY embedding <=> $4::vector
		LIMIT $7`, p.OrgID, p.UserID, teams, vec(v[0]), scopes, at, max(o.K*5, 20))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Recalled
	for rows.Next() {
		var r Recalled
		if err := scanInto(rows, &r.Memory, &r.Similarity); err != nil {
			return nil, err
		}
		r.Score = s.Weights.Score(r.Similarity, r.Memory, at)
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Score > out[j].Score })
	if len(out) > o.K {
		out = out[:o.K]
	}
	return out, nil
}

// History returns every version of a subject visible to p, oldest first.
func (s *Store) History(ctx context.Context, p memory.Principal, subject string) ([]memory.Memory, error) {
	teams := p.Teams
	if teams == nil {
		teams = []string{}
	}
	rows, err := s.DB.Query(ctx, `SELECT `+cols+` FROM memories WHERE `+visible+` AND subject = $4 ORDER BY valid_from, recorded_at`,
		p.OrgID, p.UserID, teams, memory.NormSubject(subject))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []memory.Memory
	for rows.Next() {
		var m memory.Memory
		if err := scanInto(rows, &m); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// Forget hard-deletes a user-scope memory and all its versions. Users can
// only forget their own memories.
func (s *Store) Forget(ctx context.Context, p memory.Principal, id string) (int64, error) {
	tx, err := s.DB.Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback(ctx)
	var subj string
	err = tx.QueryRow(ctx, `SELECT subject FROM memories WHERE id::text=$1 AND org_id=$2 AND scope='user' AND owner_id=$3`,
		id, p.OrgID, p.UserID).Scan(&subj)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, ErrNotFound
	}
	if err != nil {
		return 0, err
	}
	if _, err := tx.Exec(ctx, `UPDATE memories SET superseded_by = NULL WHERE org_id=$1 AND scope='user' AND owner_id=$2 AND subject=$3`,
		p.OrgID, p.UserID, subj); err != nil {
		return 0, err
	}
	tag, err := tx.Exec(ctx, `DELETE FROM memories WHERE org_id=$1 AND scope='user' AND owner_id=$2 AND subject=$3`, p.OrgID, p.UserID, subj)
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), tx.Commit(ctx)
}

// ExpireDue closes memories whose TTL has passed.
func (s *Store) ExpireDue(ctx context.Context) (int64, error) {
	tag, err := s.DB.Exec(ctx, `UPDATE memories SET valid_to = expires_at WHERE valid_to IS NULL AND expires_at <= $1`, s.Now())
	return tag.RowsAffected(), err
}

// Consolidate merges near-duplicate current memories of the same owner that
// were stored under different subjects (e.g. "manager" and "reporting
// manager"). The memory with more confirmations is kept and absorbs the
// other's confirmations; the other is closed and points at the survivor.
func (s *Store) Consolidate(ctx context.Context, threshold float64, limit int) (int, error) {
	rows, err := s.DB.Query(ctx, `
		SELECT a.id::text, b.id::text, a.confirmations, b.confirmations
		FROM memories a JOIN memories b
		  ON a.org_id = b.org_id AND a.scope = b.scope AND a.owner_id = b.owner_id AND a.id < b.id
		WHERE a.valid_to IS NULL AND b.valid_to IS NULL AND a.subject <> b.subject
		  AND 1 - (a.embedding <=> b.embedding) >= $1
		LIMIT $2`, threshold, limit)
	if err != nil {
		return 0, err
	}
	type pair struct {
		keep, drop string
		n          int
	}
	var pairs []pair
	for rows.Next() {
		var a, b string
		var ca, cb int
		if err := rows.Scan(&a, &b, &ca, &cb); err != nil {
			rows.Close()
			return 0, err
		}
		if cb > ca {
			a, b, ca, cb = b, a, cb, ca
		}
		pairs = append(pairs, pair{a, b, cb})
	}
	rows.Close()
	merged := 0
	seen := map[string]bool{}
	now := s.Now()
	for _, pr := range pairs {
		if seen[pr.keep] || seen[pr.drop] {
			continue
		}
		seen[pr.keep], seen[pr.drop] = true, true
		tx, err := s.DB.Begin(ctx)
		if err != nil {
			return merged, err
		}
		tag, err := tx.Exec(ctx, `UPDATE memories SET valid_to=$2, superseded_by=$3 WHERE id=$1 AND valid_to IS NULL`, pr.drop, now, pr.keep)
		if err == nil && tag.RowsAffected() == 1 {
			_, err = tx.Exec(ctx, `UPDATE memories SET confirmations = confirmations + $2 WHERE id=$1`, pr.keep, pr.n)
		}
		if err != nil {
			tx.Rollback(ctx)
			return merged, err
		}
		if err := tx.Commit(ctx); err != nil {
			return merged, err
		}
		if tag.RowsAffected() == 1 {
			merged++
		}
	}
	return merged, nil
}

func (s *Store) SetClock(now func() time.Time) { s.Now = now }
func (s *Store) Embeds() embed.Embedder        { return s.Embedder }
