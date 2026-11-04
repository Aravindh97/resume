package store

import (
	"bytes"
	"context"
	"encoding/csv"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/Aravindh97/kaasu/internal/auth"
	"github.com/Aravindh97/kaasu/internal/money"
)

// ---- Month-end reports ----------------------------------------------------

type Amount struct {
	Name   string `json:"name"`
	Amount int64  `json:"amount"`
}

type BudgetResult struct {
	Category string `json:"category"`
	Limit    int64  `json:"limit"`
	Spent    int64  `json:"spent"`
}

type ReportData struct {
	Month      string         `json:"month"` // YYYY-MM
	In         int64          `json:"in"`
	Out        int64          `json:"out"`
	Closing    int64          `json:"closing"` // wallet balance at the end of the month
	Txns       int            `json:"txns"`
	Spending   []Amount       `json:"spending"` // by category: wallet payments plus server-kept statement rows
	TopPayees  []Amount       `json:"top_payees"`
	Budgets    []BudgetResult `json:"budgets"`
	SavedGoals int64          `json:"saved_to_goals"` // net, into goals
	Goals      []Amount       `json:"goals"`          // saved so far, at the month's end
}

type Report struct {
	Month     string     `json:"month"`
	Data      ReportData `json:"data"`
	CreatedAt time.Time  `json:"created_at"`
}

// RunMonthEnd makes the report for `month` for every member who doesn't
// have one. Safe to run on several servers at once or twice in a row: the
// (user, month) primary key keeps one report each, and a transaction-level
// advisory lock stops two runs doing the same work at the same time.
func (s *Store) RunMonthEnd(ctx context.Context, month time.Time) (int, error) {
	from := monthStart(month)
	to := from.AddDate(0, 1, 0)
	if to.After(s.Now()) {
		return 0, fmt.Errorf("month %s hasn't ended yet", from.Format("2006-01"))
	}
	made := 0
	err := s.tx(ctx, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `select pg_advisory_xact_lock(524279)`); err != nil {
			return err
		}
		rows, err := tx.Query(ctx, `select u.id from users u join accounts a on a.user_id = u.id and a.kind = 'wallet'
			where u.role = 'member' and a.created_at < $1 and not exists (select 1 from reports r where r.user_id = u.id and r.month = $2::date) order by u.id`, to, from)
		if err != nil {
			return err
		}
		var users []int64
		for rows.Next() {
			var id int64
			if err := rows.Scan(&id); err != nil {
				rows.Close()
				return err
			}
			users = append(users, id)
		}
		rows.Close()
		for _, id := range users {
			d, err := buildReport(ctx, tx, id, from, to)
			if err != nil {
				return err
			}
			tag, err := tx.Exec(ctx, `insert into reports (user_id, month, data, created_at) values ($1, $2::date, $3, $4) on conflict do nothing`, id, from, d, s.Now())
			if err != nil {
				return err
			}
			if tag.RowsAffected() == 1 {
				made++
				if err := notify(ctx, tx, userTopic(id)); err != nil {
					return err
				}
			}
		}
		return nil
	})
	return made, err
}

