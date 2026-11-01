// Package browsermesh runs four Raft nodes in one process over an in-memory
// network. On the GitHub Pages build it runs inside the visitor's browser
// tab, compiled to WebAssembly: the consensus code is the same
// internal/raft used by the server mesh, only the network is simulated.
package browsermesh

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/Aravindh97/resume/internal/raft"
)

type NodeStatus struct {
	raft.Status
	Region  string `json:"region"`
	Version int    `json:"version"`
	Up      bool   `json:"up"`
}

type Mesh struct {
	nodes    []*raft.Node
	regions  map[string]string
	isoFor   time.Duration
	mu       sync.Mutex
	versions map[string]int
}

var Regions = []string{"Mumbai", "Bengaluru", "Singapore", "Frankfurt"}

func New(ctx context.Context, isolateFor time.Duration) *Mesh {
	m := &Mesh{regions: map[string]string{}, isoFor: isolateFor, versions: map[string]int{}}
	net := raft.NewMemNetwork()
	ids := make([]string, len(Regions))
	for i := range ids {
		ids[i] = fmt.Sprintf("node-%d", i+1)
	}
	for i, id := range ids {
		var peers []string
		for _, p := range ids {
			if p != id {
				peers = append(peers, p)
			}
		}
		id := id
		n := raft.New(raft.Config{ID: id, Peers: peers, Transport: net.Transport(id),
			ElectionTimeout: 500 * time.Millisecond, HeartbeatTimeout: 100 * time.Millisecond,
			Apply: func(int, raft.Entry) {
				m.mu.Lock()
				m.versions[id]++
				m.mu.Unlock()
			}})
		net.Add(n)
		m.nodes = append(m.nodes, n)
		m.regions[id] = Regions[i]
	}
	for _, n := range m.nodes {
		go n.Run(ctx)
	}
	return m
}

func (m *Mesh) Status() []NodeStatus {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]NodeStatus, 0, len(m.nodes))
	for _, n := range m.nodes {
		s := n.Status()
		out = append(out, NodeStatus{Status: s, Region: m.regions[s.ID], Version: m.versions[s.ID], Up: true})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

var ErrQuorum = errors.New("a node is already isolated; wait for it to rejoin so the cluster keeps a majority")

// Isolate partitions one node ("leader" or a node id). It refuses if that
// would leave fewer than a majority connected.
func (m *Mesh) Isolate(target string) (string, error) {
	st := m.Status()
	down := 0
	for _, s := range st {
		if s.Isolated {
			down++
		}
		if target == "leader" && s.Role == raft.Leader && !s.Isolated {
			target = s.ID
		}
	}
	if down >= (len(st)-1)/2 {
		return "", ErrQuorum
	}
	for _, n := range m.nodes {
		if n.Status().ID == target {
			n.Isolate(m.isoFor)
			return target, nil
		}
	}
	return "", fmt.Errorf("no node %q (no leader yet?)", target)
}

// Write replicates one update through the current leader.
func (m *Mesh) Write(ctx context.Context) (string, error) {
	for _, n := range m.nodes {
		if s := n.Status(); s.Role == raft.Leader && !s.Isolated {
			b, _ := json.Marshal(map[string]string{"op": "publish", "at": time.Now().UTC().Format(time.RFC3339)})
			c, cancel := context.WithTimeout(ctx, 2*time.Second)
			defer cancel()
			if _, err := n.Propose(c, b); err != nil {
				return "", err
			}
			return s.ID, nil
		}
	}
	return "", errors.New("no leader right now (election in progress)")
}
