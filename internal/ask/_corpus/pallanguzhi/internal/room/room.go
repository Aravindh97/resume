// Package room runs online Pallanguzhi games. The server owns the board:
// clients only ask to lift a pit, and the room checks that it is that
// player's turn, that the pit is legal, and that the request was made
// against the latest position (so a double-click or a stale tab cannot
// play twice).
//
// A player holds a seat with a random token the server gives them. The token
// lets the same browser take the seat back after a dropped connection; it is
// not an account, and nothing about the player is stored after the room
// expires.
package room

import (
	"crypto/rand"
	"encoding/base32"
	"errors"
	"strings"
	"sync"
	"time"

	"github.com/Aravindh97/pallanguzhi/internal/game"
)

const (
	Spectator     = -1
	MaxSpectators = 50
	MaxNameLen    = 24
	IdleTTL       = 15 * time.Minute // a room with nobody connected for this long is removed
	MaxAge        = 6 * time.Hour
)

var (
	ErrNoRoom    = errors.New("no such room (it may have expired)")
	ErrFull      = errors.New("room is full")
	ErrNotPlayer = errors.New("spectators cannot move")
	ErrNotTurn   = errors.New("it is not your turn")
	ErrStale     = errors.New("the board changed; your view was out of date")
	ErrOver      = errors.New("the game is over")
	ErrTooMany   = errors.New("too many rooms right now, try again later")
)

type PlayerView struct {
	Name   string `json:"name"`
	Joined bool   `json:"joined"`
	Online bool   `json:"online"`
}

// LastMove describes the move that produced the current board.
type LastMove struct {
	Seat  int          `json:"seat"`
	Pit   int          `json:"pit"`
	Trace []game.Event `json:"trace"`
}

// State is what every connected client receives after each change.
type State struct {
	Code       string        `json:"code"`
	Board      game.Board    `json:"board"`
	Over       bool          `json:"over"`
	Final      [2]int        `json:"final"`
	Moves      []int         `json:"moves"`
	Players    [2]PlayerView `json:"players"`
	Spectators int           `json:"spectators"`
	Version    int           `json:"version"`
	Game       int           `json:"game"` // 1 for the first game, +1 per rematch
	Rematch    [2]bool       `json:"rematch"`
	LastMove   *LastMove     `json:"last_move,omitempty"`
}

type seat struct {
	name   string
	token  string
	online int // open connections holding this seat
}

// Sub is one connected client. Notify fires when there is a newer state;
// Take returns the newest one. A client that falls behind skips states it
// never read instead of queueing them, so a slow connection costs one state
// of memory and is never disconnected for being slow. Notify is closed when
// the client leaves or the room expires.
type Sub struct {
	Notify  chan struct{}
	Seat    int
	Token   string
	room    *Room
	gone    bool
	pending *State
}

// Take returns the newest state not yet taken; ok is false if there is none.
func (s *Sub) Take() (State, bool) {
	s.room.mu.Lock()
	defer s.room.mu.Unlock()
	if s.pending == nil {
		return State{}, false
	}
	st := *s.pending
	s.pending = nil
	return st, true
}

type Room struct {
	mu         sync.Mutex
	code       string
	board      game.Board
	seats      [2]seat
	subs       map[*Sub]struct{}
	version    int
	gameNo     int
	rematch    [2]bool
	last       *LastMove
	history    [][2]int // (seat, pit) of every move this game, for replay and audit
	created    time.Time
	emptySince time.Time
}

type Hub struct {
	mu       sync.Mutex
	rooms    map[string]*Room
	MaxRooms int
	now      func() time.Time
}

func NewHub() *Hub { return &Hub{rooms: map[string]*Room{}, MaxRooms: 2000, now: time.Now} }

var enc = base32.NewEncoding("ABCDEFGHJKLMNPQRSTUVWXYZ23456789").WithPadding(base32.NoPadding)

func randomString(nBytes int) string {
	b := make([]byte, nBytes)
	rand.Read(b)
	return enc.EncodeToString(b)
}

// Create makes an empty room and returns its six-character code.
func (h *Hub) Create() (string, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.rooms) >= h.MaxRooms {
		return "", ErrTooMany
	}
	for {
		code := randomString(4)[:6]
		if _, taken := h.rooms[code]; taken {
			continue
		}
		now := h.now()
		h.rooms[code] = &Room{code: code, board: game.New(), subs: map[*Sub]struct{}{}, gameNo: 1, created: now, emptySince: now}
		return code, nil
	}
}

func (h *Hub) get(code string) (*Room, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	r, ok := h.rooms[strings.ToUpper(strings.TrimSpace(code))]
	if !ok {
		return nil, ErrNoRoom
	}
	return r, nil
}

// Rooms is the number of open rooms.
func (h *Hub) Rooms() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.rooms)
}

