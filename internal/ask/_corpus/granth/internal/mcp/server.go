// Package mcp serves Granth's retrieval tools over the Model Context Protocol
// (JSON-RPC 2.0, newline-delimited, over stdio).
package mcp

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/Aravindh97/granth/internal/doc"
	"github.com/Aravindh97/granth/internal/index"
	"github.com/Aravindh97/granth/internal/search"
)

const ProtocolVersion = "2025-06-18"

type Server struct {
	Searcher *search.Searcher
	Index    index.Index
	// DefaultMaxTokens caps search output when the caller does not set it.
	DefaultMaxTokens int
}

type rpcReq struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type rpcErr struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type rpcResp struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  any             `json:"result,omitempty"`
	Error   *rpcErr         `json:"error,omitempty"`
}

func schema(props map[string]any, required ...string) map[string]any {
	return map[string]any{"type": "object", "properties": props, "required": required, "additionalProperties": false}
}

var tools = []map[string]any{
	{
		"name": "search",
		"description": "Search the enterprise knowledge base. Returns the best matching passages, each with a " +
			"citation (source, page, section). Cite the source when you use a passage.",
		"inputSchema": schema(map[string]any{
			"query":       map[string]any{"type": "string", "description": "Natural-language question or keywords"},
			"k":           map[string]any{"type": "integer", "minimum": 1, "maximum": 20, "default": 5},
			"doc_ids":     map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
			"metadata":    map[string]any{"type": "object", "additionalProperties": map[string]any{"type": "string"}},
			"tables_only": map[string]any{"type": "boolean"},
			"max_tokens":  map[string]any{"type": "integer", "minimum": 50, "maximum": 8000},
		}, "query"),
	},
	{
		"name":        "get_chunk",
		"description": "Return the full text and citation of one passage by chunk_id.",
		"inputSchema": schema(map[string]any{"chunk_id": map[string]any{"type": "string"}}, "chunk_id"),
	},
	{
		"name":        "open_source",
		"description": "Read the passages around a chunk in its source document, to check context before answering.",
		"inputSchema": schema(map[string]any{
			"chunk_id": map[string]any{"type": "string"},
			"window":   map[string]any{"type": "integer", "minimum": 0, "maximum": 5, "default": 1},
		}, "chunk_id"),
	},
}

// Serve reads requests from r and writes responses to w until r is closed.
func (s *Server) Serve(ctx context.Context, r io.Reader, w io.Writer) error {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 1<<20), 16<<20)
	enc := json.NewEncoder(w)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		var req rpcReq
		if err := json.Unmarshal([]byte(line), &req); err != nil {
			_ = enc.Encode(rpcResp{JSONRPC: "2.0", ID: json.RawMessage("null"), Error: &rpcErr{-32700, "parse error"}})
			continue
		}
		if len(req.ID) == 0 { // notification: no response
			continue
		}
		res, rerr := s.handle(ctx, req)
		resp := rpcResp{JSONRPC: "2.0", ID: req.ID, Result: res, Error: rerr}
		if err := enc.Encode(resp); err != nil {
			return err
		}
	}
	return sc.Err()
}

func (s *Server) handle(ctx context.Context, req rpcReq) (any, *rpcErr) {
	switch req.Method {
	case "initialize":
		return map[string]any{
			"protocolVersion": ProtocolVersion,
			"capabilities":    map[string]any{"tools": map[string]any{}},
			"serverInfo":      map[string]any{"name": "granth", "version": "0.1.0"},
		}, nil
	case "ping":
		return map[string]any{}, nil
	case "tools/list":
		return map[string]any{"tools": tools}, nil
	case "tools/call":
		var p struct {
			Name      string          `json:"name"`
			Arguments json.RawMessage `json:"arguments"`
		}
		if err := json.Unmarshal(req.Params, &p); err != nil {
			return nil, &rpcErr{-32602, "invalid params"}
		}
		out, err := s.call(ctx, p.Name, p.Arguments)
		if errors.Is(err, errUnknownTool) {
			return nil, &rpcErr{-32602, err.Error()}
		}
		if err != nil {
			// Tool errors are returned as results so the model can see and recover.
			return map[string]any{"isError": true, "content": []any{map[string]any{"type": "text", "text": err.Error()}}}, nil
		}
		b, _ := json.MarshalIndent(out, "", " ")
		return map[string]any{"content": []any{map[string]any{"type": "text", "text": string(b)}}, "structuredContent": out}, nil
	}
	return nil, &rpcErr{-32601, "method not found: " + req.Method}
}

var errUnknownTool = errors.New("unknown tool")

func (s *Server) call(ctx context.Context, name string, args json.RawMessage) (any, error) {
	switch name {
	case "search":
		var a struct {
			Query      string            `json:"query"`
			K          int               `json:"k"`
			DocIDs     []string          `json:"doc_ids"`
			Metadata   map[string]string `json:"metadata"`
			TablesOnly bool              `json:"tables_only"`
			MaxTokens  int               `json:"max_tokens"`
		}
		if err := json.Unmarshal(args, &a); err != nil {
			return nil, fmt.Errorf("bad arguments: %w", err)
		}
		if strings.TrimSpace(a.Query) == "" {
			return nil, errors.New("query is required")
		}
		a.K = min(max(a.K, 1), 20)
		if a.MaxTokens == 0 {
			a.MaxTokens = s.DefaultMaxTokens
		}
		return s.Searcher.Search(ctx, search.Request{Query: a.Query, K: a.K, MaxTokens: a.MaxTokens,
			Filter: index.Filter{DocIDs: a.DocIDs, Metadata: a.Metadata, TablesOnly: a.TablesOnly}})
	case "get_chunk":
		var a struct {
			ChunkID string `json:"chunk_id"`
		}
		if err := json.Unmarshal(args, &a); err != nil {
			return nil, fmt.Errorf("bad arguments: %w", err)
		}
		c, err := s.Index.Get(ctx, a.ChunkID)
		if err != nil {
			return nil, fmt.Errorf("chunk %q: %w", a.ChunkID, err)
		}
		return passage(c), nil
	case "open_source":
		var a struct {
			ChunkID string `json:"chunk_id"`
			Window  *int   `json:"window"`
		}
		if err := json.Unmarshal(args, &a); err != nil {
			return nil, fmt.Errorf("bad arguments: %w", err)
		}
		w := 1
		if a.Window != nil {
			w = min(max(*a.Window, 0), 5)
		}
		c, err := s.Index.Get(ctx, a.ChunkID)
		if err != nil {
			return nil, fmt.Errorf("chunk %q: %w", a.ChunkID, err)
		}
		// Chunk ids are deterministic in (doc, version, ordinal), so the
		// neighbours can be addressed directly without a search.
		var out []map[string]any
		for o := c.Ordinal - w; o <= c.Ordinal+w; o++ {
			if o < 0 {
				continue
			}
			n, err := s.Index.Get(ctx, doc.ChunkID(c.DocID, c.DocHash, o))
			if err != nil {
				continue
			}
			out = append(out, passage(n))
		}
		return map[string]any{"passages": out}, nil
	}
	return nil, fmt.Errorf("%w %q", errUnknownTool, name)
}

func passage(c *doc.Chunk) map[string]any {
	return map[string]any{"chunk_id": c.ID, "ordinal": c.Ordinal, "text": c.Text, "citation": c.Citation}
}
