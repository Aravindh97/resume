// Package live pushes order changes to browsers. Changes are announced with
// Postgres NOTIFY when their transaction commits; every server instance
// LISTENs and forwards each notice to its connected users who may see it.
// Because the notice comes from the database, it works with any number of
// instances, and a change is never announced if its transaction rolls back.
package live

import (
	"context"
	"encoding/json"
	"log/slog"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Aravindh97/thari/internal/domain"
	"github.com/Aravindh97/thari/internal/store"
)

type Hub struct {
	mu   sync.Mutex
	subs map[*Sub]struct{}
	log  *slog.Logger
}

// Sub is one browser connection. C is buffered; a browser too slow to keep
// up misses notices rather than holding up others, and refetches on the
// next one it gets.
type Sub struct {
	C chan store.Notice
	p domain.Principal
}

func New(log *slog.Logger) *Hub { return &Hub{subs: map[*Sub]struct{}{}, log: log} }

func (h *Hub) Subscribe(p domain.Principal) *Sub {
	s := &Sub{C: make(chan store.Notice, 32), p: p}
	h.mu.Lock()
	h.subs[s] = struct{}{}
	h.mu.Unlock()
	return s
}

func (h *Hub) Unsubscribe(s *Sub) {
	h.mu.Lock()
	delete(h.subs, s)
	h.mu.Unlock()
}

func (h *Hub) Count() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.subs)
}

// visible applies the same rules as the store: factory staff see their
// factory's changes; a buyer sees only their own orders' buyer-visible ones.
func visible(p domain.Principal, n store.Notice) bool {
	if !p.Can(domain.ViewOrder, n.FactoryID, n.BuyerID) {
		return false
	}
	return n.BuyerVisible || p.Can(domain.ViewInternal, n.FactoryID, n.BuyerID)
}

func (h *Hub) Publish(n store.Notice) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for s := range h.subs {
		if !visible(s.p, n) {
			continue
		}
		select {
		case s.C <- n:
		default:
		}
	}
}

// Listen runs until ctx is done, reconnecting with backoff if the
// connection to Postgres drops.
func (h *Hub) Listen(ctx context.Context, db *pgxpool.Pool) {
	wait := time.Second
	for ctx.Err() == nil {
		err := h.listenOnce(ctx, db)
		if ctx.Err() != nil {
			return
		}
		h.log.Warn("listen", "err", err, "retry_in", wait)
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
		wait = min(wait*2, 30*time.Second)
	}
}

func (h *Hub) listenOnce(ctx context.Context, db *pgxpool.Pool) error {
	c, err := db.Acquire(ctx)
	if err != nil {
		return err
	}
	defer c.Release()
	if _, err := c.Exec(ctx, "listen thari"); err != nil {
		return err
	}
	for {
		n, err := c.Conn().WaitForNotification(ctx)
		if err != nil {
			return err
		}
		var notice store.Notice
		if json.Unmarshal([]byte(n.Payload), &notice) == nil {
			h.Publish(notice)
		}
	}
}