func buildReport(ctx context.Context, tx pgx.Tx, user int64, from, to time.Time) (*ReportData, error) {
	d := &ReportData{Month: from.Format("2006-01"), Spending: []Amount{}, TopPayees: []Amount{}, Budgets: []BudgetResult{}, Goals: []Amount{}}
	var wallet int64
	if err := tx.QueryRow(ctx, `select id from accounts where user_id = $1 and kind = 'wallet'`, user).Scan(&wallet); err != nil {
		return nil, err
	}
	// An aggregate without group by always returns a row, even for a quiet month.
	err := tx.QueryRow(ctx, `select coalesce(sum(e.amount) filter (where e.amount > 0 and t.kind in ('topup', 'transfer')), 0),
		coalesce(-sum(e.amount) filter (where e.amount < 0 and t.kind = 'transfer'), 0),
		coalesce(-sum(e.amount) filter (where t.kind in ('goal_in', 'goal_out')), 0),
		count(e.id)
		from entries e join txns t on t.id = e.txn_id
		where e.account_id = $1 and t.created_at >= $2 and t.created_at < $3`, wallet, from, to).Scan(&d.In, &d.Out, &d.SavedGoals, &d.Txns)
	if err != nil {
		return nil, err
	}
	if err := tx.QueryRow(ctx, `select coalesce((select e.balance_after from entries e join txns t on t.id = e.txn_id
		where e.account_id = $1 and t.created_at < $2 order by e.id desc limit 1), 0)`, wallet, to).Scan(&d.Closing); err != nil {
		return nil, err
	}
	spent, err := spentByCategory(ctx, tx, user, from, to)
	if err != nil {
		return nil, err
	}
	for c, v := range spent {
		d.Spending = append(d.Spending, Amount{c, v})
	}
	sort.Slice(d.Spending, func(i, j int) bool {
		return d.Spending[i].Amount > d.Spending[j].Amount || d.Spending[i].Amount == d.Spending[j].Amount && d.Spending[i].Name < d.Spending[j].Name
	})
	rows, err := tx.Query(ctx, `select u.name, sum(t.amount) from txns t join users u on u.id = t.counterparty_id
		where t.user_id = $1 and t.kind = 'transfer' and t.created_at >= $2 and t.created_at < $3 group by u.name order by 2 desc, 1 limit 3`, user, from, to)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var a Amount
		if err := rows.Scan(&a.Name, &a.Amount); err != nil {
			rows.Close()
			return nil, err
		}
		d.TopPayees = append(d.TopPayees, a)
	}
	rows.Close()
	rows, err = tx.Query(ctx, `select category, monthly_limit from budgets where user_id = $1 order by category`, user)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var b BudgetResult
		if err := rows.Scan(&b.Category, &b.Limit); err != nil {
			rows.Close()
			return nil, err
		}
		b.Spent = spent[b.Category]
		d.Budgets = append(d.Budgets, b)
	}
	rows.Close()
	rows, err = tx.Query(ctx, `select a.name, coalesce((select e.balance_after from entries e join txns t on t.id = e.txn_id where e.account_id = a.id and t.created_at < $2 order by e.id desc limit 1), 0)
		from accounts a where a.user_id = $1 and a.kind = 'goal' and a.created_at < $2 and (a.closed_at is null or a.closed_at >= $2) order by a.id`, user, to)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var a Amount
		if err := rows.Scan(&a.Name, &a.Amount); err != nil {
			rows.Close()
			return nil, err
		}
		d.Goals = append(d.Goals, a)
	}
	rows.Close()
	return d, rows.Err()
}

