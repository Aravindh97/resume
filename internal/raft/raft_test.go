package raft

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"
)

type memNet struct {
	mu    sync.Mutex
	nodes map[string]*Node
	down  map[string]bool // simulates a crashed process (no state loss)
}

var errDown = errors.New("unreachable")

func (m *memNet) get(from, to string) (*Node, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.down[from] || m.down[to] {
		return nil, errDown
	}
	return m.nodes[to], nil
}

type memTransport struct {
	net  *memNet
	from string
}

func (t memTransport) RequestVote(ctx context.Context, p string, a VoteArgs) (VoteReply, error) {
	n, err := t.net.get(t.from, p)
	if err != nil {
		return VoteReply{}, err
	}
	if t.net.nodes[t.from].Isolated() {
		return VoteReply{}, ErrIsolated
	}
	return n.HandleVote(a)
}

func (t memTransport) AppendEntries(ctx context.Context, p string, a AppendArgs) (AppendReply, error) {
	n, err := t.net.get(t.from, p)
	if err != nil {
		return AppendReply{}, err
	}
	if t.net.nodes[t.from].Isolated() {
		return AppendReply{}, ErrIsolated
	}
	return n.HandleAppend(a)
}

type cluster struct {
	net     *memNet
	nodes   []*Node
	mu      sync.Mutex
	applied map[string][]string
	cancel  context.CancelFunc
}

func newCluster(t *testing.T, n int) *cluster {
	c := &cluster{net: &memNet{nodes: map[string]*Node{}, down: map[string]bool{}}, applied: map[string][]string{}}
	ids := make([]string, n)
	for i := range ids {
		ids[i] = fmt.Sprintf("n%d", i)
	}
	for _, id := range ids {
		var peers []string
		for _, p := range ids {
			if p != id {
				peers = append(peers, p)
			}
		}
		id := id
		node := New(Config{ID: id, Peers: peers, Transport: memTransport{c.net, id},
			ElectionTimeout: 60 * time.Millisecond, HeartbeatTimeout: 15 * time.Millisecond,
			Apply: func(i int, e Entry) {
				c.mu.Lock()
				c.applied[id] = append(c.applied[id], string(e.Cmd))
				c.mu.Unlock()
			}})
		c.net.nodes[id] = node
		c.nodes = append(c.nodes, node)
	}
	ctx, cancel := context.WithCancel(context.Background())
	c.cancel = cancel
	for _, nd := range c.nodes {
		go nd.Run(ctx)
	}
	t.Cleanup(cancel)
	return c
}

// leader waits for exactly one leader among reachable nodes and checks
// there are never two leaders in the same term.
func (c *cluster) leader(t *testing.T) *Node {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		byTerm := map[uint64][]string{}
		var l *Node
		for _, n := range c.nodes {
			s := n.Status()
			if s.Role == Leader && !s.Isolated && !c.net.down[s.ID] {
				byTerm[s.Term] = append(byTerm[s.Term], s.ID)
				l = n
			}
		}
		for term, ls := range byTerm {
			if len(ls) > 1 {
				t.Fatalf("split brain: leaders %v in term %d", ls, term)
			}
		}
		if l != nil {
			return l
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("no leader elected")
	return nil
}

func propose(t *testing.T, l *Node, cmd string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if _, err := l.Propose(ctx, []byte(cmd)); err != nil {
		t.Fatalf("propose %s: %v", cmd, err)
	}
}

func (c *cluster) waitApplied(t *testing.T, id string, n int) []string {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		c.mu.Lock()
		got := append([]string(nil), c.applied[id]...)
		c.mu.Unlock()
		if len(got) >= n {
			return got
		}
		time.Sleep(10 * time.Millisecond)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	t.Fatalf("%s applied %v, want %d entries", id, c.applied[id], n)
	return nil
}

func TestElectsOneLeaderAndReplicates(t *testing.T) {
	c := newCluster(t, 4)
	l := c.leader(t)
	propose(t, l, "a")
	propose(t, l, "b")
	for _, n := range c.nodes {
		if got := c.waitApplied(t, n.cfg.ID, 2); got[0] != "a" || got[1] != "b" {
			t.Fatalf("%s applied %v", n.cfg.ID, got)
		}
	}
}

func TestLeaderIsolationTriggersFailover(t *testing.T) {
	c := newCluster(t, 4)
	old := c.leader(t)
	propose(t, old, "v1")
	old.Isolate(500 * time.Millisecond)
	var l *Node
	for {
		l = c.leader(t)
		if l != old {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if l.Status().Term <= old.Status().Term {
		t.Fatalf("new leader term %d not above old %d", l.Status().Term, old.Status().Term)
	}
	propose(t, l, "v2") // 3 of 4 nodes is still a majority
	// After the partition heals, the old leader catches up.
	got := c.waitApplied(t, old.cfg.ID, 2)
	if got[0] != "v1" || got[1] != "v2" {
		t.Fatalf("old leader applied %v", got)
	}
}

func TestNoQuorumNoCommit(t *testing.T) {
	c := newCluster(t, 4)
	l := c.leader(t)
	n := 0
	for _, x := range c.nodes {
		if x != l && n < 2 {
			x.Isolate(time.Second)
			n++
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	if _, err := l.Propose(ctx, []byte("lost")); err == nil {
		t.Fatal("committed with only 2 of 4 nodes")
	}
}

func TestDivergentFollowerLogIsRepaired(t *testing.T) {
	c := newCluster(t, 4)
	l := c.leader(t)
	propose(t, l, "x")
	// Crash a follower, commit more entries without it, bring it back.
	var f *Node
	for _, n := range c.nodes {
		if n != l {
			f = n
			break
		}
	}
	c.net.mu.Lock()
	c.net.down[f.cfg.ID] = true
	c.net.mu.Unlock()
	for i := 0; i < 5; i++ {
		propose(t, l, fmt.Sprintf("y%d", i))
	}
	c.net.mu.Lock()
	c.net.down[f.cfg.ID] = false
	c.net.mu.Unlock()
	got := c.waitApplied(t, f.cfg.ID, 6)
	if got[5] != "y4" {
		t.Fatalf("follower applied %v", got)
	}
}
