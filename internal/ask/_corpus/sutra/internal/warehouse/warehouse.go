// Package warehouse is Sutra's Postgres store: raw source records (one row
// per source record, last write wins by source position), connector
// checkpoints, resolved entities with lineage, and the review queue.
//
// A connector writes a batch of records and its new checkpoint in one
// transaction, so after a crash it resumes exactly where the last committed
// batch ended: no gaps, and replays are harmless upserts.
package warehouse

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Aravindh97/sutra/internal/ontology"
	"github.com/Aravindh97/sutra/internal/resolve"
)

const Schema = `
CREATE TABLE IF NOT EXISTS source_records (
    source     text NOT NULL,
    record_id  text NOT NULL,
    entity     text NOT NULL,
    attrs      jsonb NOT NULL,
    deleted    boolean NOT NULL DEFAULT false,
    source_updated_at timestamptz,
    position   text NOT NULL,      -- LSN or cursor that delivered this version
    seq        numeric NOT NULL,   -- monotonic ordering of position within a source
    synced_at  timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (source, record_id)
);
CREATE INDEX IF NOT EXISTS source_records_entity ON source_records (entity) WHERE NOT deleted;

CREATE TABLE IF NOT EXISTS connector_state (
    source     text PRIMARY KEY,
    checkpoint text NOT NULL,
    updated_at timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS entities (
    entity_id  text PRIMARY KEY,
    entity     text NOT NULL,
    golden     jsonb NOT NULL,
    resolved_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS entities_golden ON entities USING gin (golden jsonb_path_ops);

CREATE TABLE IF NOT EXISTS entity_members (
    entity_id text NOT NULL REFERENCES entities ON DELETE CASCADE,
    source    text NOT NULL,
    record_id text NOT NULL,
    PRIMARY KEY (source, record_id)
);
CREATE INDEX IF NOT EXISTS entity_members_entity ON entity_members (entity_id);

CREATE TABLE IF NOT EXISTS attribute_lineage (
    entity_id  text NOT NULL REFERENCES entities ON DELETE CASCADE,
    attribute  text NOT NULL,
    value      text NOT NULL,
    source     text NOT NULL,
    record_id  text NOT NULL,
    source_updated_at timestamptz,
    PRIMARY KEY (entity_id, attribute)
);

CREATE TABLE IF NOT EXISTS match_evidence (
    entity_id text NOT NULL REFERENCES entities ON DELETE CASCADE,
    a text NOT NULL, b text NOT NULL, score real NOT NULL, reason text NOT NULL
);

CREATE TABLE IF NOT EXISTS review_queue (
    entity    text NOT NULL,
    a text NOT NULL, b text NOT NULL,
    score real NOT NULL, reason text NOT NULL,
    status text NOT NULL DEFAULT 'open' CHECK (status IN ('open','match','no_match')),
    PRIMARY KEY (a, b)
);
`

type DB struct{ Pool *pgxpool.Pool }

func Open(ctx context.Context, url string) (*DB, error) {
	p, err := pgxpool.New(ctx, url)
	if err != nil {
		return nil, err
	}
	if _, err := p.Exec(ctx, Schema); err != nil {
		p.Close()
		return nil, err
	}
	return &DB{Pool: p}, nil
}

func (d *DB) Checkpoint(ctx context.Context, source string) (string, error) {
	var c string
	err := d.Pool.QueryRow(ctx, `SELECT checkpoint FROM connector_state WHERE source=$1`, source).Scan(&c)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil
	}
	return c, err
}

// Versioned is a record plus a sortable sequence number for its position.
type Versioned struct {
	Record ontology.Record
	Seq    uint64
}

