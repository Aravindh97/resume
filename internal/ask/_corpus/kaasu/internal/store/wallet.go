package store

import (
	"context"
	"encoding/json"
	"errors"
	"regexp"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"golang.org/x/crypto/bcrypt"

	"github.com/Aravindh97/kaasu/internal/auth"
	"github.com/Aravindh97/kaasu/internal/money"
)

// Limits. Demo rules, set to look like a real wallet's; not any regulator's.
const (
	DailySendLimit  = 25_000_00   // ₹25,000 sent per day (IST)
	DailyTopUpLimit = 50_000_00   // ₹50,000 added per day
	WalletCap       = 1_00_000_00 // ₹1,00,000 most a wallet holds
	PINTries        = 3           // wrong PINs before the PIN locks
	PINLock         = 30 * time.Minute
	FraudPayees     = 5                // transfers to new payees...
	FraudWindow     = 10 * time.Minute // ...within this long pauses the account
	NewPayeeAge     = 24 * time.Hour   // a payee first paid this recently is new
	PINCost         = 8                // bcrypt cost for PINs; see api.New
)

func fmtMoney(p int64) string { return money.Format(p) }

var pinRE = regexp.MustCompile(`^[0-9]{4}$`)

// CheckPIN verifies the payment PIN in its own transaction, so a wrong
// attempt is counted even though the payment it guarded doesn't happen.
// Three wrong PINs lock it for 30 minutes.
func (s *Store) CheckPIN(ctx context.Context, u *auth.User, pin, ip string) error {
	if !pinRE.MatchString(pin) {
		return problem("pin", "enter your 4-digit PIN")
	}
	var bad error
	err := s.tx(ctx, func(tx pgx.Tx) error {
		var h *string
		var fails int
		var until *time.Time
		if err := tx.QueryRow(ctx, `select pin_hash, pin_failures, pin_locked_until from users where id = $1 for no key update`, u.ID).Scan(&h, &fails, &until); err != nil {
			return err
		}
		now := s.Now()
		if until != nil && now.Before(*until) {
			bad = locked("pin", "too many wrong PINs; try again after %s", until.In(IST).Format("15:04"))
			return nil
		}
		if h == nil {
			bad = problem("pin", "set a PIN first")
			return nil
		}
		if bcrypt.CompareHashAndPassword([]byte(*h), []byte(pin)) == nil {
			_, err := tx.Exec(ctx, `update users set pin_failures = 0, pin_locked_until = null where id = $1`, u.ID)
			return err
		}
		fails++
		if fails >= PINTries {
			t := now.Add(PINLock)
			bad = locked("pin", "wrong PIN %d times; payments are locked until %s", fails, t.In(IST).Format("15:04"))
			if _, err := tx.Exec(ctx, `update users set pin_failures = 0, pin_locked_until = $2 where id = $1`, u.ID, t); err != nil {
				return err
			}
			return audit(ctx, tx, u.ID, u.ID, "pin_locked", ip, nil)
		}
		bad = problem("pin", "wrong PIN; %d tries left", PINTries-fails)
		if _, err := tx.Exec(ctx, `update users set pin_failures = $2 where id = $1`, u.ID, fails); err != nil {
			return err
		}
		return audit(ctx, tx, u.ID, u.ID, "pin_wrong", ip, nil)
	})
	if err != nil {
		return err
	}
	return bad
}

// SetPIN sets or changes the PIN; changing it needs the account password
// (checked by the caller) and is audited.
func (s *Store) SetPIN(ctx context.Context, u *auth.User, pin, ip string) error {
	if !pinRE.MatchString(pin) {
		return problem("pin", "a PIN is 4 digits")
	}
	if strings.Count(pin, pin[:1]) == 4 || pin == "1234" || pin == "4321" || pin == "0000" {
		return problem("pin", "choose a PIN that's harder to guess")
	}
	h, err := bcrypt.GenerateFromPassword([]byte(pin), s.PINCost)
	if err != nil {
		return err
	}
	return s.tx(ctx, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `update users set pin_hash = $2, pin_failures = 0, pin_locked_until = null where id = $1`, u.ID, string(h)); err != nil {
			return err
		}
		return audit(ctx, tx, u.ID, u.ID, "pin_set", ip, nil)
	})
}

