// Package memory is Smriti's core model: scoped, bitemporal memories with
// provenance.
//
// A memory belongs to exactly one scope:
//
//	user  - owned by one user, visible only to that user
//	team  - owned by a team, visible to its members
//	org   - owned by the organisation, visible to everyone in it
//
// Memories are never updated in place. A changed fact closes the old row
// (valid_to = when the new value was stated, superseded_by = new id) and
// inserts a new one. valid_from/valid_to is valid time (when the fact was
// true, taken from the message timestamp); recorded_at is when Smriti learned
// it. Recall can therefore answer "what is true now" and "what did we believe
// on date X".
package memory

import (
	"fmt"
	"strings"
	"time"
)

type Scope string

const (
	User Scope = "user"
	Team Scope = "team"
	Org  Scope = "org"
)

func (s Scope) Valid() bool { return s == User || s == Team || s == Org }

// Principal is who is reading or writing. Teams must come from a trusted
// source (the caller's identity system), never from model output.
type Principal struct {
	OrgID  string
	UserID string
	Teams  []string
	// OrgWriter allows writing org-scope memories. Without it, a single user
	// (or a prompt injected into their conversation) could change what every
	// agent in the company believes.
	OrgWriter bool
}

// Owner returns the owner id for a scope from the principal's point of view.
// A principal can write team memories only for a team it belongs to, and
// org memories only with OrgWriter.
func (p Principal) Owner(s Scope, team string) (string, error) {
	switch s {
	case User:
		return p.UserID, nil
	case Org:
		if !p.OrgWriter {
			return "", fmt.Errorf("user %s may not write org memories", p.UserID)
		}
		return p.OrgID, nil
	case Team:
		for _, t := range p.Teams {
			if t == team {
				return team, nil
			}
		}
		return "", fmt.Errorf("user %s is not a member of team %q", p.UserID, team)
	}
	return "", fmt.Errorf("invalid scope %q", s)
}

type Memory struct {
	ID            string     `json:"id"`
	OrgID         string     `json:"org_id"`
	Scope         Scope      `json:"scope"`
	OwnerID       string     `json:"owner_id"`
	Subject       string     `json:"subject"` // normalised key, e.g. "preferred_language"
	Content       string     `json:"content"`
	Confidence    float64    `json:"confidence"`
	Confirmations int        `json:"confirmations"`
	SourceRef     string     `json:"source_ref"` // conversation/message the fact came from
	ValidFrom     time.Time  `json:"valid_from"`
	ValidTo       *time.Time `json:"valid_to,omitempty"`
	RecordedAt    time.Time  `json:"recorded_at"`
	SupersededBy  string     `json:"superseded_by,omitempty"`
	ExpiresAt     *time.Time `json:"expires_at,omitempty"`
	LastConfirmed time.Time  `json:"last_confirmed"`
	Embedding     []float32  `json:"-"`
}

// Candidate is a fact proposed by an extractor, before reconciliation.
type Candidate struct {
	Scope      Scope   `json:"scope"`
	Team       string  `json:"team,omitempty"` // required for team scope
	Subject    string  `json:"subject"`
	Content    string  `json:"content"`
	Confidence float64 `json:"confidence"`
	SourceRef  string  `json:"source_ref"`
	// TTL, if set, makes the memory expire (e.g. "I'm travelling this week").
	TTL time.Duration `json:"ttl,omitempty"`
	// ObservedAt is when the user said it (valid time). Zero means now.
	ObservedAt time.Time `json:"observed_at,omitempty"`
}

// NormSubject turns a free-form subject into a stable key.
func NormSubject(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	var b strings.Builder
	us := false
	for _, r := range s {
		if r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r > 127 {
			b.WriteRune(r)
			us = false
		} else if !us && b.Len() > 0 {
			b.WriteByte('_')
			us = true
		}
	}
	return strings.TrimSuffix(b.String(), "_")
}

type Message struct {
	ID   string    `json:"id"`
	Role string    `json:"role"` // user | assistant
	Text string    `json:"text"`
	At   time.Time `json:"at"`
}

type Conversation struct {
	ID       string    `json:"id"`
	Messages []Message `json:"messages"`
}
