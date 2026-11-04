package store

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/Aravindh97/kaasu/internal/auth"
)

// ---- Budgets ------------------------------------------------------------

type Budget struct {
	Category string `json:"category"`
	Limit    int64  `json:"limit"`
	Spent    int64  `json:"spent"` // this month: wallet payments plus imported statement rows
	Left     int64  `json:"left"`
	Percent  int    `json:"percent"`
}

type Budgets struct {
	Month      string   `json:"month"` // YYYY-MM
	DaysLeft   int      `json:"days_left"`
	Budgets    []Budget `json:"budgets"`
	Categories []string `json:"categories"`
}

func monthStart(t time.Time) time.Time {
	t = t.In(IST)
	return time.Date(t.Year(), t.Month(), 1, 0, 0, 0, 0, IST)
}

// spentByCategory adds wallet payments and imported bank-statement rows
// for one month. Statement rows count only when the import was analysed
// on the server; device-mode imports keep their rows in the browser.
func spentByCategory(ctx context.Context, q interface {
	Query(context.Context, string, ...any) (pgx.Rows, error)
}, user int64, from, to time.Time) (map[string]int64, error) {
	rows, err := q.Query(ctx, `select category, sum(amount) from (
		select t.category, t.amount from txns t where t.user_id = $1 and t.kind = 'transfer' and t.created_at >= $2 and t.created_at < $3
		union all
		select r.category, -r.amount from statement_rows r where r.user_id = $1 and r.amount < 0 and r.day >= $2::date and r.day < $3::date
	) x group by category`, user, from, to)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]int64{}
	for rows.Next() {
		var c string
		var v int64
		if err := rows.Scan(&c, &v); err != nil {
			return nil, err
		}
		out[c] = v
	}
	return out, rows.Err()
}

