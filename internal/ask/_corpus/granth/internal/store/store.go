// Package store is the document ledger: one row per source document with its
// ingestion status. Failed documents stay in the ledger with the step and
// error that failed them, which is the dead-letter queue an operator works
// from (`granth failed`, then `granth retry`).
package store

import (
	"context"
	"errors"
	"sort"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type Status string

const (
	Pending Status = "pending"
	Indexed Status = "indexed"
	Failed  Status = "failed"
)

var ErrNotFound = errors.New("document not found")

type Record struct {
	DocID       string     `json:"doc_id"`
	SourceURI   string     `json:"source_uri"`
	ContentHash string     `json:"content_hash"`
	Status      Status     `json:"status"`
	ChunkCount  int        `json:"chunk_count"`
	Attempts    int        `json:"attempts"`
	FailedStep  string     `json:"failed_step,omitempty"`
	LastError   string     `json:"last_error,omitempty"`
	Embedder    string     `json:"embedder,omitempty"`
	UpdatedAt   time.Time  `json:"updated_at"`
	IndexedAt   *time.Time `json:"indexed_at,omitempty"`
}

type Store interface {
	Get(ctx context.Context, docID string) (*Record, error)
	MarkStarted(ctx context.Context, docID, uri, hash string) error
	MarkIndexed(ctx context.Context, docID, hash string, chunks int, embedder string) error
	MarkFailed(ctx context.Context, docID, uri, step, msg string) error
	// List returns records with doc_id > after, ordered by doc_id, so callers
	// can page through the ledger with a stable cursor.
	List(ctx context.Context, status Status, after string, limit int) ([]Record, error)
}

const Schema = `
CREATE TABLE IF NOT EXISTS documents (
    doc_id       text PRIMARY KEY,
    source_uri   text NOT NULL UNIQUE,
    content_hash text,
    status       text NOT NULL CHECK (status IN ('pending','indexed','failed')),
    chunk_count  int  NOT NULL DEFAULT 0,
    attempts     int  NOT NULL DEFAULT 0,
    failed_step  text,
    last_error   text,
    embedder     text,
    updated_at   timestamptz NOT NULL DEFAULT now(),
    indexed_at   timestamptz
);
CREATE INDEX IF NOT EXISTS documents_failed_idx ON documents (doc_id) WHERE status = 'failed';
`

type Postgres struct{ DB *pgxpool.Pool }

func OpenPostgres(ctx context.Context, url string) (*Postgres, error) {
	db, err := pgxpool.New(ctx, url)
	if err != nil {
		return nil, err
	}
	if _, err := db.Exec(ctx, Schema); err != nil {
		db.Close()
		return nil, err
	}
	return &Postgres{DB: db}, nil
}

const cols = `doc_id, source_uri, coalesce(content_hash,''), status, chunk_count, attempts,
	coalesce(failed_step,''), coalesce(last_error,''), coalesce(embedder,''), updated_at, indexed_at`

func scan(row pgx.Row) (*Record, error) {
	var r Record
	err := row.Scan(&r.DocID, &r.SourceURI, &r.ContentHash, &r.Status, &r.ChunkCount, &r.Attempts,
		&r.FailedStep, &r.LastError, &r.Embedder, &r.UpdatedAt, &r.IndexedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	return &r, err
}

func (p *Postgres) Get(ctx context.Context, id string) (*Record, error) {
	return scan(p.DB.QueryRow(ctx, `SELECT `+cols+` FROM documents WHERE doc_id = $1`, id))
}

func (p *Postgres) MarkStarted(ctx context.Context, id, uri, hash string) error {
	// An indexed document keeps status 'indexed' until the new version is
	// written, so searches keep working during a re-ingest.
	_, err := p.DB.Exec(ctx, `
		INSERT INTO documents (doc_id, source_uri, content_hash, status, attempts)
		VALUES ($1, $2, $3, 'pending', 1)
		ON CONFLICT (doc_id) DO UPDATE SET
			attempts = documents.attempts + 1,
			status = CASE WHEN documents.status = 'indexed' THEN 'indexed' ELSE 'pending' END,
			updated_at = now()`, id, uri, hash)
	return err
}

func (p *Postgres) MarkIndexed(ctx context.Context, id, hash string, n int, emb string) error {
	tag, err := p.DB.Exec(ctx, `
		UPDATE documents SET status='indexed', content_hash=$2, chunk_count=$3, embedder=$4,
			failed_step=NULL, last_error=NULL, updated_at=now(), indexed_at=now()
		WHERE doc_id=$1`, id, hash, n, emb)
	if err == nil && tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return err
}

func (p *Postgres) MarkFailed(ctx context.Context, id, uri, step, msg string) error {
	_, err := p.DB.Exec(ctx, `
		INSERT INTO documents (doc_id, source_uri, status, failed_step, last_error, attempts)
		VALUES ($1, $2, 'failed', $3, $4, 1)
		ON CONFLICT (doc_id) DO UPDATE SET status='failed', failed_step=$3, last_error=$4, updated_at=now()`,
		id, uri, step, truncate(msg, 2000))
	return err
}

func (p *Postgres) List(ctx context.Context, st Status, after string, limit int) ([]Record, error) {
	rows, err := p.DB.Query(ctx, `SELECT `+cols+` FROM documents
		WHERE ($1 = '' OR status = $1) AND doc_id > $2 ORDER BY doc_id LIMIT $3`, string(st), after, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Record
	for rows.Next() {
		r, err := scan(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *r)
	}
	return out, rows.Err()
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

// Memory is an in-process Store for tests and local runs.
type Memory struct {
	mu sync.Mutex
	m  map[string]*Record
}

func NewMemory() *Memory { return &Memory{m: map[string]*Record{}} }

func (s *Memory) Get(_ context.Context, id string) (*Record, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.m[id]
	if !ok {
		return nil, ErrNotFound
	}
	cp := *r
	return &cp, nil
}

func (s *Memory) MarkStarted(_ context.Context, id, uri, hash string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.m[id]
	if !ok {
		s.m[id] = &Record{DocID: id, SourceURI: uri, ContentHash: hash, Status: Pending, Attempts: 1, UpdatedAt: time.Now()}
		return nil
	}
	r.Attempts++
	if r.Status != Indexed {
		r.Status = Pending
	}
	r.UpdatedAt = time.Now()
	return nil
}

func (s *Memory) MarkIndexed(_ context.Context, id, hash string, n int, emb string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.m[id]
	if !ok {
		return ErrNotFound
	}
	now := time.Now()
	r.Status, r.ContentHash, r.ChunkCount, r.Embedder = Indexed, hash, n, emb
	r.FailedStep, r.LastError, r.UpdatedAt, r.IndexedAt = "", "", now, &now
	return nil
}

func (s *Memory) MarkFailed(_ context.Context, id, uri, step, msg string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.m[id]
	if !ok {
		r = &Record{DocID: id, SourceURI: uri, Attempts: 1}
		s.m[id] = r
	}
	r.Status, r.FailedStep, r.LastError, r.UpdatedAt = Failed, step, truncate(msg, 2000), time.Now()
	return nil
}

func (s *Memory) List(_ context.Context, st Status, after string, limit int) ([]Record, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []Record
	for _, r := range s.m {
		if (st == "" || r.Status == st) && r.DocID > after {
			out = append(out, *r)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].DocID < out[j].DocID })
	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}
