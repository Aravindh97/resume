package server

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/Aravindh97/pallanguzhi/internal/ai"
	"github.com/Aravindh97/pallanguzhi/internal/room"
)

func newTest(t *testing.T, cfg Config) *httptest.Server {
	t.Helper()
	s := New(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)
	return ts
}

func create(t *testing.T, ts *httptest.Server) string {
	t.Helper()
	r, err := http.Post(ts.URL+"/api/rooms", "application/json", nil)
	if err != nil || r.StatusCode != 201 {
		t.Fatalf("create: %v %v", err, r)
	}
	var out struct{ Code string }
	json.NewDecoder(r.Body).Decode(&out)
	return out.Code
}

type client struct {
	t    *testing.T
	c    *websocket.Conn
	seat int
	tok  string
	last room.State
}

func dial(t *testing.T, ts *httptest.Server, code, query, origin string) (*client, *http.Response, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	h := http.Header{}
	if origin != "" {
		h.Set("Origin", origin)
	}
	c, resp, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(ts.URL, "http")+"/api/rooms/"+code+"/ws?"+query, &websocket.DialOptions{HTTPHeader: h})
	if err != nil {
		return nil, resp, err
	}
	cl := &client{t: t, c: c}
	m := cl.read()
	if m.Type != "hello" {
		t.Fatalf("first message %q", m.Type)
	}
	cl.seat, cl.tok = *m.Seat, m.Token
	return cl, resp, nil
}

func (cl *client) read() ServerMsg {
	cl.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, b, err := cl.c.Read(ctx)
	if err != nil {
		cl.t.Fatalf("read: %v", err)
	}
	var m ServerMsg
	json.Unmarshal(b, &m)
	if m.State != nil {
		cl.last = *m.State
	}
	return m
}

// until reads until a state satisfies ok.
func (cl *client) until(ok func(room.State) bool) room.State {
	cl.t.Helper()
	for i := 0; i < 200; i++ {
		if m := cl.read(); m.State != nil && ok(*m.State) {
			return *m.State
		}
	}
	cl.t.Fatal("state never reached")
	return room.State{}
}

// nextError skips state updates and returns the next error message.
func (cl *client) nextError() string {
	cl.t.Helper()
	for i := 0; i < 50; i++ {
		if m := cl.read(); m.Type == "error" {
			return m.Error
		}
	}
	cl.t.Fatal("no error message")
	return ""
}

func (cl *client) send(m ClientMsg) {
	b, _ := json.Marshal(m)
	cl.c.Write(context.Background(), websocket.MessageText, b)
}

// Two players play a whole game over WebSockets, each picking moves with
// the AI; a spectator sees every position; the final scores add up to 148.
func TestFullGameOverWebSockets(t *testing.T) {
	ts := newTest(t, DefaultConfig())
	code := create(t, ts)
	a, _, err := dial(t, ts, code, "play=1&name=Anita", "")
	if err != nil {
		t.Fatal(err)
	}
	b, _, _ := dial(t, ts, code, "play=1&name=Ravi", "")
	spec, _, _ := dial(t, ts, code, "", "")
	if a.seat != 0 || b.seat != 1 || spec.seat != room.Spectator {
		t.Fatalf("seats %d %d %d", a.seat, b.seat, spec.seat)
	}
	st := a.until(func(s room.State) bool { return s.Players[1].Online && s.Spectators == 1 })
	players := []*client{a, b}
	moves := 0
	for !st.Over {
		p := players[st.Board.Turn]
		pit, _, _ := ai.Choose(st.Board, ai.Medium, nil)
		v := st.Version
		p.send(ClientMsg{Type: "move", Pit: pit, Version: v})
		st = p.until(func(s room.State) bool { return s.Version > v })
		if st.LastMove == nil || st.LastMove.Pit != pit || st.Board.Total() != 148 {
			t.Fatalf("move %d: %+v", moves, st.LastMove)
		}
		moves++
		if moves > 500 {
			t.Fatal("game did not end")
		}
	}
	end := spec.until(func(s room.State) bool { return s.Over })
	if end.Final[0]+end.Final[1] != 148 || end.Version != st.Version {
		t.Fatalf("spectator saw %+v", end.Final)
	}
	t.Logf("%d moves, final %v", moves, end.Final)
}