// ---- Top-up -------------------------------------------------------------

type TopUp struct {
	Amount int64 `json:"amount"` // paise
}

type Moved struct {
	TxnID   int64 `json:"txn_id"`
	Balance int64 `json:"balance"`
	Replay  bool  `json:"replay"` // this was a retry; nothing moved twice
}

// AddMoney simulates a UPI top-up from the user's bank: no real money moves.
func (s *Store) AddMoney(ctx context.Context, u *auth.User, r Request, in TopUp) (*Moved, error) {
	if err := member(u); err != nil {
		return nil, err
	}
	if in.Amount < 100 || in.Amount > DailyTopUpLimit {
		return nil, problem("amount", "add between ₹1 and %s", fmtMoney(DailyTopUpLimit))
	}
	raw, replay, err := s.once(ctx, u, r, in, func(tx pgx.Tx) (any, error) {
		if _, err := tx.Exec(ctx, `select 1 from users where id = $1 for no key update`, u.ID); err != nil {
			return nil, err
		}
		var added int64
		if err := tx.QueryRow(ctx, `select coalesce(sum(amount), 0) from txns where user_id = $1 and kind = 'topup' and created_at >= $2`, u.ID, s.today()).Scan(&added); err != nil {
			return nil, err
		}
		if added+in.Amount > DailyTopUpLimit {
			return nil, problem("amount", "you can add %s a day; %s left today", fmtMoney(DailyTopUpLimit), fmtMoney(DailyTopUpLimit-added))
		}
		w, err := s.wallet(ctx, tx, u.ID)
		if err != nil {
			return nil, err
		}
		var bal int64
		if err := tx.QueryRow(ctx, `select balance from accounts where id = $1`, w).Scan(&bal); err != nil {
			return nil, err
		}
		if bal+in.Amount > WalletCap {
			return nil, problem("amount", "a wallet holds at most %s; you can add %s", fmtMoney(WalletCap), fmtMoney(max(0, WalletCap-bal)))
		}
		bank, err := bankAccount(ctx, tx)
		if err != nil {
			return nil, err
		}
		id, err := move(ctx, tx, moveSpec{kind: "topup", user: u.ID, from: bank, to: w, amount: in.Amount, note: "UPI top-up (simulated)", category: "Top-up", at: s.Now()})
		if err != nil {
			return nil, err
		}
		if err := audit(ctx, tx, u.ID, u.ID, "topup", r.IP, map[string]any{"amount": in.Amount, "txn": id}); err != nil {
			return nil, err
		}
		return &Moved{TxnID: id, Balance: bal + in.Amount}, notify(ctx, tx, userTopic(u.ID))
	})
	return decodeMoved(raw, replay, err)
}

func decodeMoved(raw []byte, replay bool, err error) (*Moved, error) {
	if err != nil {
		return nil, err
	}
	var m Moved
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, err
	}
	m.Replay = replay
	return &m, nil
}

// ---- Sending ------------------------------------------------------------

type Payee struct {
	ID    int64  `json:"id"`
	Name  string `json:"name"`
	UPI   string `json:"upi_id"`
	Phone string `json:"phone"` // masked: 98XXXXXX01
	New   bool   `json:"new"`   // never paid, or first paid in the last day
}

func mask(phone string) string {
	if len(phone) != 10 {
		return ""
	}
	return phone[:2] + "XXXXXX" + phone[8:]
}

var phoneRE = regexp.MustCompile(`^[6-9][0-9]{9}$`)

