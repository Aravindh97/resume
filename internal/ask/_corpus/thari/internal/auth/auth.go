// Package auth handles accounts and sessions: bcrypt password hashes,
// random session tokens stored only as SHA-256 hashes, and a login throttle.
package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"net/mail"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"golang.org/x/crypto/bcrypt"

	"github.com/Aravindh97/thari/internal/domain"
)

const SessionTTL = 7 * 24 * time.Hour

var (
	ErrBadLogin  = errors.New("wrong email or password")
	ErrThrottled = errors.New("too many attempts; wait a few minutes")
	ErrNoSession = errors.New("not signed in")
)

type User struct {
	domain.Principal
	Email   string `json:"email"`
	Name    string `json:"name"`
	OrgName string `json:"org_name"`
	Demo    bool   `json:"demo"`
	Active  bool   `json:"active"`
}

type Auth struct {
	DB   *pgxpool.Pool
	Cost int // bcrypt cost

	mu       sync.Mutex
	failures map[string][]time.Time // per email and per IP
	dummy    []byte
}

func New(db *pgxpool.Pool) *Auth {
	a := &Auth{DB: db, Cost: bcrypt.DefaultCost, failures: map[string][]time.Time{}}
	a.dummy, _ = bcrypt.GenerateFromPassword([]byte("dummy password for timing"), a.Cost)
	return a
}

func HashPassword(pw string, cost int) (string, error) {
	if len(pw) < 10 || len(pw) > 72 {
		return "", errors.New("passwords must be 10 to 72 characters")
	}
	h, err := bcrypt.GenerateFromPassword([]byte(pw), cost)
	return string(h), err
}

// RandomPassword is a 16-character password for new accounts, shown once.
func RandomPassword() string {
	b := make([]byte, 12)
	rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}

func tokenHash(t string) []byte {
	h := sha256.Sum256([]byte(t))
	return h[:]
}

// throttled reports whether key had 5 failures in the last 10 minutes.
func (a *Auth) throttled(key string, now time.Time) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	recent := a.failures[key][:0]
	for _, t := range a.failures[key] {
		if now.Sub(t) < 10*time.Minute {
			recent = append(recent, t)
		}
	}
	a.failures[key] = recent
	return len(recent) >= 5
}

func (a *Auth) fail(now time.Time, keys ...string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	for _, k := range keys {
		a.failures[k] = append(a.failures[k], now)
	}
	if len(a.failures) > 50000 {
		a.failures = map[string][]time.Time{}
	}
}

const userCols = `u.id, u.org_id, u.role, coalesce(o.factory_id, o.id), u.email, u.name, o.name, u.demo, u.active`

func scanUser(row pgx.Row) (*User, error) {
	var u User
	err := row.Scan(&u.UserID, &u.OrgID, &u.Role, &u.FactoryID, &u.Email, &u.Name, &u.OrgName, &u.Demo, &u.Active)
	return &u, err
}

// Login checks a password and returns a new session token. A missing user
// still costs one bcrypt comparison, so timing does not reveal which emails
// exist; 5 failures per email or per IP in 10 minutes block further tries.
func (a *Auth) Login(ctx context.Context, email, password, ip string) (string, *User, error) {
	email = strings.ToLower(strings.TrimSpace(email))
	now := time.Now()
	if a.throttled("e:"+email, now) || a.throttled("i:"+ip, now) {
		return "", nil, ErrThrottled
	}
	var hash string
	u, err := scanUser(a.DB.QueryRow(ctx, `select `+userCols+` from users u join orgs o on o.id = u.org_id where u.email = $1`, email))
	if err == nil {
		err = a.DB.QueryRow(ctx, `select password_hash from users where id = $1`, u.UserID).Scan(&hash)
	}
	if errors.Is(err, pgx.ErrNoRows) {
		bcrypt.CompareHashAndPassword(a.dummy, []byte(password))
		a.fail(now, "e:"+email, "i:"+ip)
		return "", nil, ErrBadLogin
	}
	if err != nil {
		return "", nil, err
	}
	if bcrypt.CompareHashAndPassword([]byte(hash), []byte(password)) != nil || !u.Active {
		a.fail(now, "e:"+email, "i:"+ip)
		return "", nil, ErrBadLogin
	}
	tok, err := a.newSession(ctx, u.UserID)
	return tok, u, err
}

// DemoLogin signs in as a demo account without a password. The server only
// exposes it when started with DEMO=1, and only accounts marked demo qualify.
func (a *Auth) DemoLogin(ctx context.Context, email string) (string, *User, error) {
	u, err := scanUser(a.DB.QueryRow(ctx, `select `+userCols+` from users u join orgs o on o.id = u.org_id where u.email = $1 and u.demo and u.active`, strings.ToLower(email)))
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil, ErrBadLogin
	}
	if err != nil {
		return "", nil, err
	}
	tok, err := a.newSession(ctx, u.UserID)
	return tok, u, err
}

func (a *Auth) newSession(ctx context.Context, userID int64) (string, error) {
	b := make([]byte, 32)
	rand.Read(b)
	tok := base64.RawURLEncoding.EncodeToString(b)
	_, err := a.DB.Exec(ctx, `insert into sessions (token_hash, user_id, expires_at) values ($1, $2, $3)`, tokenHash(tok), userID, time.Now().Add(SessionTTL))
	return tok, err
}

