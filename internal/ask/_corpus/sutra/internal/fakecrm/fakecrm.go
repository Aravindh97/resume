// Package fakecrm is a small in-memory CRM API used by tests and the local
// demo. It requires a bearer token, pages with keyset pagination on
// (updated_at, id), and can inject 429 and 503 responses to exercise the
// connector's retry logic.
package fakecrm

import (
	"crypto/subtle"
	"encoding/json"
	"net/http"
	"sort"
	"strconv"
	"sync"
	"time"
)

type Server struct {
	Token string
	// FailEvery makes every Nth request fail (alternating 429 and 503).
	FailEvery int

	mu       sync.Mutex
	accounts map[string]map[string]any
	requests int
}

func New(token string, rows []map[string]string) *Server {
	s := &Server{Token: token, accounts: map[string]map[string]any{}}
	for _, r := range rows {
		m := map[string]any{}
		for k, v := range r {
			m[k] = v
		}
		s.accounts[r["id"]] = m
	}
	return s
}

// Update changes fields of an account and bumps updated_at.
func (s *Server) Update(id string, fields map[string]any, at time.Time) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	a, ok := s.accounts[id]
	if !ok {
		return false
	}
	for k, v := range fields {
		a[k] = v
	}
	a["updated_at"] = at.UTC().Format(time.RFC3339Nano)
	return true
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if subtle.ConstantTimeCompare([]byte(r.Header.Get("Authorization")), []byte("Bearer "+s.Token)) != 1 || s.Token == "" {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	s.mu.Lock()
	s.requests++
	n := s.requests
	s.mu.Unlock()
	if s.FailEvery > 0 && n%s.FailEvery == 0 {
		if (n/s.FailEvery)%2 == 1 {
			w.Header().Set("Retry-After", "1")
			http.Error(w, "rate limited", http.StatusTooManyRequests)
		} else {
			http.Error(w, "upstream unavailable", http.StatusServiceUnavailable)
		}
		return
	}
	if r.Method != http.MethodGet || r.URL.Path != "/api/v1/accounts" {
		http.NotFound(w, r)
		return
	}
	q := r.URL.Query()
	limit, _ := strconv.Atoi(q.Get("limit"))
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	var since time.Time
	if v := q.Get("since"); v != "" {
		t, err := time.Parse(time.RFC3339Nano, v)
		if err != nil {
			http.Error(w, "bad since", http.StatusBadRequest)
			return
		}
		since = t
	}
	after := q.Get("after_id")

	s.mu.Lock()
	type kv struct {
		at time.Time
		id string
		m  map[string]any
	}
	var all []kv
	for id, a := range s.accounts {
		t, _ := time.Parse(time.RFC3339Nano, a["updated_at"].(string))
		if t.After(since) || t.Equal(since) && id > after {
			cp := map[string]any{}
			for k, v := range a {
				cp[k] = v
			}
			all = append(all, kv{t, id, cp})
		}
	}
	s.mu.Unlock()
	sort.Slice(all, func(i, j int) bool {
		if !all[i].at.Equal(all[j].at) {
			return all[i].at.Before(all[j].at)
		}
		return all[i].id < all[j].id
	})
	more := len(all) > limit
	if more {
		all = all[:limit]
	}
	data := make([]map[string]any, len(all))
	for i, a := range all {
		data[i] = a.m
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{"data": data, "has_more": more})
}
