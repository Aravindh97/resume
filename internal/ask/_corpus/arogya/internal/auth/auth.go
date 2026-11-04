// Package auth handles accounts and sessions: bcrypt passwords, random
// session tokens stored only as SHA-256 hashes, and a login throttle.
package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"golang.org/x/crypto/bcrypt"
)

const SessionTTL = 12 * time.Hour // a shift, roughly

var (
	ErrBadLogin  = errors.New("wrong email or password")
	ErrThrottled = errors.New("too many attempts; wait a few minutes")
	ErrNoSession = errors.New("not signed in")
)

// User is the signed-in person.
type User struct {
	ID           int64  `json:"id"`
	Email        string `json:"email"`
	Name         string `json:"name"`
	Role         string `json:"role"`
	DepartmentID *int64 `json:"department_id,omitempty"`
	Department   string `json:"department,omitempty"`
	WardID       *int64 `json:"ward_id,omitempty"`
	Ward         string `json:"ward,omitempty"`
	PatientID    *int64 `json:"patient_id,omitempty"`
	Demo         bool   `json:"demo"`
}

type Auth struct {
	DB    *pgxpool.Pool
	Cost  int
	mu    sync.Mutex
	fails map[string][]time.Time
	dummy []byte
}

func New(db *pgxpool.Pool) *Auth {
	a := &Auth{DB: db, Cost: bcrypt.DefaultCost, fails: map[string][]time.Time{}}
	a.dummy, _ = bcrypt.GenerateFromPassword([]byte("timing dummy"), a.Cost)
	return a
}

func HashPassword(pw string, cost int) (string, error) {
	if len(pw) < 10 || len(pw) > 72 {
		return "", errors.New("passwords must be 10 to 72 characters")
	}
	h, err := bcrypt.GenerateFromPassword([]byte(pw), cost)
	return string(h), err
}

func RandomPassword() string {
	b := make([]byte, 12)
	rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}

func hash(t string) []byte { h := sha256.Sum256([]byte(t)); return h[:] }

const cols = `u.id, u.email, u.name, u.role, u.department_id, coalesce(d.name, ''), u.ward_id, coalesce(w.name, ''), u.patient_id, u.demo`
const from = ` from users u left join departments d on d.id = u.department_id left join wards w on w.id = u.ward_id`

func scan(row pgx.Row) (*User, error) {
	var u User
	err := row.Scan(&u.ID, &u.Email, &u.Name, &u.Role, &u.DepartmentID, &u.Department, &u.WardID, &u.Ward, &u.PatientID, &u.Demo)
	return &u, err
}

func (a *Auth) throttled(now time.Time, keys ...string) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	for _, k := range keys {
		recent := a.fails[k][:0]
		for _, t := range a.fails[k] {
			if now.Sub(t) < 10*time.Minute {
				recent = append(recent, t)
			}
		}
		a.fails[k] = recent
		if len(recent) >= 5 {
			return true
		}
	}
	return false
}

func (a *Auth) fail(now time.Time, keys ...string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if len(a.fails) > 50000 {
		a.fails = map[string][]time.Time{}
	}
	for _, k := range keys {
		a.fails[k] = append(a.fails[k], now)
	}
}

// Login checks a password. A missing account still costs one bcrypt
// comparison, so timing does not reveal which emails exist.
func (a *Auth) Login(ctx context.Context, email, pw, ip string) (string, *User, error) {
	email = strings.ToLower(strings.TrimSpace(email))
	now := time.Now()
	if a.throttled(now, "e:"+email, "i:"+ip) {
		return "", nil, ErrThrottled
	}
	var h string
	var active bool
	err := a.DB.QueryRow(ctx, `select password_hash, active from users where email = $1`, email).Scan(&h, &active)
	if errors.Is(err, pgx.ErrNoRows) {
		bcrypt.CompareHashAndPassword(a.dummy, []byte(pw))
		a.fail(now, "e:"+email, "i:"+ip)
		return "", nil, ErrBadLogin
	}
	if err != nil {
		return "", nil, err
	}
	if bcrypt.CompareHashAndPassword([]byte(h), []byte(pw)) != nil || !active {
		a.fail(now, "e:"+email, "i:"+ip)
		return "", nil, ErrBadLogin
	}
	u, err := scan(a.DB.QueryRow(ctx, `select `+cols+from+` where u.email = $1`, email))
	if err != nil {
		return "", nil, err
	}
	tok, err := a.session(ctx, u.ID)
	return tok, u, err
}

// DemoLogin: one click into a demo account. Only reachable when the server
// runs with DEMO=1, and only for accounts marked demo.
func (a *Auth) DemoLogin(ctx context.Context, email string) (string, *User, error) {
	u, err := scan(a.DB.QueryRow(ctx, `select `+cols+from+` where u.email = $1 and u.demo and u.active`, strings.ToLower(email)))
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil, ErrBadLogin
	}
	if err != nil {
		return "", nil, err
	}
	tok, err := a.session(ctx, u.ID)
	return tok, u, err
}

func (a *Auth) session(ctx context.Context, user int64) (string, error) {
	b := make([]byte, 32)
	rand.Read(b)
	tok := base64.RawURLEncoding.EncodeToString(b)
	_, err := a.DB.Exec(ctx, `insert into sessions (token_hash, user_id, expires_at) values ($1, $2, $3)`, hash(tok), user, time.Now().Add(SessionTTL))
	return tok, err
}

func (a *Auth) Session(ctx context.Context, tok string) (*User, error) {
	if tok == "" || len(tok) > 100 {
		return nil, ErrNoSession
	}
	u, err := scan(a.DB.QueryRow(ctx, `select `+cols+from+` join sessions s on s.user_id = u.id where s.token_hash = $1 and s.expires_at > now() and u.active`, hash(tok)))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNoSession
	}
	return u, err
}

func (a *Auth) Logout(ctx context.Context, tok string) error {
	_, err := a.DB.Exec(ctx, `delete from sessions where token_hash = $1`, hash(tok))
	return err
}
