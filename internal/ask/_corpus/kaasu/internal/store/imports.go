package store

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/Aravindh97/kaasu/internal/analyse"
	"github.com/Aravindh97/kaasu/internal/auth"
)

type Import struct {
	ID        int64           `json:"id"`
	Filename  string          `json:"filename"`
	Mode      string          `json:"mode"` // server: rows kept here; device: only this summary was sent
	RowCount  int             `json:"row_count"`
	Summary   analyse.Summary `json:"summary"`
	CreatedAt time.Time       `json:"created_at"`
	Rows      []analyse.Txn   `json:"rows,omitempty"`
	RowIDs    []int64         `json:"row_ids,omitempty"`
	Skipped   []analyse.Skip  `json:"skipped,omitempty"`
	Columns   string          `json:"columns,omitempty"`
}

func (s *Store) rules(ctx context.Context, q interface {
	Query(context.Context, string, ...any) (pgx.Rows, error)
}, user int64) (map[string]string, error) {
	rows, err := q.Query(ctx, `select merchant, category from category_rules where user_id = $1`, user)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var m, c string
		if err := rows.Scan(&m, &c); err != nil {
			return nil, err
		}
		out[m] = c
	}
	return out, rows.Err()
}

// Rules returns what the user has taught Kaasu, for the in-browser analyser.
func (s *Store) Rules(ctx context.Context, u *auth.User) (map[string]string, error) {
	if err := member(u); err != nil {
		return nil, err
	}
	return s.rules(ctx, s.DB, u.ID)
}