func (s *Store) Reports(ctx context.Context, u *auth.User) ([]Report, error) {
	if err := member(u); err != nil {
		return nil, err
	}
	rows, err := s.DB.Query(ctx, `select to_char(month, 'YYYY-MM'), data, created_at from reports where user_id = $1 order by month desc limit 24`, u.ID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Report{}
	for rows.Next() {
		var r Report
		if err := rows.Scan(&r.Month, &r.Data, &r.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// ---- Statement download ---------------------------------------------------

// safeCell stops a spreadsheet treating a note like "=HYPERLINK(...)" as a
// formula when the statement is opened (CSV injection).
func SafeCell(v string) string { return safeCell(v) }

func safeCell(v string) string {
	if v != "" && strings.ContainsRune("=+-@\t\r", rune(v[0])) {
		return "'" + v
	}
	return v
}

// StatementCSV is the wallet statement for a date range (IST, inclusive).
func (s *Store) StatementCSV(ctx context.Context, u *auth.User, from, to time.Time) ([]byte, error) {
	if err := member(u); err != nil {
		return nil, err
	}
	if to.Before(from) || to.Sub(from) > 366*24*time.Hour {
		return nil, problem("to", "choose a range of up to a year")
	}
	rows, err := s.DB.Query(ctx, activityQuery+` and t.created_at >= $2 and t.created_at < $3 order by t.id`, u.ID, from, to.AddDate(0, 0, 1))
	if err != nil {
		return nil, err
	}
	acts, err := scanActivity(rows)
	if err != nil {
		return nil, err
	}
	var b bytes.Buffer
	w := csv.NewWriter(&b)
	w.Write([]string{"Kaasu wallet statement (simulated money, not a real account)"})
	w.Write([]string{"Name", safeCell(u.Name), "UPI ID", u.UPI})
	w.Write([]string{"Period", from.Format("02/01/2006") + " to " + to.Format("02/01/2006")})
	w.Write(nil)
	w.Write([]string{"Date", "Time", "Description", "Category", "Money out", "Money in", "Balance", "Reference"})
	plain := func(p int64) string { return strings.TrimPrefix(money.Format(p), "₹") }
	var in, out int64
	for _, a := range acts {
		desc := map[string]string{"topup": "Added from bank (UPI, simulated)", "goal_in": "Moved to " + a.With, "goal_out": "Moved back from " + a.With}[a.Kind]
		if a.Kind == "transfer" {
			desc = "Paid " + a.With
			if a.Amount > 0 {
				desc = "Received from " + a.With
			}
		}
		if a.Note != "" {
			desc += ": " + a.Note
		}
		o, i := "", ""
		if a.Amount < 0 {
			o, out = plain(-a.Amount), out-a.Amount
		} else {
			i, in = plain(a.Amount), in+a.Amount
		}
		t := a.At.In(IST)
		w.Write([]string{t.Format("02/01/2006"), t.Format("15:04"), safeCell(desc), a.Category, o, i, plain(a.Balance), fmt.Sprintf("KAASU%08d", a.ID)})
	}
	w.Write(nil)
	w.Write([]string{"Total", "", "", "", plain(out), plain(in)})
	w.Flush()
	return b.Bytes(), w.Error()
}

// ---- Ops ----------------------------------------------------------------

type PausedAccount struct {
	ID       int64      `json:"id"`
	Name     string     `json:"name"`
	UPI      string     `json:"upi_id"`
	PausedAt time.Time  `json:"paused_at"`
	Reason   string     `json:"reason"`
	Recent   []Activity `json:"recent"`
}

func ops(u *auth.User) error {
	if u.Role != "ops" {
		return ErrForbidden
	}
	return nil
}

func (s *Store) Paused(ctx context.Context, u *auth.User) ([]PausedAccount, error) {
	if err := ops(u); err != nil {
		return nil, err
	}
	rows, err := s.DB.Query(ctx, `select id, name, coalesce(upi_id, ''), paused_at, coalesce(paused_reason, '') from users where paused_at is not null order by paused_at`)
	if err != nil {
		return nil, err
	}
	var out []PausedAccount
	for rows.Next() {
		var p PausedAccount
		if err := rows.Scan(&p.ID, &p.Name, &p.UPI, &p.PausedAt, &p.Reason); err != nil {
			rows.Close()
			return nil, err
		}
		out = append(out, p)
	}
	rows.Close()
	for i := range out {
		r, err := s.DB.Query(ctx, activityQuery+` and t.created_at > $2 order by t.id desc limit 10`, out[i].ID, out[i].PausedAt.Add(-time.Hour))
		if err != nil {
			return nil, err
		}
		if out[i].Recent, err = scanActivity(r); err != nil {
			return nil, err
		}
	}
	if out == nil {
		out = []PausedAccount{}
	}
	return out, nil
}

type Unpause struct {
	Note string `json:"note"`
}

// Unpause clears a paused account after a person has checked it. The note
// is required and goes into the audit log.
func (s *Store) Unpause(ctx context.Context, u *auth.User, id int64, in Unpause, ip string) error {
	if err := ops(u); err != nil {
		return err
	}
	in.Note = strings.TrimSpace(in.Note)
	if len(in.Note) < 10 {
		return problem("note", "write what you checked (at least 10 characters)")
	}
	return s.tx(ctx, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `update users set paused_at = null, paused_reason = null, reviewed_at = $2 where id = $1 and paused_at is not null`, id, s.Now())
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return ErrNotFound
		}
		if err := audit(ctx, tx, id, u.ID, "unpaused", ip, map[string]any{"note": in.Note}); err != nil {
			return err
		}
		return notify(ctx, tx, userTopic(id), "ops")
	})
}

type AuditEntry struct {
	ID     int64          `json:"id"`
	At     time.Time      `json:"at"`
	User   string         `json:"user"`
	Actor  string         `json:"actor"`
	Action string         `json:"action"`
	Detail map[string]any `json:"detail"`
	IP     string         `json:"ip"`
}

// Audit: ops see everyone's entries; a member sees their own.
func (s *Store) Audit(ctx context.Context, u *auth.User, limit int) ([]AuditEntry, error) {
	if limit <= 0 || limit > 200 {
		limit = 100
	}
	q := `select l.id, l.at, coalesce(a.name, ''), coalesce(b.name, ''), l.action, l.detail, l.ip from audit_log l
		left join users a on a.id = l.user_id left join users b on b.id = l.actor_id`
	args := []any{limit}
	if u.Role != "ops" {
		q += ` where l.user_id = $2`
		args = append(args, u.ID)
	}
	rows, err := s.DB.Query(ctx, q+` order by l.id desc limit $1`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []AuditEntry{}
	for rows.Next() {
		var e AuditEntry
		if err := rows.Scan(&e.ID, &e.At, &e.User, &e.Actor, &e.Action, &e.Detail, &e.IP); err != nil {
			return nil, err
		}
		if u.Role != "ops" {
			e.IP = ""
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

type LedgerCheck struct {
	Txns          int   `json:"txns"`
	Entries       int   `json:"entries"`
	Accounts      int   `json:"accounts"`
	Unbalanced    int   `json:"unbalanced_txns"`   // txns whose entries don't sum to zero
	WrongBalances int   `json:"wrong_balances"`    // accounts whose balance isn't the sum of their entries
	BrokenRunning int   `json:"broken_running"`    // entries whose balance_after doesn't follow from the previous one
	Negative      int   `json:"negative_balances"` // non-bank accounts below zero
	Total         int64 `json:"total"`             // all balances added: must be zero
	OK            bool  `json:"ok"`
}

// CheckLedger recomputes every balance from the entries.
func (s *Store) CheckLedger(ctx context.Context, u *auth.User) (*LedgerCheck, error) {
	if u != nil {
		if err := ops(u); err != nil {
			return nil, err
		}
	}
	var c LedgerCheck
	err := s.DB.QueryRow(ctx, `select
		(select count(*) from txns), (select count(*) from entries), (select count(*) from accounts),
		(select count(*) from (select txn_id from entries group by txn_id having sum(amount) <> 0) x),
		(select count(*) from accounts a where a.balance <> coalesce((select sum(amount) from entries e where e.account_id = a.id), 0)),
		(select count(*) from (select balance_after - amount - coalesce(lag(balance_after) over (partition by account_id order by id), 0) as gap from entries) x where gap <> 0),
		(select count(*) from accounts where kind <> 'bank' and balance < 0),
		(select coalesce(sum(balance), 0) from accounts)`).
		Scan(&c.Txns, &c.Entries, &c.Accounts, &c.Unbalanced, &c.WrongBalances, &c.BrokenRunning, &c.Negative, &c.Total)
	if err != nil {
		return nil, err
	}
	c.OK = c.Unbalanced == 0 && c.WrongBalances == 0 && c.BrokenRunning == 0 && c.Negative == 0 && c.Total == 0
	return &c, nil
}
