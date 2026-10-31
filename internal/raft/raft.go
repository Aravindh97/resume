// Package raft is a compact implementation of the Raft consensus algorithm
// (Ongaro & Ousterhout, 2014): leader election, log replication and commit
// by majority. It replicates the resume document across the mesh.
//
// Scope: state is kept in memory, so a node that restarts rejoins as a fresh
// follower and is caught up by the leader. Chaos "kills" in the demo are
// network isolation, which keeps state, exactly like a partition. There is
// no log compaction or membership change; the cluster is fixed at start.
package raft

import (
	"context"
	"errors"
	"math/rand/v2"
	"sync"
	"time"
)

type Role string

const (
	Follower  Role = "follower"
	Candidate Role = "candidate"
	Leader    Role = "leader"
)

type Entry struct {
	Term uint64 `json:"term"`
	Cmd  []byte `json:"cmd"`
}

type VoteArgs struct {
	Term         uint64 `json:"term"`
	Candidate    string `json:"candidate"`
	LastLogIndex int    `json:"last_log_index"`
	LastLogTerm  uint64 `json:"last_log_term"`
}

type VoteReply struct {
	Term    uint64 `json:"term"`
	Granted bool   `json:"granted"`
}

type AppendArgs struct {
	Term         uint64  `json:"term"`
	Leader       string  `json:"leader"`
	PrevLogIndex int     `json:"prev_log_index"`
	PrevLogTerm  uint64  `json:"prev_log_term"`
	Entries      []Entry `json:"entries"`
	LeaderCommit int     `json:"leader_commit"`
}

type AppendReply struct {
	Term    uint64 `json:"term"`
	Success bool   `json:"success"`
	// ConflictIndex lets the leader skip back a whole term at a time instead
	// of one entry per round trip.
	ConflictIndex int `json:"conflict_index"`
}

// Transport sends RPCs to peers.
type Transport interface {
	RequestVote(ctx context.Context, peer string, a VoteArgs) (VoteReply, error)
	AppendEntries(ctx context.Context, peer string, a AppendArgs) (AppendReply, error)
}

type Config struct {
	ID        string
	Peers     []string // other node ids
	Transport Transport
	// Apply is called, in order and exactly once per index, for committed entries.
	Apply            func(index int, e Entry)
	ElectionTimeout  time.Duration // randomised in [t, 2t)
	HeartbeatTimeout time.Duration
	Logf             func(format string, args ...any)
}

var (
	ErrNotLeader = errors.New("not the leader")
	ErrIsolated  = errors.New("node is isolated")
)

type Node struct {
	cfg Config

	mu          sync.Mutex
	role        Role
	term        uint64
	votedFor    string
	log         []Entry // log[0] is a sentinel; real entries start at index 1
	commitIndex int
	lastApplied int
	leader      string
	nextIndex   map[string]int
	matchIndex  map[string]int
	lastContact time.Time
	isolatedTo  time.Time
	electionAt  time.Time
	stopped     bool
	commitCh    chan struct{}
	waiters     map[int][]chan uint64 // index -> notify with the term committed at that index
	elections   int
}

func New(cfg Config) *Node {
	if cfg.ElectionTimeout == 0 {
		cfg.ElectionTimeout = 400 * time.Millisecond
	}
	if cfg.HeartbeatTimeout == 0 {
		cfg.HeartbeatTimeout = 100 * time.Millisecond
	}
	if cfg.Logf == nil {
		cfg.Logf = func(string, ...any) {}
	}
	n := &Node{cfg: cfg, role: Follower, log: []Entry{{}}, commitCh: make(chan struct{}, 1),
		waiters: map[int][]chan uint64{}, lastContact: time.Now()}
	n.resetElectionLocked()
	return n
}

func (n *Node) resetElectionLocked() {
	t := n.cfg.ElectionTimeout
	n.electionAt = time.Now().Add(t + time.Duration(rand.Int64N(int64(t))))
}

func (n *Node) isolatedLocked() bool { return time.Now().Before(n.isolatedTo) }

// Isolate cuts the node off from its peers for d, like a network partition.
func (n *Node) Isolate(d time.Duration) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.isolatedTo = time.Now().Add(d)
	n.cfg.Logf("%s isolated for %s", n.cfg.ID, d)
}

func (n *Node) Isolated() bool {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.isolatedLocked()
}

// Run drives timers until ctx is cancelled.
func (n *Node) Run(ctx context.Context) {
	go n.applier(ctx)
	tick := time.NewTicker(n.cfg.HeartbeatTimeout / 2)
	defer tick.Stop()
	lastBeat := time.Time{}
	for {
		select {
		case <-ctx.Done():
			n.mu.Lock()
			n.stopped = true
			n.mu.Unlock()
			return
		case <-tick.C:
		}
		n.mu.Lock()
		if n.isolatedLocked() {
			// While cut off, a leader cannot reach anyone; step down so a
			// stale leader never believes it can still commit.
			if n.role == Leader {
				n.role, n.leader = Follower, ""
			}
			n.resetElectionLocked()
			n.mu.Unlock()
			continue
		}
		switch n.role {
		case Leader:
			if time.Since(lastBeat) >= n.cfg.HeartbeatTimeout {
				lastBeat = time.Now()
				n.mu.Unlock()
				n.broadcastAppend(ctx)
				continue
			}
		default:
			if time.Now().After(n.electionAt) {
				n.mu.Unlock()
				n.startElection(ctx)
				continue
			}
		}
		n.mu.Unlock()
	}
}