// Peek returns a room's state without joining it.
func (h *Hub) Peek(code string) (State, error) {
	r, err := h.get(code)
	if err != nil {
		return State{}, err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.stateLocked(false), nil
}

func cleanName(n string) string {
	n = strings.Join(strings.Fields(n), " ")
	if r := []rune(n); len(r) > MaxNameLen {
		n = string(r[:MaxNameLen])
	}
	if n == "" {
		n = "Player"
	}
	return n
}

// Join connects a client. With a token that holds a seat, it takes that seat
// back; otherwise it takes a free seat if asked to play, or watches.
func (h *Hub) Join(code, name, token string, play bool) (*Sub, error) {
	r, err := h.get(code)
	if err != nil {
		return nil, err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	s := &Sub{Notify: make(chan struct{}, 1), Seat: Spectator, room: r}
	for i := range r.seats {
		if token != "" && r.seats[i].token == token {
			s.Seat, s.Token = i, token
		}
	}
	if s.Seat == Spectator && play {
		for i := range r.seats {
			if r.seats[i].token == "" {
				r.seats[i] = seat{name: cleanName(name), token: randomString(16)}
				s.Seat, s.Token = i, r.seats[i].token
				r.version++
				break
			}
		}
	}
	if s.Seat == Spectator && r.spectatorsLocked() >= MaxSpectators {
		return nil, ErrFull
	}
	if s.Seat != Spectator {
		r.seats[s.Seat].online++
	}
	r.subs[s] = struct{}{}
	r.broadcastLocked(false)
	return s, nil
}

// Leave disconnects a client. Its seat stays reserved for its token.
func (s *Sub) Leave(now time.Time) {
	r := s.room
	r.mu.Lock()
	defer r.mu.Unlock()
	if s.gone {
		return
	}
	s.gone = true
	delete(r.subs, s)
	close(s.Notify)
	if s.Seat != Spectator {
		r.seats[s.Seat].online--
	}
	if len(r.subs) == 0 {
		r.emptySince = now
	}
	r.broadcastLocked(false)
}

// Move lifts a pit for the client's seat. version must be the version of
// the state the client was looking at.
func (s *Sub) Move(pit, version int) error {
	r := s.room
	r.mu.Lock()
	defer r.mu.Unlock()
	switch {
	case s.Seat == Spectator:
		return ErrNotPlayer
	case r.board.Over():
		return ErrOver
	case version != r.version:
		return ErrStale
	case r.board.Turn != s.Seat:
		return ErrNotTurn
	}
	var tr []game.Event
	if err := r.board.Play(pit, &tr); err != nil {
		return err
	}
	r.version++
	r.history = append(r.history, [2]int{s.Seat, pit})
	r.last = &LastMove{Seat: s.Seat, Pit: pit, Trace: tr}
	r.broadcastLocked(true)
	return nil
}

// Rematch records that this player wants another game; when both do, a new
// game starts with the other player moving first.
func (s *Sub) Rematch() error {
	r := s.room
	r.mu.Lock()
	defer r.mu.Unlock()
	if s.Seat == Spectator {
		return ErrNotPlayer
	}
	if !r.board.Over() {
		return errors.New("finish this game first")
	}
	r.rematch[s.Seat] = true
	if r.rematch[0] && r.rematch[1] {
		r.gameNo++
		r.board = game.New()
		r.board.Turn = (r.gameNo + 1) % 2 // game 1: South first, game 2: North first, ...
		r.rematch = [2]bool{}
		r.last = nil
		r.history = nil
	}
	r.version++
	r.broadcastLocked(false)
	return nil
}

func (r *Room) spectatorsLocked() int {
	n := 0
	for s := range r.subs {
		if s.Seat == Spectator {
			n++
		}
	}
	return n
}

func (r *Room) stateLocked(withMove bool) State {
	st := State{Code: r.code, Board: r.board, Over: r.board.Over(), Final: r.board.Final(), Moves: r.board.Moves(),
		Spectators: r.spectatorsLocked(), Version: r.version, Game: r.gameNo, Rematch: r.rematch}
	for i, s := range r.seats {
		st.Players[i] = PlayerView{Name: s.name, Joined: s.token != "", Online: s.online > 0}
	}
	if withMove {
		st.LastMove = r.last
	}
	return st
}

// broadcastLocked gives every client the new state. It never blocks: a
// client that has not taken the previous state yet just gets this one instead.
func (r *Room) broadcastLocked(withMove bool) {
	st := r.stateLocked(withMove)
	for s := range r.subs {
		s.pending = &st
		select {
		case s.Notify <- struct{}{}:
		default: // already notified
		}
	}
}

// Sweep removes rooms nobody has been connected to for IdleTTL, and rooms
// older than MaxAge. It returns how many were removed.
func (h *Hub) Sweep() int {
	now := h.now()
	h.mu.Lock()
	defer h.mu.Unlock()
	n := 0
	for code, r := range h.rooms {
		r.mu.Lock()
		dead := len(r.subs) == 0 && now.Sub(r.emptySince) > IdleTTL || now.Sub(r.created) > MaxAge
		if dead {
			for s := range r.subs {
				s.gone = true
				close(s.Notify)
			}
			r.subs = nil
		}
		r.mu.Unlock()
		if dead {
			delete(h.rooms, code)
			n++
		}
	}
	return n
}