func cleanName(n string) string {
	n = strings.TrimSpace(n)
	if i := strings.LastIndexAny(n, `/\`); i >= 0 {
		n = n[i+1:]
	}
	if len(n) > 80 {
		n = n[:80]
	}
	if n == "" {
		n = "statement.csv"
	}
	return n
}

// ImportOnServer parses and analyses a CSV statement here and keeps its rows,
// so the budget page can count them and corrections can re-categorise them.
func (s *Store) ImportOnServer(ctx context.Context, u *auth.User, filename string, csv []byte) (*Import, error) {
	if err := member(u); err != nil {
		return nil, err
	}
	p, err := analyse.Parse(bytes.NewReader(csv))
	if err != nil {
		return nil, problem("file", "%s", err.Error())
	}
	rules, err := s.rules(ctx, s.DB, u.ID)
	if err != nil {
		return nil, err
	}
	sum := analyse.Analyse(p.Txns, rules)
	var id int64
	err = s.tx(ctx, func(tx pgx.Tx) error {
		if err := tx.QueryRow(ctx, `insert into imports (user_id, filename, mode, summary, row_count, created_at) values ($1, $2, 'server', $3, $4, $5) returning id`,
			u.ID, cleanName(filename), sum, len(p.Txns), s.Now()).Scan(&id); err != nil {
			return err
		}
		rows := make([][]any, len(p.Txns))
		for i, t := range p.Txns {
			rows[i] = []any{id, u.ID, t.Line, t.Date, t.Description, t.Merchant, t.Amount, t.Category, t.Learned}
		}
		if _, err := tx.CopyFrom(ctx, pgx.Identifier{"statement_rows"}, []string{"import_id", "user_id", "line", "day", "description", "merchant", "amount", "category", "learned"}, pgx.CopyFromRows(rows)); err != nil {
			return err
		}
		return notify(ctx, tx, userTopic(u.ID))
	})
	if err != nil {
		return nil, err
	}
	im, err := s.GetImport(ctx, u, id)
	if err != nil {
		return nil, err
	}
	im.Skipped, im.Columns = p.Skipped, p.Columns
	return im, nil
}

type DeviceImport struct {
	Filename string          `json:"filename"`
	Summary  analyse.Summary `json:"summary"`
}

// ImportFromDevice stores only a summary made in the browser. The rows
// never reach the server, so there is nothing to check them against; the
// summary is the user's own view of their own data, and is size-checked.
func (s *Store) ImportFromDevice(ctx context.Context, u *auth.User, in DeviceImport) (*Import, error) {
	if err := member(u); err != nil {
		return nil, err
	}
	if err := checkSummary(in.Summary); err != nil {
		return nil, err
	}
	sm := in.Summary
	var id int64
	err := s.tx(ctx, func(tx pgx.Tx) error {
		if err := tx.QueryRow(ctx, `insert into imports (user_id, filename, mode, summary, row_count, created_at) values ($1, $2, 'device', $3, $4, $5) returning id`,
			u.ID, cleanName(in.Filename), sm, sm.Count, s.Now()).Scan(&id); err != nil {
			return err
		}
		return notify(ctx, tx, userTopic(u.ID))
	})
	if err != nil {
		return nil, err
	}
	return s.GetImport(ctx, u, id)
}

func (s *Store) Imports(ctx context.Context, u *auth.User) ([]Import, error) {
	if err := member(u); err != nil {
		return nil, err
	}
	rows, err := s.DB.Query(ctx, `select id, filename, mode, row_count, summary, created_at from imports where user_id = $1 order by id desc limit 50`, u.ID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Import{}
	for rows.Next() {
		var im Import
		if err := rows.Scan(&im.ID, &im.Filename, &im.Mode, &im.RowCount, &im.Summary, &im.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, im)
	}
	return out, rows.Err()
}

func (s *Store) GetImport(ctx context.Context, u *auth.User, id int64) (*Import, error) {
	var im Import
	err := s.DB.QueryRow(ctx, `select id, filename, mode, row_count, summary, created_at from imports where id = $1 and user_id = $2`, id, u.ID).
		Scan(&im.ID, &im.Filename, &im.Mode, &im.RowCount, &im.Summary, &im.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	rows, err := s.DB.Query(ctx, `select id, line, to_char(day, 'YYYY-MM-DD'), description, merchant, amount, category, learned from statement_rows where import_id = $1 order by day, line`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var t analyse.Txn
		var rid int64
		if err := rows.Scan(&rid, &t.Line, &t.Date, &t.Description, &t.Merchant, &t.Amount, &t.Category, &t.Learned); err != nil {
			return nil, err
		}
		im.Rows, im.RowIDs = append(im.Rows, t), append(im.RowIDs, rid)
	}
	return &im, rows.Err()
}

func (s *Store) DeleteImport(ctx context.Context, u *auth.User, id int64) error {
	tag, err := s.DB.Exec(ctx, `delete from imports where id = $1 and user_id = $2`, id, u.ID)
	if err == nil && tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return err
}

type Teach struct {
	Merchant string `json:"merchant"`
	Category string `json:"category"`
}

// Teach saves "this merchant is this category" and applies it to every
// server-kept row from that merchant, then re-analyses those imports. A
// device-mode import is re-analysed in the browser with the new rule.
func (s *Store) Teach(ctx context.Context, u *auth.User, in Teach) (int, error) {
	if err := member(u); err != nil {
		return 0, err
	}
	in.Merchant = strings.ToUpper(strings.TrimSpace(in.Merchant))
	ok := false
	for _, c := range analyse.Categories() {
		ok = ok || c == in.Category
	}
	switch {
	case !ok:
		return 0, problem("category", "choose a category from the list")
	case in.Merchant == "" || in.Merchant == "UNKNOWN" || len(in.Merchant) > 60:
		return 0, problem("merchant", "this row has no merchant name to learn from")
	}
	changed := 0
	err := s.tx(ctx, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `insert into category_rules (user_id, merchant, category, updated_at) values ($1, $2, $3, $4)
			on conflict (user_id, merchant) do update set category = excluded.category, updated_at = excluded.updated_at`, u.ID, in.Merchant, in.Category, s.Now()); err != nil {
			return err
		}
		tag, err := tx.Exec(ctx, `update statement_rows set category = $3, learned = true where user_id = $1 and merchant = $2`, u.ID, in.Merchant, in.Category)
		if err != nil {
			return err
		}
		changed = int(tag.RowsAffected())
		rules, err := s.rules(ctx, tx, u.ID)
		if err != nil {
			return err
		}
		ids := []int64{}
		r, err := tx.Query(ctx, `select distinct import_id from statement_rows where user_id = $1 and merchant = $2`, u.ID, in.Merchant)
		if err != nil {
			return err
		}
		for r.Next() {
			var id int64
			if err := r.Scan(&id); err != nil {
				r.Close()
				return err
			}
			ids = append(ids, id)
		}
		r.Close()
		for _, id := range ids {
			if err := reanalyse(ctx, tx, id, rules); err != nil {
				return err
			}
		}
		return notify(ctx, tx, userTopic(u.ID))
	})
	return changed, err
}

func reanalyse(ctx context.Context, tx pgx.Tx, importID int64, rules map[string]string) error {
	rows, err := tx.Query(ctx, `select line, to_char(day, 'YYYY-MM-DD'), description, merchant, amount from statement_rows where import_id = $1`, importID)
	if err != nil {
		return err
	}
	var txns []analyse.Txn
	for rows.Next() {
		var t analyse.Txn
		if err := rows.Scan(&t.Line, &t.Date, &t.Description, &t.Merchant, &t.Amount); err != nil {
			rows.Close()
			return err
		}
		txns = append(txns, t)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	sum := analyse.Analyse(txns, rules)
	_, err = tx.Exec(ctx, `update imports set summary = $2 where id = $1`, importID, sum)
	return err
}

// UpdateDeviceSummary replaces a device-mode import's summary after the
// browser re-analysed it with a new rule.
func (s *Store) UpdateDeviceSummary(ctx context.Context, u *auth.User, id int64, in DeviceImport) (*Import, error) {
	if err := member(u); err != nil {
		return nil, err
	}
	if err := checkSummary(in.Summary); err != nil {
		return nil, err
	}
	tag, err := s.DB.Exec(ctx, `update imports set summary = $3 where id = $1 and user_id = $2 and mode = 'device'`, id, u.ID, in.Summary)
	if err != nil {
		return nil, err
	}
	if tag.RowsAffected() == 0 {
		return nil, ErrNotFound
	}
	return s.GetImport(ctx, u, id)
}

// checkSummary bounds a summary sent from a browser.
func checkSummary(sm analyse.Summary) error {
	if sm.Count <= 0 || sm.Count > analyse.MaxRows || len(sm.Months) > 120 || len(sm.Categories) > 60 || len(sm.Findings) > 300 {
		return problem("summary", "that summary doesn't look like one Kaasu made")
	}
	for _, f := range sm.Findings {
		if len(f.Title) > 300 || len(f.Detail) > 600 || len(f.Lines) > analyse.MaxRows {
			return problem("summary", "that summary doesn't look like one Kaasu made")
		}
	}
	return nil
}