func (n *Node) lastLog() (int, uint64) {
	i := len(n.log) - 1
	return i, n.log[i].Term
}

func (n *Node) stepDownLocked(term uint64) {
	if term > n.term {
		n.term, n.votedFor = term, ""
	}
	if n.role != Follower {
		n.cfg.Logf("%s steps down in term %d", n.cfg.ID, n.term)
	}
	n.role = Follower
}

func (n *Node) startElection(ctx context.Context) {
	n.mu.Lock()
	n.role = Candidate
	n.term++
	n.votedFor = n.cfg.ID
	n.leader = ""
	n.elections++
	n.resetElectionLocked()
	term := n.term
	li, lt := n.lastLog()
	n.mu.Unlock()
	n.cfg.Logf("%s starts election for term %d", n.cfg.ID, term)

	votes := 1
	need := (len(n.cfg.Peers)+1)/2 + 1
	if need <= 1 {
		n.mu.Lock()
		n.becomeLeaderLocked()
		n.mu.Unlock()
		return
	}
	var wg sync.WaitGroup
	var vmu sync.Mutex
	for _, p := range n.cfg.Peers {
		wg.Add(1)
		go func(p string) {
			defer wg.Done()
			c, cancel := context.WithTimeout(ctx, n.cfg.ElectionTimeout)
			defer cancel()
			r, err := n.cfg.Transport.RequestVote(c, p, VoteArgs{Term: term, Candidate: n.cfg.ID, LastLogIndex: li, LastLogTerm: lt})
			if err != nil {
				return
			}
			n.mu.Lock()
			defer n.mu.Unlock()
			if r.Term > n.term {
				n.stepDownLocked(r.Term)
				n.resetElectionLocked()
				return
			}
			if !r.Granted || n.role != Candidate || n.term != term {
				return
			}
			vmu.Lock()
			votes++
			won := votes == need
			vmu.Unlock()
			if won {
				n.becomeLeaderLocked()
			}
		}(p)
	}
	wg.Wait()
	n.mu.Lock()
	isLeader := n.role == Leader && n.term == term
	n.mu.Unlock()
	if isLeader {
		n.broadcastAppend(ctx)
	}
}

func (n *Node) becomeLeaderLocked() {
	n.role, n.leader = Leader, n.cfg.ID
	n.nextIndex, n.matchIndex = map[string]int{}, map[string]int{}
	for _, p := range n.cfg.Peers {
		n.nextIndex[p] = len(n.log)
		n.matchIndex[p] = 0
	}
	// A no-op entry in the new term lets the leader commit entries from
	// earlier terms (Raft section 5.4.2).
	n.log = append(n.log, Entry{Term: n.term})
	n.cfg.Logf("%s is leader for term %d", n.cfg.ID, n.term)
}

func (n *Node) broadcastAppend(ctx context.Context) {
	n.mu.Lock()
	if n.role != Leader {
		n.mu.Unlock()
		return
	}
	n.mu.Unlock()
	for _, p := range n.cfg.Peers {
		go n.replicate(ctx, p)
	}
}

func (n *Node) replicate(ctx context.Context, p string) {
	n.mu.Lock()
	if n.role != Leader || n.isolatedLocked() {
		n.mu.Unlock()
		return
	}
	next := n.nextIndex[p]
	if next < 1 {
		next = 1
	}
	prev := next - 1
	args := AppendArgs{Term: n.term, Leader: n.cfg.ID, PrevLogIndex: prev, PrevLogTerm: n.log[prev].Term,
		Entries: append([]Entry(nil), n.log[next:]...), LeaderCommit: n.commitIndex}
	n.mu.Unlock()

	c, cancel := context.WithTimeout(ctx, n.cfg.HeartbeatTimeout*3)
	defer cancel()
	r, err := n.cfg.Transport.AppendEntries(c, p, args)
	if err != nil {
		return
	}
	n.mu.Lock()
	defer n.mu.Unlock()
	if r.Term > n.term {
		n.stepDownLocked(r.Term)
		n.resetElectionLocked()
		return
	}
	if n.role != Leader || n.term != args.Term {
		return
	}
	if r.Success {
		m := args.PrevLogIndex + len(args.Entries)
		if m > n.matchIndex[p] {
			n.matchIndex[p] = m
		}
		n.nextIndex[p] = m + 1
		n.advanceCommitLocked()
		return
	}
	n.nextIndex[p] = max(1, min(r.ConflictIndex, n.nextIndex[p]-1))
}