// Lookup finds a payee by mobile number or UPI ID, the way UPI apps show
// the registered name before you pay.
func (s *Store) Lookup(ctx context.Context, u *auth.User, to string) (*Payee, error) {
	if err := member(u); err != nil {
		return nil, err
	}
	to = strings.ToLower(strings.TrimSpace(strings.TrimPrefix(strings.ReplaceAll(to, " ", ""), "+91")))
	var p Payee
	var phone *string
	var first *time.Time
	q := `select u.id, u.name, coalesce(u.upi_id, ''), u.phone, p.first_paid_at from users u
		left join payees p on p.user_id = $2 and p.payee_id = u.id
		where u.active and u.role = 'member' and `
	switch {
	case phoneRE.MatchString(to):
		q += `u.phone = $1`
	case strings.Contains(to, "@"):
		q += `u.upi_id = $1`
	default:
		return nil, problem("to", "enter a 10-digit mobile number or a UPI ID like name@kaasu")
	}
	err := s.DB.QueryRow(ctx, q, to, u.ID).Scan(&p.ID, &p.Name, &p.UPI, &phone, &first)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, problem("to", "no Kaasu account with that number or UPI ID")
	}
	if err != nil {
		return nil, err
	}
	if p.ID == u.ID {
		return nil, problem("to", "that's you")
	}
	if phone != nil {
		p.Phone = mask(*phone)
	}
	p.New = first == nil || s.Now().Sub(*first) < NewPayeeAge
	return &p, nil
}

type Send struct {
	To       string `json:"to"`     // mobile number or UPI ID
	Amount   int64  `json:"amount"` // paise
	Note     string `json:"note"`
	Category string `json:"category"`
	PIN      string `json:"pin"`
}

var sendCategories = map[string]bool{"Transfers": true, "Rent": true, "Food & dining": true, "Groceries": true, "Shopping": true, "Bills & recharges": true,
	"Travel & fuel": true, "Health": true, "Education": true, "Gifts": true, "Other": true}

// SendCategories are the categories a payment can be tagged with.
func SendCategories() []string {
	return []string{"Transfers", "Rent", "Food & dining", "Groceries", "Shopping", "Bills & recharges", "Travel & fuel", "Health", "Education", "Gifts", "Other"}
}

// SendMoney moves money to another Kaasu user, exactly once per key.
func (s *Store) SendMoney(ctx context.Context, u *auth.User, r Request, in Send) (*Moved, error) {
	if err := member(u); err != nil {
		return nil, err
	}
	in.Note = strings.TrimSpace(in.Note)
	if in.Category == "" {
		in.Category = "Transfers"
	}
	switch {
	case in.Amount < 100:
		return nil, problem("amount", "send at least ₹1")
	case in.Amount > DailySendLimit:
		return nil, problem("amount", "you can send up to %s a day", fmtMoney(DailySendLimit))
	case len(in.Note) > 80:
		return nil, problem("note", "keep the note under 80 characters")
	case !sendCategories[in.Category]:
		return nil, problem("category", "choose a category from the list")
	}
	p, err := s.Lookup(ctx, u, in.To)
	if err != nil {
		return nil, err
	}
	if err := s.CheckPIN(ctx, u, in.PIN, r.IP); err != nil {
		return nil, err
	}
	body := in
	body.PIN = "" // never hashed or stored: a 4-digit PIN is easy to brute-force from a hash
	raw, replay, err := s.once(ctx, u, r, body, func(tx pgx.Tx) (any, error) {
		return s.send(ctx, tx, u, p.ID, in.Amount, in.Note, in.Category, nil, r.IP)
	})
	return decodeMoved(raw, replay, err)
}

