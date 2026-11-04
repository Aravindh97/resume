// Package db opens the Postgres pool and applies the embedded migrations.
package db

import (
	"context"
	"embed"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5/pgxpool"
)

//go:embed migrations/*.sql
var migrations embed.FS

func Open(ctx context.Context, url string) (*pgxpool.Pool, error) {
	cfg, err := pgxpool.ParseConfig(url)
	if err != nil {
		return nil, err
	}
	if cfg.MaxConns < 8 {
		cfg.MaxConns = 8
	}
	p, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, err
	}
	if err := p.Ping(ctx); err != nil {
		p.Close()
		return nil, err
	}
	return p, nil
}

// Migrate applies migrations not yet recorded, each in its own transaction.
// An advisory lock makes two servers starting at once take turns.
func Migrate(ctx context.Context, p *pgxpool.Pool) error {
	c, err := p.Acquire(ctx)
	if err != nil {
		return err
	}
	defer c.Release()
	if _, err := c.Exec(ctx, `select pg_advisory_lock(524278)`); err != nil {
		return err
	}
	defer c.Exec(context.Background(), `select pg_advisory_unlock(524278)`)
	if _, err := c.Exec(ctx, `create table if not exists schema_migrations (version int primary key, applied_at timestamptz not null default now())`); err != nil {
		return err
	}
	entries, _ := migrations.ReadDir("migrations")
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })
	for _, e := range entries {
		v, err := strconv.Atoi(strings.SplitN(e.Name(), "_", 2)[0])
		if err != nil {
			return fmt.Errorf("migration %s: name must start with a number", e.Name())
		}
		var done bool
		if err := c.QueryRow(ctx, `select exists (select 1 from schema_migrations where version = $1)`, v).Scan(&done); err != nil {
			return err
		}
		if done {
			continue
		}
		sql, _ := migrations.ReadFile("migrations/" + e.Name())
		tx, err := c.Begin(ctx)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, string(sql)); err != nil {
			tx.Rollback(ctx)
			return fmt.Errorf("migration %s: %w", e.Name(), err)
		}
		if _, err := tx.Exec(ctx, `insert into schema_migrations (version) values ($1)`, v); err != nil {
			tx.Rollback(ctx)
			return err
		}
		if err := tx.Commit(ctx); err != nil {
			return err
		}
	}
	return nil
}