func (s *Store) Budgets(ctx context.Context, u *auth.User) (*Budgets, error) {
	if err := member(u); err != nil {
		return nil, err
	}
	from := monthStart(s.Now())
	to := from.AddDate(0, 1, 0)
	spent, err := spentByCategory(ctx, s.DB, u.ID, from, to)
	if err != nil {
		return nil, err
	}
	rows, err := s.DB.Query(ctx, `select category, monthly_limit from budgets where user_id = $1 order by category`, u.ID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := &Budgets{Month: from.Format("2006-01"), DaysLeft: int(to.Sub(s.Now()).Hours()/24) + 1, Budgets: []Budget{}, Categories: BudgetCategories()}
	for rows.Next() {
		var b Budget
		if err := rows.Scan(&b.Category, &b.Limit); err != nil {
			return nil, err
		}
		b.Spent = spent[b.Category]
		b.Left = b.Limit - b.Spent
		b.Percent = int(b.Spent * 100 / b.Limit)
		out.Budgets = append(out.Budgets, b)
	}
	return out, rows.Err()
}

func BudgetCategories() []string {
	return []string{"Food & dining", "Groceries", "Shopping", "Bills & recharges", "Travel & fuel", "Health", "Education", "Rent", "Subscriptions", "Gifts", "Transfers", "Other"}
}

type SetBudget struct {
	Category string `json:"category"`
	Limit    int64  `json:"limit"` // 0 removes the budget
}

func (s *Store) SetBudget(ctx context.Context, u *auth.User, in SetBudget) error {
	if err := member(u); err != nil {
		return err
	}
	ok := false
	for _, c := range BudgetCategories() {
		ok = ok || c == in.Category
	}
	if !ok {
		return problem("category", "choose a category from the list")
	}
	if in.Limit < 0 || in.Limit > 10_00_000_00 {
		return problem("limit", "enter a monthly amount up to ₹10,00,000")
	}
	return s.tx(ctx, func(tx pgx.Tx) error {
		var err error
		if in.Limit == 0 {
			_, err = tx.Exec(ctx, `delete from budgets where user_id = $1 and category = $2`, u.ID, in.Category)
		} else {
			_, err = tx.Exec(ctx, `insert into budgets (user_id, category, monthly_limit) values ($1, $2, $3)
				on conflict (user_id, category) do update set monthly_limit = excluded.monthly_limit`, u.ID, in.Category, in.Limit)
		}
		if err != nil {
			return err
		}
		return notify(ctx, tx, userTopic(u.ID))
	})
}

// ---- Goals --------------------------------------------------------------
// A goal is its own ledger account. Saving towards it moves money out of
// the wallet, so it can't be spent by accident; taking it back moves it in.

type Goal struct {
	ID         int64      `json:"id"`
	Name       string     `json:"name"`
	Saved      int64      `json:"saved"`
	Target     int64      `json:"target"`
	TargetDate *string    `json:"target_date"`
	PerMonth   int64      `json:"per_month"` // what's still needed each month to make the date
	Percent    int        `json:"percent"`
	CreatedAt  time.Time  `json:"created_at"`
	ClosedAt   *time.Time `json:"closed_at"`
}

type NewGoal struct {
	Name       string `json:"name"`
	Target     int64  `json:"target"`
	TargetDate string `json:"target_date"` // optional, YYYY-MM-DD
}

func (s *Store) Goals(ctx context.Context, u *auth.User) ([]Goal, error) {
	if err := member(u); err != nil {
		return nil, err
	}
	rows, err := s.DB.Query(ctx, `select id, name, balance, target, to_char(target_date, 'YYYY-MM-DD'), created_at, closed_at from accounts
		where user_id = $1 and kind = 'goal' and (closed_at is null or closed_at > $2) order by closed_at nulls first, id`, u.ID, s.Now().AddDate(0, -1, 0))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Goal{}
	for rows.Next() {
		var g Goal
		if err := rows.Scan(&g.ID, &g.Name, &g.Saved, &g.Target, &g.TargetDate, &g.CreatedAt, &g.ClosedAt); err != nil {
			return nil, err
		}
		g.Percent = int(min(100, g.Saved*100/g.Target))
		if g.TargetDate != nil && g.Saved < g.Target {
			d, _ := time.ParseInLocation(time.DateOnly, *g.TargetDate, IST)
			months := monthsBetween(s.Now(), d)
			g.PerMonth = (g.Target - g.Saved + int64(months) - 1) / int64(months) // round up to the paisa
		}
		out = append(out, g)
	}
	return out, rows.Err()
}

// monthsBetween counts the months left, at least 1: saving can happen this month.
func monthsBetween(now, until time.Time) int {
	n := (until.Year()-now.Year())*12 + int(until.Month()) - int(now.Month())
	if until.Day() >= now.Day() {
		n++
	}
	return max(1, n)
}

func (s *Store) CreateGoal(ctx context.Context, u *auth.User, in NewGoal) (*Goal, error) {
	if err := member(u); err != nil {
		return nil, err
	}
	in.Name = strings.TrimSpace(in.Name)
	if in.Name == "" || len(in.Name) > 40 {
		return nil, problem("name", "name the goal (up to 40 characters)")
	}
	if in.Target < 100_00 || in.Target > 1_00_00_000_00 {
		return nil, problem("target", "set a target between ₹100 and ₹1 crore")
	}
	var date *time.Time
	if in.TargetDate != "" {
		d, err := time.ParseInLocation(time.DateOnly, in.TargetDate, IST)
		if err != nil || !d.After(s.today()) || d.After(s.today().AddDate(30, 0, 0)) {
			return nil, problem("target_date", "pick a date after today")
		}
		date = &d
	}
	var id int64
	err := s.tx(ctx, func(tx pgx.Tx) error {
		var n int
		if err := tx.QueryRow(ctx, `select count(*) from accounts where user_id = $1 and kind = 'goal' and closed_at is null`, u.ID).Scan(&n); err != nil {
			return err
		}
		if n >= 10 {
			return problem("name", "you can have 10 open goals")
		}
		if err := tx.QueryRow(ctx, `insert into accounts (user_id, kind, name, target, target_date, created_at) values ($1, 'goal', $2, $3, $4, $5) returning id`,
			u.ID, in.Name, in.Target, date, s.Now()).Scan(&id); err != nil {
			return err
		}
		return notify(ctx, tx, userTopic(u.ID))
	})
	if err != nil {
		return nil, err
	}
	gs, err := s.Goals(ctx, u)
	if err != nil {
		return nil, err
	}
	for _, g := range gs {
		if g.ID == id {
			return &g, nil
		}
	}
	return nil, ErrNotFound
}

type GoalMove struct {
	Amount int64 `json:"amount"` // + saves into the goal, - takes back to the wallet
}

func (s *Store) MoveGoal(ctx context.Context, u *auth.User, r Request, goal int64, in GoalMove) (*Moved, error) {
	if err := member(u); err != nil {
		return nil, err
	}
	if in.Amount == 0 {
		return nil, problem("amount", "enter an amount")
	}
	raw, replay, err := s.once(ctx, u, r, map[string]int64{"goal": goal, "amount": in.Amount}, func(tx pgx.Tx) (any, error) {
		var owner int64
		err := tx.QueryRow(ctx, `select user_id from accounts where id = $1 and kind = 'goal'`, goal).Scan(&owner)
		if errors.Is(err, pgx.ErrNoRows) || err == nil && owner != u.ID {
			return nil, ErrNotFound
		}
		if err != nil {
			return nil, err
		}
		w, err := s.wallet(ctx, tx, u.ID)
		if err != nil {
			return nil, err
		}
		spec := moveSpec{kind: "goal_in", user: u.ID, from: w, to: goal, amount: in.Amount, note: "Saved towards goal", category: "Savings", at: s.Now()}
		if in.Amount < 0 {
			spec = moveSpec{kind: "goal_out", user: u.ID, from: goal, to: w, amount: -in.Amount, note: "Taken back from goal", category: "Savings", at: s.Now()}
		}
		id, err := move(ctx, tx, spec)
		if err != nil {
			return nil, err
		}
		var bal int64
		if err := tx.QueryRow(ctx, `select balance from accounts where id = $1`, w).Scan(&bal); err != nil {
			return nil, err
		}
		return &Moved{TxnID: id, Balance: bal}, notify(ctx, tx, userTopic(u.ID))
	})
	return decodeMoved(raw, replay, err)
}

// CloseGoal closes an empty goal. Money must be taken back first, so
// closing can never lose any.
func (s *Store) CloseGoal(ctx context.Context, u *auth.User, goal int64) error {
	return s.tx(ctx, func(tx pgx.Tx) error {
		var bal int64
		err := tx.QueryRow(ctx, `select balance from accounts where id = $1 and user_id = $2 and kind = 'goal' and closed_at is null for update`, goal, u.ID).Scan(&bal)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		if bal != 0 {
			return problem("", "move the %s saved back to your wallet first", fmtMoney(bal))
		}
		if _, err := tx.Exec(ctx, `update accounts set closed_at = $2 where id = $1`, goal, s.Now()); err != nil {
			return err
		}
		return notify(ctx, tx, userTopic(u.ID))
	})
}