// send runs inside a once() transaction. It locks the sender's user row
// first, so one person's payments are checked one at a time against the
// daily limit and the fraud rule.
func (s *Store) send(ctx context.Context, tx pgx.Tx, u *auth.User, to, amount int64, note, category string, requestID *int64, ip string) (*Moved, error) {
	var paused, reviewed *time.Time
	if err := tx.QueryRow(ctx, `select paused_at, reviewed_at from users where id = $1 for no key update`, u.ID).Scan(&paused, &reviewed); err != nil {
		return nil, err
	}
	if paused != nil {
		return nil, locked("", "your account is paused for a security review; payments are off until it's cleared")
	}
	now := s.Now()
	var sent int64
	if err := tx.QueryRow(ctx, `select coalesce(sum(amount), 0) from txns where user_id = $1 and kind = 'transfer' and created_at >= $2`, u.ID, s.today()).Scan(&sent); err != nil {
		return nil, err
	}
	if sent+amount > DailySendLimit {
		return nil, problem("amount", "daily limit is %s; you can send %s more today", fmtMoney(DailySendLimit), fmtMoney(DailySendLimit-sent))
	}
	// The fraud rule: the fifth payment to a new payee within 10 minutes
	// looks like someone emptying the account, so it is refused and the
	// account paused until a person at Kaasu reviews it.
	var first *time.Time
	if err := tx.QueryRow(ctx, `select first_paid_at from payees where user_id = $1 and payee_id = $2`, u.ID, to).Scan(&first); err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return nil, err
	}
	if first == nil || now.Sub(*first) < NewPayeeAge {
		// Payments made before a person at Kaasu last reviewed the account
		// were looked at already, so they don't count again.
		since := now.Add(-FraudWindow)
		if reviewed != nil && reviewed.After(since) {
			since = *reviewed
		}
		var recent int
		if err := tx.QueryRow(ctx, `select count(*) from txns t join payees p on p.user_id = t.user_id and p.payee_id = t.counterparty_id
			where t.user_id = $1 and t.kind = 'transfer' and t.created_at > $2 and p.first_paid_at > $3`,
			u.ID, since, now.Add(-NewPayeeAge)).Scan(&recent); err != nil {
			return nil, err
		}
		if recent+1 >= FraudPayees {
			reason := "5 payments to new payees within 10 minutes"
			if _, err := tx.Exec(ctx, `update users set paused_at = $2, paused_reason = $3 where id = $1`, u.ID, now, reason); err != nil {
				return nil, err
			}
			if err := audit(ctx, tx, u.ID, u.ID, "paused_by_rule", ip, map[string]any{"reason": reason, "refused_amount": amount, "payee": to}); err != nil {
				return nil, err
			}
			if err := notify(ctx, tx, userTopic(u.ID), "ops"); err != nil {
				return nil, err
			}
			return nil, refusal{&Problem{Msg: "this payment looks unusual (" + reason + "), so it wasn't sent and payments are paused until Kaasu reviews it", Code: 423}}
		}
	}
	from, err := s.wallet(ctx, tx, u.ID)
	if err != nil {
		return nil, err
	}
	dest, err := s.wallet(ctx, tx, to)
	if err != nil {
		return nil, err
	}
	var destBal int64
	if err := tx.QueryRow(ctx, `select balance from accounts where id = $1`, dest).Scan(&destBal); err != nil {
		return nil, err
	}
	if destBal+amount > WalletCap {
		return nil, problem("amount", "their wallet can't hold that much; they can receive %s", fmtMoney(max(0, WalletCap-destBal)))
	}
	id, err := move(ctx, tx, moveSpec{kind: "transfer", user: u.ID, counterparty: &to, from: from, to: dest, amount: amount, note: note, category: category, requestID: requestID, at: now})
	if err != nil {
		return nil, err
	}
	if _, err := tx.Exec(ctx, `insert into payees (user_id, payee_id, first_paid_at) values ($1, $2, $3) on conflict do nothing`, u.ID, to, now); err != nil {
		return nil, err
	}
	if err := audit(ctx, tx, u.ID, u.ID, "transfer", ip, map[string]any{"amount": amount, "to": to, "txn": id}); err != nil {
		return nil, err
	}
	var bal int64
	if err := tx.QueryRow(ctx, `select balance from accounts where id = $1`, from).Scan(&bal); err != nil {
		return nil, err
	}
	return &Moved{TxnID: id, Balance: bal}, notify(ctx, tx, userTopic(u.ID), userTopic(to))
}

