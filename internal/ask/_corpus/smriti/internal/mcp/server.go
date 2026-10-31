// Package mcp exposes Smriti to agents over MCP (JSON-RPC 2.0 on stdio).
//
// The principal (org, user, teams) is fixed when the server starts, from the
// launching application's trusted configuration. Tools never accept a user
// or team id as an argument, so a model cannot read or write another user's
// memories by asking.
package mcp

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/Aravindh97/smriti/internal/extract"
	"github.com/Aravindh97/smriti/internal/memory"
	"github.com/Aravindh97/smriti/internal/store"
)

type Server struct {
	Store     *store.Store
	Extractor extract.Extractor
	Principal memory.Principal
	Team      string // team context for "our team ..." statements
	Now       func() time.Time
}

func obj(props map[string]any, req ...string) map[string]any {
	return map[string]any{"type": "object", "properties": props, "required": req, "additionalProperties": false}
}

var tools = []map[string]any{
	{"name": "remember", "description": "Store durable facts from what the user just said (preferences, role, team conventions). " +
		"Pass the user's own words; Smriti extracts and reconciles facts and returns what changed.",
		"inputSchema": obj(map[string]any{"text": map[string]any{"type": "string", "maxLength": 4000}}, "text")},
	{"name": "recall", "description": "Retrieve what is known about the user, their team and the organisation that is relevant to a query.",
		"inputSchema": obj(map[string]any{
			"query": map[string]any{"type": "string"},
			"k":     map[string]any{"type": "integer", "minimum": 1, "maximum": 20},
			"as_of": map[string]any{"type": "string", "format": "date-time", "description": "what was true at this time"},
		}, "query")},
	{"name": "history", "description": "Every recorded value of one subject over time, with sources.",
		"inputSchema": obj(map[string]any{"subject": map[string]any{"type": "string"}}, "subject")},
	{"name": "forget", "description": "Permanently delete one of the user's own memories (all versions) when they ask you to.",
		"inputSchema": obj(map[string]any{"memory_id": map[string]any{"type": "string"}}, "memory_id")},
}

func (s *Server) Serve(ctx context.Context, r io.Reader, w io.Writer) error {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 1<<20), 8<<20)
	enc := json.NewEncoder(w)
	for sc.Scan() {
		var req struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
			Params json.RawMessage `json:"params"`
		}
		if json.Unmarshal(sc.Bytes(), &req) != nil {
			enc.Encode(map[string]any{"jsonrpc": "2.0", "id": nil, "error": map[string]any{"code": -32700, "message": "parse error"}})
			continue
		}
		if len(req.ID) == 0 {
			continue
		}
		resp := map[string]any{"jsonrpc": "2.0", "id": req.ID}
		switch req.Method {
		case "initialize":
			resp["result"] = map[string]any{"protocolVersion": "2025-06-18", "capabilities": map[string]any{"tools": map[string]any{}},
				"serverInfo": map[string]any{"name": "smriti", "version": "0.1.0"}}
		case "ping":
			resp["result"] = map[string]any{}
		case "tools/list":
			resp["result"] = map[string]any{"tools": tools}
		case "tools/call":
			var p struct {
				Name      string          `json:"name"`
				Arguments json.RawMessage `json:"arguments"`
			}
			if json.Unmarshal(req.Params, &p) != nil {
				resp["error"] = map[string]any{"code": -32602, "message": "invalid params"}
				break
			}
			out, err := s.call(ctx, p.Name, p.Arguments)
			if err != nil {
				resp["result"] = map[string]any{"isError": true, "content": []any{map[string]any{"type": "text", "text": err.Error()}}}
				break
			}
			b, _ := json.Marshal(out)
			resp["result"] = map[string]any{"content": []any{map[string]any{"type": "text", "text": string(b)}}, "structuredContent": out}
		default:
			resp["error"] = map[string]any{"code": -32601, "message": "method not found"}
		}
		if err := enc.Encode(resp); err != nil {
			return err
		}
	}
	return sc.Err()
}

func (s *Server) call(ctx context.Context, name string, raw json.RawMessage) (any, error) {
	var a struct {
		Text     string `json:"text"`
		Query    string `json:"query"`
		K        int    `json:"k"`
		AsOf     string `json:"as_of"`
		Subject  string `json:"subject"`
		MemoryID string `json:"memory_id"`
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields() // e.g. a model trying to pass user_id
	if len(raw) > 0 {
		if err := dec.Decode(&a); err != nil {
			return nil, fmt.Errorf("bad arguments: %w", err)
		}
	}
	now := time.Now
	if s.Now != nil {
		now = s.Now
	}
	switch name {
	case "remember":
		if strings.TrimSpace(a.Text) == "" {
			return nil, fmt.Errorf("text is required")
		}
		t := now()
		conv := memory.Conversation{ID: fmt.Sprintf("mcp-%d", t.UnixNano()),
			Messages: []memory.Message{{ID: "0", Role: "user", Text: a.Text, At: t}}}
		cands, err := s.Extractor.Extract(ctx, conv, extract.Context{Team: s.Team})
		if err != nil {
			return nil, err
		}
		res, err := s.Store.Write(ctx, s.Principal, cands)
		return map[string]any{"results": res}, err
	case "recall":
		o := store.RecallOptions{K: min(max(a.K, 1), 20)}
		if a.K == 0 {
			o.K = 5
		}
		if a.AsOf != "" {
			t, err := time.Parse(time.RFC3339, a.AsOf)
			if err != nil {
				return nil, fmt.Errorf("as_of: %w", err)
			}
			o.AsOf = &t
		}
		res, err := s.Store.Recall(ctx, s.Principal, a.Query, o)
		return map[string]any{"memories": res}, err
	case "history":
		res, err := s.Store.History(ctx, s.Principal, a.Subject)
		return map[string]any{"versions": res}, err
	case "forget":
		n, err := s.Store.Forget(ctx, s.Principal, a.MemoryID)
		return map[string]any{"deleted": n}, err
	}
	return nil, fmt.Errorf("unknown tool %q", name)
}
