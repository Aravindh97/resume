package raft

import (
	"context"
	"errors"
	"sync"
)

// MemNetwork connects nodes in one process. It is used to run the mesh
// inside a browser tab (WebAssembly) and in tests. Isolation works the same
// as over HTTP: an isolated node can neither send nor receive.
type MemNetwork struct {
	mu    sync.RWMutex
	nodes map[string]*Node
}

func NewMemNetwork() *MemNetwork { return &MemNetwork{nodes: map[string]*Node{}} }

func (m *MemNetwork) Add(n *Node) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.nodes[n.cfg.ID] = n
}

// Transport returns the transport a node with this id should use.
func (m *MemNetwork) Transport(from string) Transport { return memNetTransport{m, from} }

var errUnknownPeer = errors.New("unknown peer")

type memNetTransport struct {
	m    *MemNetwork
	from string
}

func (t memNetTransport) peer(id string) (*Node, error) {
	t.m.mu.RLock()
	defer t.m.mu.RUnlock()
	if self, ok := t.m.nodes[t.from]; ok && self.Isolated() {
		return nil, ErrIsolated
	}
	n, ok := t.m.nodes[id]
	if !ok {
		return nil, errUnknownPeer
	}
	return n, nil
}

func (t memNetTransport) RequestVote(_ context.Context, p string, a VoteArgs) (VoteReply, error) {
	n, err := t.peer(p)
	if err != nil {
		return VoteReply{}, err
	}
	return n.HandleVote(a)
}

func (t memNetTransport) AppendEntries(_ context.Context, p string, a AppendArgs) (AppendReply, error) {
	n, err := t.peer(p)
	if err != nil {
		return AppendReply{}, err
	}
	return n.HandleAppend(a)
}