// ---- Reading ------------------------------------------------------------

type Activity struct {
	ID        int64     `json:"id"`
	Kind      string    `json:"kind"`
	At        time.Time `json:"at"`
	Amount    int64     `json:"amount"` // + into the wallet, - out of it
	Balance   int64     `json:"balance"`
	With      string    `json:"with"`
	Note      string    `json:"note"`
	Category  string    `json:"category"`
	RequestID *int64    `json:"request_id"`
}

const activityQuery = `select t.id, t.kind, t.created_at, e.amount, e.balance_after,
	case when t.kind = 'topup' then 'Bank (UPI top-up)'
	     when t.kind in ('goal_in', 'goal_out') then 'Goal: ' || coalesce((select a2.name from entries e2 join accounts a2 on a2.id = e2.account_id where e2.txn_id = t.id and e2.account_id <> e.account_id), '')
	     when t.user_id = a.user_id then coalesce((select name from users where id = t.counterparty_id), '')
	     else (select name from users where id = t.user_id) end,
	t.note, case when e.amount > 0 and t.kind = 'transfer' then 'Money in' else t.category end, t.request_id
	from entries e join txns t on t.id = e.txn_id join accounts a on a.id = e.account_id
	where a.user_id = $1 and a.kind = 'wallet'`

func scanActivity(rows pgx.Rows) ([]Activity, error) {
	defer rows.Close()
	out := []Activity{}
	for rows.Next() {
		var a Activity
		if err := rows.Scan(&a.ID, &a.Kind, &a.At, &a.Amount, &a.Balance, &a.With, &a.Note, &a.Category, &a.RequestID); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// Activity is the wallet's history, newest first, a page at a time.
func (s *Store) Activity(ctx context.Context, u *auth.User, before int64, limit int) ([]Activity, error) {
	if err := member(u); err != nil {
		return nil, err
	}
	if limit <= 0 || limit > 100 {
		limit = 30
	}
	if before <= 0 {
		before = 1 << 62
	}
	rows, err := s.DB.Query(ctx, activityQuery+` and t.id < $2 order by t.id desc limit $3`, u.ID, before, limit)
	if err != nil {
		return nil, err
	}
	return scanActivity(rows)
}

type Wallet struct {
	Balance   int64      `json:"balance"`
	SentToday int64      `json:"sent_today"`
	SendLeft  int64      `json:"send_left"`
	TopUpLeft int64      `json:"topup_left"`
	Paused    bool       `json:"paused"`
	PausedWhy string     `json:"paused_reason"`
	PINLocked *time.Time `json:"pin_locked_until"`
	ToPay     int        `json:"to_pay"` // pending requests for this user to pay
}

func (s *Store) Wallet(ctx context.Context, u *auth.User) (*Wallet, error) {
	if err := member(u); err != nil {
		return nil, err
	}
	var w Wallet
	var why *string
	var added int64
	err := s.DB.QueryRow(ctx, `select a.balance,
		(select coalesce(sum(amount), 0) from txns where user_id = $1 and kind = 'transfer' and created_at >= $2),
		(select coalesce(sum(amount), 0) from txns where user_id = $1 and kind = 'topup' and created_at >= $2),
		u.paused_at is not null, u.paused_reason, case when u.pin_locked_until > $3 then u.pin_locked_until end,
		(select count(*) from money_requests where payer_id = $1 and status = 'pending')
		from accounts a join users u on u.id = a.user_id where a.user_id = $1 and a.kind = 'wallet'`, u.ID, s.today(), s.Now()).
		Scan(&w.Balance, &w.SentToday, &added, &w.Paused, &why, &w.PINLocked, &w.ToPay)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if why != nil {
		w.PausedWhy = *why
	}
	w.SendLeft = max(0, DailySendLimit-w.SentToday)
	w.TopUpLeft = max(0, min(DailyTopUpLimit-added, WalletCap-w.Balance))
	return &w, err
}
