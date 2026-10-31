package index

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/Aravindh97/granth/internal/doc"
)

// OpenSearch stores chunks in one index with a multi-field text mapping
// (standard, English and Hindi analyzers; the standard tokenizer follows
// Unicode word boundaries, so Tamil text is tokenized correctly too) and a
// Lucene HNSW knn_vector field, which supports filtered k-NN.
type OpenSearch struct {
	URL, Index, User, Password string
	Dim                        int
	HTTP                       *http.Client
	// Refresh controls the refresh parameter on writes. "wait_for" makes
	// writes visible to the next search (tests, evals); "" is fastest.
	Refresh string
}

type OpenSearchConfig struct {
	URL, Index, User, Password string
	Dim                        int
	CACertFile                 string // PEM bundle that signed the cluster cert
	InsecureSkipVerify         bool   // local dev with self-signed certs only
}

func NewOpenSearch(c OpenSearchConfig) (*OpenSearch, error) {
	tlsCfg := &tls.Config{MinVersion: tls.VersionTLS12}
	if c.CACertFile != "" {
		pem, err := os.ReadFile(c.CACertFile)
		if err != nil {
			return nil, err
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(pem) {
			return nil, fmt.Errorf("no certificates in %s", c.CACertFile)
		}
		tlsCfg.RootCAs = pool
	}
	tlsCfg.InsecureSkipVerify = c.InsecureSkipVerify //nolint:gosec // explicit opt-in for local dev
	return &OpenSearch{URL: strings.TrimRight(c.URL, "/"), Index: c.Index, User: c.User, Password: c.Password,
		Dim: c.Dim, HTTP: &http.Client{Timeout: 30 * time.Second, Transport: &http.Transport{TLSClientConfig: tlsCfg}}}, nil
}

func (o *OpenSearch) do(ctx context.Context, method, path string, body io.Reader, ctype string, out any) error {
	req, err := http.NewRequestWithContext(ctx, method, o.URL+path, body)
	if err != nil {
		return err
	}
	if ctype != "" {
		req.Header.Set("Content-Type", ctype)
	}
	if o.User != "" {
		req.SetBasicAuth(o.User, o.Password)
	}
	resp, err := o.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 256<<20))
	if resp.StatusCode == http.StatusNotFound && method == http.MethodGet {
		return ErrNotFound
	}
	if resp.StatusCode >= 300 {
		return fmt.Errorf("opensearch %s %s: HTTP %d: %.500s", method, path, resp.StatusCode, b)
	}
	if out != nil {
		return json.Unmarshal(b, out)
	}
	return nil
}

func (o *OpenSearch) jsonDo(ctx context.Context, method, path string, body, out any) error {
	var r io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		r = bytes.NewReader(b)
	}
	return o.do(ctx, method, path, r, "application/json", out)
}

// EnsureIndex creates the index if it does not exist.
func (o *OpenSearch) EnsureIndex(ctx context.Context) error {
	err := o.do(ctx, http.MethodHead, "/"+o.Index, nil, "", nil)
	if err == nil {
		return nil
	}
	mapping := map[string]any{
		"settings": map[string]any{"index": map[string]any{"knn": true, "number_of_replicas": 0}},
		"mappings": map[string]any{
			"dynamic_templates": []any{map[string]any{"metadata_keywords": map[string]any{
				"path_match": "metadata.*", "mapping": map[string]any{"type": "keyword"}}}},
			"properties": map[string]any{
				"doc_id":   map[string]any{"type": "keyword"},
				"doc_hash": map[string]any{"type": "keyword"},
				"ordinal":  map[string]any{"type": "integer"},
				"is_table": map[string]any{"type": "boolean"},
				"text": map[string]any{"type": "text", "analyzer": "standard", "fields": map[string]any{
					"en": map[string]any{"type": "text", "analyzer": "english"},
					"hi": map[string]any{"type": "text", "analyzer": "hindi"}}},
				"citation": map[string]any{"type": "object", "enabled": false},
				"embedding": map[string]any{"type": "knn_vector", "dimension": o.Dim, "method": map[string]any{
					"name": "hnsw", "engine": "lucene", "space_type": "cosinesimil",
					"parameters": map[string]any{"m": 16, "ef_construction": 128}}},
			},
		},
	}
	return o.jsonDo(ctx, http.MethodPut, "/"+o.Index, mapping, nil)
}

type osDoc struct {
	DocID     string            `json:"doc_id"`
	DocHash   string            `json:"doc_hash"`
	Ordinal   int               `json:"ordinal"`
	IsTable   bool              `json:"is_table"`
	Text      string            `json:"text"`
	Citation  doc.Citation      `json:"citation"`
	Metadata  map[string]string `json:"metadata,omitempty"`
	Embedding []float32         `json:"embedding,omitempty"`
}

func fromOS(id string, d osDoc) doc.Chunk {
	return doc.Chunk{ID: id, DocID: d.DocID, DocHash: d.DocHash, Ordinal: d.Ordinal, Text: d.Text,
		IsTable: d.IsTable, Citation: d.Citation, Metadata: d.Metadata, Embedding: d.Embedding}
}

