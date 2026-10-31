// Package mcp exposes resolved entities to agents over MCP (stdio). All tools
// are read-only. Every returned value can be traced to a source record via
// get_entity's lineage.
package mcp

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"sort"

	"github.com/Aravindh97/sutra/internal/normalize"
	"github.com/Aravindh97/sutra/internal/ontology"
	"github.com/Aravindh97/sutra/internal/warehouse"
)

type Server struct {
	WH       *warehouse.DB
	Ontology *ontology.Ontology
}

func obj(p map[string]any, req ...string) map[string]any {
	return map[string]any{"type": "object", "properties": p, "required": req, "additionalProperties": false}
}

func (s *Server) tools() []map[string]any {
	var types []string
	for t := range s.Ontology.Entities {
		types = append(types, t)
	}
	sort.Strings(types)
	return []map[string]any{
		{"name": "describe_ontology", "description": "List entity types, their attributes and the source systems that feed them.",
			"inputSchema": obj(map[string]any{})},
		{"name": "find_entities", "description": "Find resolved entities by an attribute. Identifiers (gstin, pan, phone) are normalised " +
			"before matching; name does a case-insensitive contains match. Returns golden records.",
			"inputSchema": obj(map[string]any{
				"type":      map[string]any{"type": "string", "enum": types},
				"attribute": map[string]any{"type": "string"},
				"value":     map[string]any{"type": "string", "minLength": 2},
				"limit":     map[string]any{"type": "integer", "minimum": 1, "maximum": 50},
			}, "type", "attribute", "value")},
		{"name": "get_entity", "description": "One entity with its golden record, the source record behind every value (lineage), " +
			"all member source records and the evidence that linked them. Cite lineage when you state a value.",
			"inputSchema": obj(map[string]any{"entity_id": map[string]any{"type": "string"}}, "entity_id")},
	}
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
				"serverInfo": map[string]any{"name": "sutra", "version": "0.1.0"}}
		case "ping":
			resp["result"] = map[string]any{}
		case "tools/list":
			resp["result"] = map[string]any{"tools": s.tools()}
		case "tools/call":
			var p struct {
				Name      string          `json:"name"`
				Arguments json.RawMessage `json:"arguments"`
			}
			json.Unmarshal(req.Params, &p)
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
		Type      string `json:"type"`
		Attribute string `json:"attribute"`
		Value     string `json:"value"`
		Limit     int    `json:"limit"`
		EntityID  string `json:"entity_id"`
	}
	if len(raw) > 0 {
		dec := json.NewDecoder(bytes.NewReader(raw))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&a); err != nil {
			return nil, fmt.Errorf("bad arguments: %w", err)
		}
	}
	switch name {
	case "describe_ontology":
		return s.Ontology, nil
	case "find_entities":
		e, ok := s.Ontology.Entities[a.Type]
		if !ok {
			return nil, fmt.Errorf("unknown entity type %q", a.Type)
		}
		kind, ok := e.Attributes[a.Attribute]
		if !ok {
			return nil, fmt.Errorf("%s has no attribute %q", a.Type, a.Attribute)
		}
		if len(a.Value) < 2 {
			return nil, fmt.Errorf("value must be at least 2 characters")
		}
		v := a.Value
		switch kind {
		case ontology.GSTIN:
			v, _ = normalize.GSTIN(v)
		case ontology.PAN:
			v, _ = normalize.PAN(v)
		case ontology.Phone:
			if p := normalize.Phone(v); p != "" {
				v = p
			}
		}
		limit := min(max(a.Limit, 1), 50)
		if a.Limit == 0 {
			limit = 10
		}
		res, err := s.WH.Find(ctx, a.Type, a.Attribute, v, limit)
		return map[string]any{"entities": res}, err
	case "get_entity":
		return s.WH.Get(ctx, a.EntityID)
	}
	return nil, fmt.Errorf("unknown tool %q", name)
}