func TestIllegalAndOutOfTurnMovesGetErrors(t *testing.T) {
	ts := newTest(t, DefaultConfig())
	code := create(t, ts)
	a, _, _ := dial(t, ts, code, "play=1", "")
	b, _, _ := dial(t, ts, code, "play=1", "")
	st := a.until(func(s room.State) bool { return s.Players[1].Joined })
	b.send(ClientMsg{Type: "move", Pit: 7, Version: st.Version})
	if e := b.nextError(); !strings.Contains(e, "not your turn") {
		t.Fatalf("out of turn: %q", e)
	}
	a.send(ClientMsg{Type: "move", Pit: 0, Version: st.Version - 1})
	if e := a.nextError(); !strings.Contains(e, "out of date") {
		t.Fatalf("stale move: %q", e)
	}
	a.send(ClientMsg{Type: "nonsense"})
	if e := a.nextError(); !strings.Contains(e, "unknown message") {
		t.Fatalf("unknown type: %q", e)
	}
}

func TestReconnectWithToken(t *testing.T) {
	ts := newTest(t, DefaultConfig())
	code := create(t, ts)
	a, _, _ := dial(t, ts, code, "play=1&name=Anita", "")
	dial(t, ts, code, "play=1&name=Ravi", "")
	tok := a.tok
	a.c.Close(websocket.StatusNormalClosure, "")
	back, _, err := dial(t, ts, code, "play=1&token="+tok, "")
	if err != nil || back.seat != 0 {
		t.Fatalf("reconnect: %v seat %d", err, back.seat)
	}
	st := back.until(func(s room.State) bool { return s.Players[0].Online })
	if st.Players[0].Name != "Anita" {
		t.Fatal("seat name lost")
	}
}

func TestOriginChecks(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Origins = []string{"https://aravindh97.github.io"}
	ts := newTest(t, cfg)
	code := create(t, ts)
	if _, resp, err := dial(t, ts, code, "play=1", "https://evil.example"); err == nil || resp == nil || resp.StatusCode != 403 {
		t.Fatalf("foreign origin allowed: %v", err)
	}
	if _, _, err := dial(t, ts, code, "play=1", "https://aravindh97.github.io"); err != nil {
		t.Fatalf("allowed origin refused: %v", err)
	}
	req, _ := http.NewRequest("POST", ts.URL+"/api/rooms", nil)
	req.Header.Set("Origin", "https://evil.example")
	if r, _ := http.DefaultClient.Do(req); r.StatusCode != 403 {
		t.Fatalf("foreign origin created a room: %d", r.StatusCode)
	}
}

func TestLimits(t *testing.T) {
	cfg := DefaultConfig()
	cfg.RoomsPerMinute, cfg.ConnsPerIP, cfg.MsgsPerSecond = 2, 3, 5
	ts := newTest(t, cfg)
	create(t, ts)
	code := create(t, ts)
	if r, _ := http.Post(ts.URL+"/api/rooms", "", nil); r.StatusCode != 429 {
		t.Fatalf("third room this minute: %d", r.StatusCode)
	}
	var open []*client
	for i := 0; i < 3; i++ {
		c, _, err := dial(t, ts, code, "", "")
		if err != nil {
			t.Fatal(err)
		}
		open = append(open, c)
	}
	if _, resp, err := dial(t, ts, code, "", ""); err == nil || resp.StatusCode != 429 {
		t.Fatal("connection limit not enforced")
	}
	// Flooding messages closes the connection.
	flood := open[0]
	for i := 0; i < 50; i++ {
		flood.send(ClientMsg{Type: "ping"})
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	for {
		if _, _, err := flood.c.Read(ctx); err != nil {
			if websocket.CloseStatus(err) != websocket.StatusPolicyViolation {
				t.Fatalf("flood closed with %v", err)
			}
			break
		}
	}
	// Oversized messages are refused.
	big := open[1]
	big.c.Write(context.Background(), websocket.MessageText, []byte(`{"type":"move","pad":"`+strings.Repeat("x", 2000)+`"}`))
	for {
		if _, _, err := big.c.Read(ctx); err != nil {
			if websocket.CloseStatus(err) != websocket.StatusMessageTooBig {
				t.Fatalf("big message closed with %v", err)
			}
			break
		}
	}
}

func TestUnknownRoom(t *testing.T) {
	ts := newTest(t, DefaultConfig())
	if r, _ := http.Get(ts.URL + "/api/rooms/ZZZZZZ"); r.StatusCode != 404 {
		t.Fatal(r.StatusCode)
	}
	if _, resp, err := dial(t, ts, "ZZZZZZ", "play=1", ""); err == nil || resp.StatusCode != 404 {
		t.Fatal("joined a missing room")
	}
}