func (o *OpenSearch) refreshQS(prefix string) string {
	if o.Refresh == "" {
		return ""
	}
	return prefix + "refresh=" + o.Refresh
}

func (o *OpenSearch) ReplaceDoc(ctx context.Context, docID, docHash string, chunks []doc.Chunk) error {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	for _, c := range chunks {
		_ = enc.Encode(map[string]any{"index": map[string]any{"_index": o.Index, "_id": c.ID}})
		if err := enc.Encode(osDoc{c.DocID, c.DocHash, c.Ordinal, c.IsTable, c.Text, c.Citation, c.Metadata, c.Embedding}); err != nil {
			return err
		}
	}
	if len(chunks) > 0 {
		var r struct {
			Errors bool `json:"errors"`
			Items  []map[string]struct {
				Status int             `json:"status"`
				Error  json.RawMessage `json:"error"`
			} `json:"items"`
		}
		if err := o.do(ctx, http.MethodPost, "/_bulk"+o.refreshQS("?"), &buf, "application/x-ndjson", &r); err != nil {
			return err
		}
		if r.Errors {
			for _, it := range r.Items {
				for _, v := range it {
					if v.Status >= 300 {
						return fmt.Errorf("opensearch bulk item failed: %d %s", v.Status, v.Error)
					}
				}
			}
		}
	}
	q := map[string]any{"query": map[string]any{"bool": map[string]any{
		"filter":   []any{map[string]any{"term": map[string]any{"doc_id": docID}}},
		"must_not": []any{map[string]any{"term": map[string]any{"doc_hash": docHash}}}}}}
	path := "/" + o.Index + "/_delete_by_query?conflicts=proceed" + o.refreshQS("&")
	path = strings.Replace(path, "refresh=wait_for", "refresh=true", 1) // delete_by_query takes a bool
	return o.jsonDo(ctx, http.MethodPost, path, q, nil)
}

func (o *OpenSearch) DeleteDoc(ctx context.Context, docID string) error {
	q := map[string]any{"query": map[string]any{"term": map[string]any{"doc_id": docID}}}
	return o.jsonDo(ctx, http.MethodPost, "/"+o.Index+"/_delete_by_query?conflicts=proceed&refresh=true", q, nil)
}

func (o *OpenSearch) Get(ctx context.Context, id string) (*doc.Chunk, error) {
	var r struct {
		Found  bool  `json:"found"`
		Source osDoc `json:"_source"`
	}
	if err := o.jsonDo(ctx, http.MethodGet, "/"+o.Index+"/_doc/"+id, nil, &r); err != nil {
		return nil, err
	}
	if !r.Found {
		return nil, ErrNotFound
	}
	c := fromOS(id, r.Source)
	return &c, nil
}

func filterClauses(f Filter) []any {
	var cl []any
	if len(f.DocIDs) > 0 {
		cl = append(cl, map[string]any{"terms": map[string]any{"doc_id": f.DocIDs}})
	}
	for k, v := range f.Metadata {
		cl = append(cl, map[string]any{"term": map[string]any{"metadata." + k: v}})
	}
	if f.TablesOnly {
		cl = append(cl, map[string]any{"term": map[string]any{"is_table": true}})
	}
	return cl
}

func (o *OpenSearch) search(ctx context.Context, body map[string]any) ([]Hit, error) {
	body["_source"] = map[string]any{"excludes": []string{"embedding"}}
	var r struct {
		Hits struct {
			Hits []struct {
				ID     string  `json:"_id"`
				Score  float64 `json:"_score"`
				Source osDoc   `json:"_source"`
			} `json:"hits"`
		} `json:"hits"`
	}
	if err := o.jsonDo(ctx, http.MethodPost, "/"+o.Index+"/_search", body, &r); err != nil {
		return nil, err
	}
	out := make([]Hit, 0, len(r.Hits.Hits))
	for _, h := range r.Hits.Hits {
		out = append(out, Hit{Chunk: fromOS(h.ID, h.Source), Score: h.Score})
	}
	return out, nil
}

func (o *OpenSearch) Lexical(ctx context.Context, query string, f Filter, k int) ([]Hit, error) {
	return o.search(ctx, map[string]any{"size": k, "query": map[string]any{"bool": map[string]any{
		"must": map[string]any{"multi_match": map[string]any{"query": query,
			"fields": []string{"text", "text.en", "text.hi"}, "type": "most_fields"}},
		"filter": filterClauses(f)}}})
}

func (o *OpenSearch) Vector(ctx context.Context, vec []float32, f Filter, k int) ([]Hit, error) {
	knn := map[string]any{"vector": vec, "k": k}
	if cl := filterClauses(f); len(cl) > 0 {
		knn["filter"] = map[string]any{"bool": map[string]any{"filter": cl}}
	}
	return o.search(ctx, map[string]any{"size": k, "query": map[string]any{"knn": map[string]any{"embedding": knn}}})
}
