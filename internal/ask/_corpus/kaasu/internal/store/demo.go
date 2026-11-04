package store

import (
	"context"
	"math/rand/v2"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"golang.org/x/crypto/bcrypt"

	"github.com/Aravindh97/kaasu/internal/analyse"
	"github.com/Aravindh97/kaasu/internal/auth"
)

type DemoUser struct {
	Email string `json:"email"`
	Name  string `json:"name"`
	Role  string `json:"role"`
	Note  string `json:"note"`
	UPI   string `json:"upi_id"`
	Phone string `json:"phone"`
}

// DemoPIN is the payment PIN of every demo account, shown on the sign-in page.
const DemoPIN = "2580"

var DemoUsers = []DemoUser{
	{"anbu@kaasu.demo", "Anbu Selvan", "member", "Has a sample bank statement analysed, two savings goals and budgets", "anbu@kaasu", "9800000001"},
	{"kavya@kaasu.demo", "Kavya Raman", "member", "Asked Anbu for ₹450; try paying her from Anbu's account", "kavya@kaasu", "9800000002"},
	{"ravi@kaasu.demo", "Ravi Kumar", "member", "Split a dinner bill with Anbu and Kavya", "ravi@kaasu", "9800000003"},
	{"meena@kaasu.demo", "Meena Sundaram", "member", "New to Kaasu. Pay five different people within 10 minutes to see the fraud rule pause her account", "meena@kaasu", "9800000004"},
	{"selva@kaasu.demo", "Selva (Kaasu ops)", "ops", "Reviews paused accounts, the audit log and the ledger check", "", ""},
}

// Payees: accounts you can pay (arun@kaasu and so on) but not sign in as.
var payees = []struct{ name, upi, phone string }{
	{"Arun Prakash", "arun@kaasu", "9800000011"}, {"Divya Mohan", "divya@kaasu", "9800000012"},
	{"Siva Shankar", "siva@kaasu", "9800000013"}, {"Nila Raj", "nila@kaasu", "9800000014"},
}