// Session returns the user for a session token, if it is valid and the
// account is active.
func (a *Auth) Session(ctx context.Context, tok string) (*User, error) {
	if tok == "" || len(tok) > 100 {
		return nil, ErrNoSession
	}
	u, err := scanUser(a.DB.QueryRow(ctx, `select `+userCols+` from sessions s join users u on u.id = s.user_id join orgs o on o.id = u.org_id
		where s.token_hash = $1 and s.expires_at > now() and u.active`, tokenHash(tok)))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNoSession
	}
	return u, err
}

func (a *Auth) Logout(ctx context.Context, tok string) error {
	_, err := a.DB.Exec(ctx, `delete from sessions where token_hash = $1`, tokenHash(tok))
	return err
}

func (a *Auth) ChangePassword(ctx context.Context, u *User, oldPw, newPw string) error {
	var hash string
	if err := a.DB.QueryRow(ctx, `select password_hash from users where id = $1`, u.UserID).Scan(&hash); err != nil {
		return err
	}
	if bcrypt.CompareHashAndPassword([]byte(hash), []byte(oldPw)) != nil {
		return ErrBadLogin
	}
	h, err := HashPassword(newPw, a.Cost)
	if err != nil {
		return err
	}
	// a new password signs out every other session
	_, err = a.DB.Exec(ctx, `with x as (update users set password_hash = $2 where id = $1) delete from sessions where user_id = $1`, u.UserID, h)
	return err
}

type NewUser struct {
	Email   string `json:"email"`
	Name    string `json:"name"`
	Role    string `json:"role"`
	BuyerID int64  `json:"buyer_id"` // for buyer accounts: which buyer company
}

// CreateUser adds an account in the admin's factory (or one of its buyer
// companies) with a random password, returned once to hand over.
func (a *Auth) CreateUser(ctx context.Context, admin *User, in NewUser) (*User, string, error) {
	if !admin.CanDo(domain.ManageUsers) || admin.Role == domain.Buyer {
		return nil, "", domain.ErrNotAllowed
	}
	in.Email = strings.ToLower(strings.TrimSpace(in.Email))
	in.Name = strings.TrimSpace(in.Name)
	if _, err := mail.ParseAddress(in.Email); err != nil || len(in.Email) > 200 {
		return nil, "", errors.New("enter a valid email address")
	}
	if in.Name == "" || len(in.Name) > 100 {
		return nil, "", errors.New("name is required")
	}
	org := admin.FactoryID
	switch in.Role {
	case domain.Admin, domain.Merchandiser, domain.Production:
	case domain.Buyer:
		var ok bool
		a.DB.QueryRow(ctx, `select exists (select 1 from orgs where id = $1 and kind = 'buyer' and factory_id = $2)`, in.BuyerID, admin.FactoryID).Scan(&ok)
		if !ok {
			return nil, "", errors.New("choose the buyer company this person works for")
		}
		org = in.BuyerID
	default:
		return nil, "", errors.New("unknown role")
	}
	pw := RandomPassword()
	h, err := HashPassword(pw, a.Cost)
	if err != nil {
		return nil, "", err
	}
	var id int64
	err = a.DB.QueryRow(ctx, `insert into users (org_id, email, name, role, password_hash) values ($1, $2, $3, $4, $5) returning id`, org, in.Email, in.Name, in.Role, h).Scan(&id)
	if err != nil {
		if strings.Contains(err.Error(), "23505") {
			return nil, "", errors.New("an account with this email already exists")
		}
		return nil, "", err
	}
	u, err := scanUser(a.DB.QueryRow(ctx, `select `+userCols+` from users u join orgs o on o.id = u.org_id where u.id = $1`, id))
	return u, pw, err
}

// Users lists the accounts in the admin's factory and its buyer companies.
func (a *Auth) Users(ctx context.Context, admin *User) ([]User, error) {
	if !admin.CanDo(domain.ManageUsers) || admin.Role == domain.Buyer {
		return nil, domain.ErrNotAllowed
	}
	rows, err := a.DB.Query(ctx, `select `+userCols+` from users u join orgs o on o.id = u.org_id
		where coalesce(o.factory_id, o.id) = $1 order by o.kind desc, o.name, u.name`, admin.FactoryID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []User{}
	for rows.Next() {
		u, err := scanUser(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *u)
	}
	return out, rows.Err()
}

// SetActive enables or disables an account; disabling signs it out everywhere.
func (a *Auth) SetActive(ctx context.Context, admin *User, userID int64, active bool) error {
	if !admin.CanDo(domain.ManageUsers) || admin.Role == domain.Buyer {
		return domain.ErrNotAllowed
	}
	if userID == admin.UserID && !active {
		return errors.New("you cannot disable your own account")
	}
	tag, err := a.DB.Exec(ctx, `update users u set active = $3 from orgs o where o.id = u.org_id and u.id = $1 and coalesce(o.factory_id, o.id) = $2`, userID, admin.FactoryID, active)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return errors.New("no such user")
	}
	if !active {
		_, err = a.DB.Exec(ctx, `delete from sessions where user_id = $1`, userID)
	}
	return err
}
