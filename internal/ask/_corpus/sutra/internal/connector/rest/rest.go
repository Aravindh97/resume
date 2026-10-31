// Package rest syncs records from a CRM-style REST API incrementally.
//
// The API is read with keyset pagination on (updated_at, id): each request
// asks for records changed after the last (updated_at, id) seen. That
// position is the checkpoint, written in the same warehouse transaction as
// the page it ends, so a crash resumes at the next unprocessed record.
// Keyset pagination (unlike offset pages) does not skip or repeat records
// when other records change during the sync.
package rest

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/rand/v2"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/Aravindh97/sutra/internal/ontology"
	"github.com/Aravindh97/sutra/internal/warehouse"
)

type Connector struct {
	BaseURL string
	Token   string // bearer token, from the environment
	Source  ontology.Source
	WH      *warehouse.DB
	HTTP    *http.Client
	Limit   int
	Log     *slog.Logger
	// MaxAttempts per request for retryable failures (429, 5xx, network).
	MaxAttempts int
	Sleep       func(context.Context, time.Duration) error
}

type Checkpoint struct {
	Since   time.Time `json:"since"`
	AfterID string    `json:"after_id"`
}

var ErrAuth = errors.New("authentication rejected by source")

type page struct {
	Data    []map[string]any `json:"data"`
	HasMore bool             `json:"has_more"`
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// Sync reads every change since the checkpoint and returns the number of
// records applied.
func (c *Connector) Sync(ctx context.Context) (int, error) {
	if c.HTTP == nil {
		c.HTTP = &http.Client{Timeout: 30 * time.Second}
	}
	if c.Limit <= 0 {
		c.Limit = 200
	}
	if c.MaxAttempts <= 0 {
		c.MaxAttempts = 6
	}
	if c.Sleep == nil {
		c.Sleep = sleepCtx
	}
	log := c.Log
	if log == nil {
		log = slog.Default()
	}
	var cp Checkpoint
	if s, err := c.WH.Checkpoint(ctx, c.Source.Name); err != nil {
		return 0, err
	} else if s != "" {
		if err := json.Unmarshal([]byte(s), &cp); err != nil {
			return 0, fmt.Errorf("bad checkpoint: %w", err)
		}
	}
	total := 0
	for {
		p, err := c.fetch(ctx, cp)
		if err != nil {
			return total, err
		}
		batch := make([]warehouse.Versioned, 0, len(p.Data))
		for _, raw := range p.Data {
			row := map[string]string{}
			for k, v := range raw {
				if v != nil {
					row[k] = fmt.Sprint(v)
				}
			}
			r, err := c.Source.MapRow(row)
			if err != nil {
				return total, err
			}
			upd, err := time.Parse(time.RFC3339Nano, row["updated_at"])
			if err != nil {
				return total, fmt.Errorf("record %s: bad updated_at %q", r.ID, row["updated_at"])
			}
			r.UpdatedAt = upd
			r.Deleted = row["deleted"] == "true"
			r.Position = upd.Format(time.RFC3339Nano) + "/" + r.ID
			batch = append(batch, warehouse.Versioned{Record: r, Seq: uint64(upd.UnixNano())})
			cp = Checkpoint{Since: upd, AfterID: r.ID}
		}
		b, _ := json.Marshal(cp)
		if _, err := c.WH.Apply(ctx, c.Source.Name, string(b), batch); err != nil {
			return total, err
		}
		total += len(batch)
		log.Debug("page applied", "source", c.Source.Name, "records", len(batch), "checkpoint", string(b))
		if !p.HasMore || len(p.Data) == 0 {
			return total, nil
		}
	}
}

func (c *Connector) fetch(ctx context.Context, cp Checkpoint) (*page, error) {
	q := url.Values{"limit": {strconv.Itoa(c.Limit)}}
	if !cp.Since.IsZero() {
		q.Set("since", cp.Since.Format(time.RFC3339Nano))
		q.Set("after_id", cp.AfterID)
	}
	u := strings.TrimRight(c.BaseURL, "/") + c.Source.Path + "?" + q.Encode()
	var lastErr error
	for attempt := 1; attempt <= c.MaxAttempts; attempt++ {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
		if err != nil {
			return nil, err
		}
		req.Header.Set("Authorization", "Bearer "+c.Token)
		req.Header.Set("Accept", "application/json")
		resp, err := c.HTTP.Do(req)
		var wait time.Duration
		switch {
		case err != nil:
			lastErr = err
		case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
			resp.Body.Close()
			return nil, fmt.Errorf("%w: HTTP %d", ErrAuth, resp.StatusCode)
		case resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500:
			lastErr = fmt.Errorf("HTTP %d", resp.StatusCode)
			if s, err := strconv.Atoi(resp.Header.Get("Retry-After")); err == nil {
				wait = min(time.Duration(s)*time.Second, time.Minute)
			}
			resp.Body.Close()
		case resp.StatusCode >= 300:
			b, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
			resp.Body.Close()
			return nil, fmt.Errorf("source returned HTTP %d: %s", resp.StatusCode, b)
		default:
			var p page
			err := json.NewDecoder(io.LimitReader(resp.Body, 64<<20)).Decode(&p)
			resp.Body.Close()
			if err != nil {
				return nil, fmt.Errorf("decode page: %w", err)
			}
			return &p, nil
		}
		if attempt == c.MaxAttempts {
			break
		}
		if wait == 0 {
			d := min(30*time.Second, 250*time.Millisecond<<(attempt-1))
			wait = d/2 + time.Duration(rand.Int64N(int64(d/2)+1))
		}
		if err := c.Sleep(ctx, wait); err != nil {
			return nil, err
		}
	}
	return nil, fmt.Errorf("fetch %s after %d attempts: %w", c.Source.Path, c.MaxAttempts, lastErr)
}