// advanceCommitLocked commits the highest index replicated on a majority,
// but only for entries of the current term (Raft section 5.4.2).
func (n *Node) advanceCommitLocked() {
	for idx := len(n.log) - 1; idx > n.commitIndex; idx-- {
		if n.log[idx].Term != n.term {
			break
		}
		count := 1
		for _, p := range n.cfg.Peers {
			if n.matchIndex[p] >= idx {
				count++
			}
		}
		if count*2 > len(n.cfg.Peers)+1 {
			n.commitIndex = idx
			n.signalCommit()
			return
		}
	}
}

func (n *Node) signalCommit() {
	select {
	case n.commitCh <- struct{}{}:
	default:
	}
}

func (n *Node) applier(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-n.commitCh:
		}
		for {
			n.mu.Lock()
			if n.lastApplied >= n.commitIndex {
				n.mu.Unlock()
				break
			}
			n.lastApplied++
			i, e := n.lastApplied, n.log[n.lastApplied]
			ws := n.waiters[i]
			delete(n.waiters, i)
			n.mu.Unlock()
			if len(e.Cmd) > 0 && n.cfg.Apply != nil {
				n.cfg.Apply(i, e)
			}
			for _, w := range ws {
				w <- e.Term
			}
		}
	}
}

// HandleVote is the RequestVote RPC handler.
func (n *Node) HandleVote(a VoteArgs) (VoteReply, error) {
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.isolatedLocked() {
		return VoteReply{}, ErrIsolated
	}
	if a.Term > n.term {
		n.stepDownLocked(a.Term)
	}
	r := VoteReply{Term: n.term}
	if a.Term < n.term {
		return r, nil
	}
	li, lt := n.lastLog()
	upToDate := a.LastLogTerm > lt || a.LastLogTerm == lt && a.LastLogIndex >= li
	if (n.votedFor == "" || n.votedFor == a.Candidate) && upToDate {
		n.votedFor = a.Candidate
		r.Granted = true
		n.resetElectionLocked()
	}
	return r, nil
}

// HandleAppend is the AppendEntries RPC handler.
func (n *Node) HandleAppend(a AppendArgs) (AppendReply, error) {
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.isolatedLocked() {
		return AppendReply{}, ErrIsolated
	}
	if a.Term < n.term {
		return AppendReply{Term: n.term}, nil
	}
	if a.Term > n.term || n.role != Follower {
		n.stepDownLocked(a.Term)
	}
	n.leader = a.Leader
	n.lastContact = time.Now()
	n.resetElectionLocked()
	r := AppendReply{Term: n.term}
	if a.PrevLogIndex >= len(n.log) {
		r.ConflictIndex = len(n.log)
		return r, nil
	}
	if n.log[a.PrevLogIndex].Term != a.PrevLogTerm {
		t := n.log[a.PrevLogIndex].Term
		ci := a.PrevLogIndex
		for ci > 1 && n.log[ci-1].Term == t {
			ci--
		}
		r.ConflictIndex = ci
		return r, nil
	}
	for i, e := range a.Entries {
		idx := a.PrevLogIndex + 1 + i
		if idx < len(n.log) {
			if n.log[idx].Term == e.Term {
				continue
			}
			n.log = n.log[:idx] // conflict: drop this and everything after
		}
		n.log = append(n.log, a.Entries[i:]...)
		break
	}
	if a.LeaderCommit > n.commitIndex {
		n.commitIndex = min(a.LeaderCommit, a.PrevLogIndex+len(a.Entries))
		n.signalCommit()
	}
	r.Success = true
	return r, nil
}

// Propose appends cmd on the leader and waits until it is committed.
func (n *Node) Propose(ctx context.Context, cmd []byte) (int, error) {
	n.mu.Lock()
	if n.role != Leader {
		n.mu.Unlock()
		return 0, ErrNotLeader
	}
	n.log = append(n.log, Entry{Term: n.term, Cmd: cmd})
	idx, term := len(n.log)-1, n.term
	ch := make(chan uint64, 1)
	n.waiters[idx] = append(n.waiters[idx], ch)
	n.mu.Unlock()
	go n.broadcastAppend(ctx)
	select {
	case t := <-ch:
		if t != term {
			return 0, errors.New("entry was overwritten by a new leader")
		}
		return idx, nil
	case <-ctx.Done():
		return 0, ctx.Err()
	}
}

type Status struct {
	ID          string `json:"id"`
	Role        Role   `json:"role"`
	Term        uint64 `json:"term"`
	Leader      string `json:"leader"`
	CommitIndex int    `json:"commit_index"`
	LastIndex   int    `json:"last_index"`
	Isolated    bool   `json:"isolated"`
	Elections   int    `json:"elections"`
}

func (n *Node) Status() Status {
	n.mu.Lock()
	defer n.mu.Unlock()
	li, _ := n.lastLog()
	return Status{ID: n.cfg.ID, Role: n.role, Term: n.term, Leader: n.leader, CommitIndex: n.commitIndex,
		LastIndex: li, Isolated: n.isolatedLocked(), Elections: n.elections}
}