// Apply upserts records and stores the checkpoint in one transaction. An
// incoming version only replaces the stored one if its sequence is not
// older, so a replayed or reordered batch cannot roll a record back.
func (d *DB) Apply(ctx context.Context, source, checkpoint string, recs []Versioned) (applied int, err error) {
	tx, err := d.Pool.Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback(ctx)
	for _, v := range recs {
		r := v.Record
		attrs, _ := json.Marshal(r.Attrs)
		var upd *time.Time
		if !r.UpdatedAt.IsZero() {
			upd = &r.UpdatedAt
		}
		tag, err := tx.Exec(ctx, `
			INSERT INTO source_records (source, record_id, entity, attrs, deleted, source_updated_at, position, seq, synced_at)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8, now())
			ON CONFLICT (source, record_id) DO UPDATE SET
				attrs = CASE WHEN EXCLUDED.deleted AND EXCLUDED.attrs = '{}'::jsonb THEN source_records.attrs ELSE EXCLUDED.attrs END,
				deleted = EXCLUDED.deleted, source_updated_at = EXCLUDED.source_updated_at,
				position = EXCLUDED.position, seq = EXCLUDED.seq, synced_at = now()
			WHERE source_records.seq <= EXCLUDED.seq`,
			r.Source, r.ID, r.Entity, attrs, r.Deleted, upd, r.Position, v.Seq)
		if err != nil {
			return applied, err
		}
		applied += int(tag.RowsAffected())
	}
	if checkpoint != "" {
		if _, err := tx.Exec(ctx, `INSERT INTO connector_state (source, checkpoint) VALUES ($1,$2)
			ON CONFLICT (source) DO UPDATE SET checkpoint=EXCLUDED.checkpoint, updated_at=now()`, source, checkpoint); err != nil {
			return applied, err
		}
	}
	return applied, tx.Commit(ctx)
}