// ResetDemo replaces everything with the demo data. TRUNCATE is used
// because the ledger and audit tables reject DELETE by design.
func (s *Store) ResetDemo(ctx context.Context, passwordCost int) error {
	now := s.Now()
	r := rand.New(rand.NewPCG(uint64(now.YearDay()), 3))
	pin, err := bcrypt.GenerateFromPassword([]byte(DemoPIN), s.PINCost)
	if err != nil {
		return err
	}
	pw, err := auth.HashPassword(auth.RandomPassword(), passwordCost) // demo accounts sign in with one click only
	if err != nil {
		return err
	}
	err = s.tx(ctx, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `truncate users, sessions, accounts, txns, entries, idempotency, payees, splits, money_requests, budgets, imports, statement_rows, category_rules, reports, audit_log restart identity cascade`); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `insert into accounts (kind, name) values ('bank', 'Banks (simulated UPI)')`); err != nil {
			return err
		}
		start := now.AddDate(0, -4, 0)
		ids := map[string]int64{}
		wallets := map[string]int64{}
		for _, d := range DemoUsers {
			var id int64
			var phone, upi, ph any = nil, nil, nil
			if d.Role == "member" {
				phone, upi, ph = d.Phone, d.UPI, string(pin)
			}
			if err := tx.QueryRow(ctx, `insert into users (email, name, role, phone, upi_id, password_hash, pin_hash, demo, created_at) values ($1, $2, $3, $4, $5, $6, $7, true, $8) returning id`,
				d.Email, d.Name, d.Role, phone, upi, pw, ph, start).Scan(&id); err != nil {
				return err
			}
			key := strings.SplitN(d.Email, "@", 2)[0]
			ids[key] = id
			if d.Role == "member" {
				var w int64
				if err := tx.QueryRow(ctx, `insert into accounts (user_id, kind, name, created_at) values ($1, 'wallet', 'Wallet', $2) returning id`, id, start).Scan(&w); err != nil {
					return err
				}
				wallets[key] = w
			}
		}
		for _, p := range payees {
			var id int64
			if err := tx.QueryRow(ctx, `insert into users (email, name, phone, upi_id, password_hash, created_at) values ($1, $2, $3, $4, $5, $6) returning id`,
				strings.SplitN(p.upi, "@", 2)[0]+"@payee.kaasu.demo", p.name, p.phone, p.upi, pw, start).Scan(&id); err != nil {
				return err
			}
			if _, err := tx.Exec(ctx, `insert into accounts (user_id, kind, name, created_at) values ($1, 'wallet', 'Wallet', $2)`, id, start); err != nil {
				return err
			}
		}
		bank, err := bankAccount(ctx, tx)
		if err != nil {
			return err
		}
		// Everything is applied in time order, so ids follow dates the way
		// they do in real use.
		type event struct {
			at time.Time
			do func(at time.Time) error
		}
		var events []event
		at := func(t time.Time, f func(time.Time) error) { events = append(events, event{t, f}) }
		balance := func(acct int64) (b int64, err error) {
			err = tx.QueryRow(ctx, `select balance from accounts where id = $1`, acct).Scan(&b)
			return
		}
		topup := func(who string, amount int64, t time.Time) error {
			_, err := move(ctx, tx, moveSpec{kind: "topup", user: ids[who], from: bank, to: wallets[who], amount: amount, note: "UPI top-up (simulated)", category: "Top-up", at: t})
			return err
		}
		// Four months of everyday payments between the three friends. Meena
		// stays new, so trying the fraud rule from her account is easy.
		friends := []string{"anbu", "kavya", "ravi"}
		notes := []struct{ note, cat string }{{"Auto share", "Travel & fuel"}, {"Lunch", "Food & dining"}, {"Movie tickets", "Other"}, {"Groceries", "Groceries"},
			{"Petrol", "Travel & fuel"}, {"Birthday gift", "Gifts"}, {"Coffee", "Food & dining"}, {"Book", "Education"}, {"Electricity share", "Bills & recharges"}}
		for _, f := range friends {
			amt := int64(15000+r.IntN(10000)) * 100
			at(start.Add(time.Hour), func(t time.Time) error { return topup(f, amt, t) })
		}
		for d := start.AddDate(0, 0, 1); d.Before(now.Add(-2 * time.Hour)); d = d.Add(time.Duration(5+r.IntN(14)) * time.Hour) {
			a := friends[r.IntN(3)]
			b := friends[(r.IntN(2)+1+indexOf(friends, a))%3]
			n := notes[r.IntN(len(notes))]
			amt := int64(50+r.IntN(1200)) * 100
			at(d, func(t time.Time) error {
				if bal, err := balance(wallets[a]); err != nil {
					return err
				} else if bal < amt+5_000_00 { // running low: top up first, as people do
					if err := topup(a, 10_000_00, t.Add(-time.Minute)); err != nil {
						return err
					}
				}
				to := ids[b]
				if _, err := move(ctx, tx, moveSpec{kind: "transfer", user: ids[a], counterparty: &to, from: wallets[a], to: wallets[b], amount: amt, note: n.note, category: n.cat, at: t}); err != nil {
					return err
				}
				_, err := tx.Exec(ctx, `insert into payees (user_id, payee_id, first_paid_at) values ($1, $2, $3) on conflict do nothing`, ids[a], to, t)
				return err
			})
		}
		at(now.Add(-time.Hour), func(t time.Time) error { return topup("meena", 10_000_00, t) })
		// Anbu's goals, saved into from the wallet in two steps each
		for _, g := range []struct {
			name         string
			target, save int64
			months       int
		}{{"Ooty trip in the holidays", 30_000_00, 12_000_00, 2}, {"Emergency fund", 1_00_000_00, 25_000_00, 10}} {
			var gid int64
			if err := tx.QueryRow(ctx, `insert into accounts (user_id, kind, name, target, target_date, created_at) values ($1, 'goal', $2, $3, $4, $5) returning id`,
				ids["anbu"], g.name, g.target, now.AddDate(0, g.months, 0).Format(time.DateOnly), start.AddDate(0, 0, 10)).Scan(&gid); err != nil {
				return err
			}
			for i, part := range []int64{g.save / 2, g.save - g.save/2} {
				at(start.AddDate(0, 0, 12+30*i), func(t time.Time) error {
					if err := topup("anbu", part, t.Add(-time.Minute)); err != nil {
						return err
					}
					_, err := move(ctx, tx, moveSpec{kind: "goal_in", user: ids["anbu"], from: wallets["anbu"], to: gid, amount: part, note: "Saved towards goal", category: "Savings", at: t})
					return err
				})
			}
		}
		sort.SliceStable(events, func(i, j int) bool { return events[i].at.Before(events[j].at) })
		for _, e := range events {
			if err := e.do(e.at); err != nil {
				return err
			}
		}
		for c, l := range map[string]int64{"Food & dining": 6_000_00, "Shopping": 5_000_00, "Travel & fuel": 4_000_00, "Bills & recharges": 3_000_00} {
			if _, err := tx.Exec(ctx, `insert into budgets (user_id, category, monthly_limit) values ($1, $2, $3)`, ids["anbu"], c, l); err != nil {
				return err
			}
		}
		// open requests: Kavya asks Anbu; Ravi splits a dinner
		if _, err := tx.Exec(ctx, `insert into money_requests (requester_id, payer_id, amount, note, created_at) values ($1, $2, 45000, 'Auto to Gandhipuram', $3)`, ids["kavya"], ids["anbu"], now.Add(-3*time.Hour)); err != nil {
			return err
		}
		var split int64
		if err := tx.QueryRow(ctx, `insert into splits (created_by, title, total, created_at) values ($1, 'Dinner at Annapoorna', 186000, $2) returning id`, ids["ravi"], now.Add(-26*time.Hour)).Scan(&split); err != nil {
			return err
		}
		for _, p := range []string{"anbu", "kavya"} {
			if _, err := tx.Exec(ctx, `insert into money_requests (requester_id, payer_id, amount, note, split_id, created_at) values ($1, $2, 62000, 'Dinner at Annapoorna', $3, $4)`, ids["ravi"], ids[p], split, now.Add(-26*time.Hour)); err != nil {
				return err
			}
		}
		return audit(ctx, tx, 0, 0, "demo_reset", "", nil)
	})
	if err != nil {
		return err
	}
	// Anbu's sample statement, analysed on the server, and month-end reports
	anbu := &auth.User{ID: 1, Role: "member"}
	if err := s.DB.QueryRow(ctx, `select id from users where email = 'anbu@kaasu.demo'`).Scan(&anbu.ID); err != nil {
		return err
	}
	if _, err := s.ImportOnServer(ctx, anbu, "sample-bank-statement.csv", []byte(analyse.SampleCSV(uint64(now.YearDay()), now.In(IST)))); err != nil {
		return err
	}
	for m := 3; m >= 1; m-- {
		if _, err := s.RunMonthEnd(ctx, monthStart(now).AddDate(0, -m, 0)); err != nil {
			return err
		}
	}
	return nil
}

func indexOf(xs []string, x string) int {
	for i, v := range xs {
		if v == x {
			return i
		}
	}
	return -1
}