func (d *DB) Records(ctx context.Context, entity string) ([]ontology.Record, error) {
	rows, err := d.Pool.Query(ctx, `SELECT source, record_id, entity, attrs, deleted, coalesce(source_updated_at, synced_at), position
		FROM source_records WHERE entity=$1`, entity)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ontology.Record
	for rows.Next() {
		var r ontology.Record
		var attrs []byte
		if err := rows.Scan(&r.Source, &r.ID, &r.Entity, &attrs, &r.Deleted, &r.UpdatedAt, &r.Position); err != nil {
			return nil, err
		}
		if err := json.Unmarshal(attrs, &r.Attrs); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// SaveResolution replaces the resolved entities of one type atomically, so
// readers see either the previous resolution or the new one, never a mix.
// Human review decisions are kept across runs.
func (d *DB) SaveResolution(ctx context.Context, entity string, res resolve.Result) error {
	tx, err := d.Pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, `DELETE FROM entities WHERE entity=$1`, entity); err != nil {
		return err
	}
	for _, e := range res.Entities {
		g, _ := json.Marshal(e.Golden)
		if _, err := tx.Exec(ctx, `INSERT INTO entities (entity_id, entity, golden) VALUES ($1,$2,$3)`, e.ID, entity, g); err != nil {
			return err
		}
		rows := make([][]any, 0, len(e.Members))
		for _, m := range e.Members {
			src, id := splitKey(m)
			rows = append(rows, []any{e.ID, src, id})
		}
		if _, err := tx.CopyFrom(ctx, pgx.Identifier{"entity_members"}, []string{"entity_id", "source", "record_id"}, pgx.CopyFromRows(rows)); err != nil {
			return err
		}
		lin := make([][]any, 0, len(e.Lineage))
		for _, l := range e.Lineage {
			lin = append(lin, []any{e.ID, l.Attribute, l.Value, l.Source, l.RecordID, l.UpdatedAt})
		}
		if _, err := tx.CopyFrom(ctx, pgx.Identifier{"attribute_lineage"},
			[]string{"entity_id", "attribute", "value", "source", "record_id", "source_updated_at"}, pgx.CopyFromRows(lin)); err != nil {
			return err
		}
		for _, p := range e.Reasons {
			if _, err := tx.Exec(ctx, `INSERT INTO match_evidence VALUES ($1,$2,$3,$4,$5)`, e.ID, p.A, p.B, p.Score, p.Reason); err != nil {
				return err
			}
		}
	}
	if _, err := tx.Exec(ctx, `DELETE FROM review_queue WHERE entity=$1 AND status='open'`, entity); err != nil {
		return err
	}
	for _, p := range res.Review {
		if _, err := tx.Exec(ctx, `INSERT INTO review_queue (entity, a, b, score, reason) VALUES ($1,$2,$3,$4,$5)
			ON CONFLICT (a, b) DO NOTHING`, entity, p.A, p.B, p.Score, p.Reason); err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}

// Assignments returns record key -> entity id from the last resolution.
func (d *DB) Assignments(ctx context.Context, entity string) (map[string]string, error) {
	rows, err := d.Pool.Query(ctx, `SELECT m.source, m.record_id, m.entity_id FROM entity_members m
		JOIN entities e USING (entity_id) WHERE e.entity = $1`, entity)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var src, id, eid string
		if err := rows.Scan(&src, &id, &eid); err != nil {
			return nil, err
		}
		out[src+":"+id] = eid
	}
	return out, rows.Err()
}

func splitKey(k string) (string, string) {
	for i := 0; i < len(k); i++ {
		if k[i] == ':' {
			return k[:i], k[i+1:]
		}
	}
	return k, ""
}

type EntityView struct {
	ID       string            `json:"entity_id"`
	Type     string            `json:"type"`
	Golden   map[string]string `json:"golden"`
	Lineage  []resolve.Lineage `json:"lineage,omitempty"`
	Members  []ontology.Record `json:"members,omitempty"`
	Evidence []resolve.Pair    `json:"match_evidence,omitempty"`
}

// Find returns entities whose golden attribute equals value (exact, after the
// caller's normalisation), or whose name contains it when attr is "name".
func (d *DB) Find(ctx context.Context, entity, attr, value string, limit int) ([]EntityView, error) {
	q := `SELECT entity_id, entity, golden FROM entities WHERE entity=$1 AND golden->>$2 = $3 ORDER BY entity_id LIMIT $4`
	if attr == "name" {
		value = "%" + value + "%"
		q = `SELECT entity_id, entity, golden FROM entities WHERE entity=$1 AND golden->>$2 ILIKE $3 ORDER BY entity_id LIMIT $4`
	}
	rows, err := d.Pool.Query(ctx, q, entity, attr, value, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []EntityView
	for rows.Next() {
		var v EntityView
		var g []byte
		if err := rows.Scan(&v.ID, &v.Type, &g); err != nil {
			return nil, err
		}
		json.Unmarshal(g, &v.Golden)
		out = append(out, v)
	}
	return out, rows.Err()
}

var ErrNotFound = errors.New("entity not found")

// Get returns one entity with lineage, member records and match evidence.
func (d *DB) Get(ctx context.Context, id string) (*EntityView, error) {
	var v EntityView
	var g []byte
	err := d.Pool.QueryRow(ctx, `SELECT entity_id, entity, golden FROM entities WHERE entity_id=$1`, id).Scan(&v.ID, &v.Type, &g)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	json.Unmarshal(g, &v.Golden)
	rows, err := d.Pool.Query(ctx, `SELECT attribute, value, source, record_id, coalesce(source_updated_at, 'epoch') FROM attribute_lineage
		WHERE entity_id=$1 ORDER BY attribute`, id)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var l resolve.Lineage
		rows.Scan(&l.Attribute, &l.Value, &l.Source, &l.RecordID, &l.UpdatedAt)
		v.Lineage = append(v.Lineage, l)
	}
	rows.Close()
	rows, err = d.Pool.Query(ctx, `SELECT r.source, r.record_id, r.entity, r.attrs, coalesce(r.source_updated_at, r.synced_at), r.position
		FROM entity_members m JOIN source_records r USING (source, record_id) WHERE m.entity_id=$1 ORDER BY r.source, r.record_id`, id)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var r ontology.Record
		var a []byte
		rows.Scan(&r.Source, &r.ID, &r.Entity, &a, &r.UpdatedAt, &r.Position)
		json.Unmarshal(a, &r.Attrs)
		v.Members = append(v.Members, r)
	}
	rows.Close()
	rows, err = d.Pool.Query(ctx, `SELECT a, b, score, reason FROM match_evidence WHERE entity_id=$1`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var p resolve.Pair
		rows.Scan(&p.A, &p.B, &p.Score, &p.Reason)
		v.Evidence = append(v.Evidence, p)
	}
	return &v, rows.Err()
}
