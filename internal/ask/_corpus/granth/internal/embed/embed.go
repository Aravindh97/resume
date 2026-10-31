// Package embed provides text embedders.
//
// Hashing is a dependency-free embedder (feature hashing of words and
// character trigrams). It captures lexical and sub-word overlap, not meaning,
// and exists so the pipeline, tests and evals run fully offline. HTTP calls any
// OpenAI-compatible /v1/embeddings endpoint (vLLM, TEI, Ollama, OpenAI) and is
// what a real deployment uses.
package embed

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"hash/fnv"
	"io"
	"math"
	"net/http"
	"time"

	"github.com/Aravindh97/granth/internal/resilience"
	"github.com/Aravindh97/granth/internal/textproc"
)

type Embedder interface {
	Embed(ctx context.Context, texts []string) ([][]float32, error)
	Dim() int
	Name() string
}

type Hashing struct{ D int }

func NewHashing(dim int) Hashing { return Hashing{D: dim} }

func (h Hashing) Dim() int     { return h.D }
func (h Hashing) Name() string { return fmt.Sprintf("hashing-%d", h.D) }

func (h Hashing) Embed(_ context.Context, texts []string) ([][]float32, error) {
	out := make([][]float32, len(texts))
	for i, t := range texts {
		out[i] = h.one(t)
	}
	return out, nil
}

func (h Hashing) one(t string) []float32 {
	v := make([]float32, h.D)
	add := func(s string, w float32) {
		f := fnv.New64a()
		f.Write([]byte(s))
		x := f.Sum64()
		sign := float32(1)
		if x>>63 == 1 {
			sign = -1
		}
		v[x%uint64(h.D)] += sign * w
	}
	for _, tok := range textproc.Tokens(t) {
		add("w:"+tok, 1)
		r := []rune("^" + tok + "$")
		for i := 0; i+3 <= len(r); i++ {
			add("c:"+string(r[i:i+3]), 0.5)
		}
	}
	Normalize(v)
	return v
}

func Normalize(v []float32) {
	var n float64
	for _, x := range v {
		n += float64(x) * float64(x)
	}
	if n == 0 {
		return
	}
	inv := float32(1 / math.Sqrt(n))
	for i := range v {
		v[i] *= inv
	}
}

func Cosine(a, b []float32) float32 {
	var s float32
	for i := range a {
		s += a[i] * b[i]
	}
	return s
}

// HTTP is an OpenAI-compatible embeddings client with timeouts, retries and a
// circuit breaker. 4xx responses (other than 429) are permanent errors.
type HTTP struct {
	URL, Model, APIKey string
	D                  int
	Client             *http.Client
	Retry              resilience.Retry
	Breaker            *resilience.Breaker
}

func NewHTTP(url, model, apiKey string, dim int) *HTTP {
	return &HTTP{URL: url, Model: model, APIKey: apiKey, D: dim,
		Client:  &http.Client{Timeout: 30 * time.Second},
		Retry:   resilience.DefaultRetry(),
		Breaker: resilience.NewBreaker(5, 30*time.Second)}
}

func (h *HTTP) Dim() int     { return h.D }
func (h *HTTP) Name() string { return h.Model }

func (h *HTTP) Embed(ctx context.Context, texts []string) ([][]float32, error) {
	body, _ := json.Marshal(map[string]any{"model": h.Model, "input": texts})
	var out [][]float32
	err := h.Retry.Do(ctx, func(ctx context.Context) error {
		return h.Breaker.Do(ctx, func(ctx context.Context) error {
			req, err := http.NewRequestWithContext(ctx, http.MethodPost, h.URL, bytes.NewReader(body))
			if err != nil {
				return resilience.Permanent{Err: err}
			}
			req.Header.Set("Content-Type", "application/json")
			if h.APIKey != "" {
				req.Header.Set("Authorization", "Bearer "+h.APIKey)
			}
			resp, err := h.Client.Do(req)
			if err != nil {
				return err
			}
			defer resp.Body.Close()
			b, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
			if resp.StatusCode >= 400 {
				e := fmt.Errorf("embeddings: HTTP %d: %.200s", resp.StatusCode, b)
				if resp.StatusCode < 500 && resp.StatusCode != http.StatusTooManyRequests {
					return resilience.Permanent{Err: e}
				}
				return e
			}
			var r struct {
				Data []struct {
					Index     int       `json:"index"`
					Embedding []float32 `json:"embedding"`
				} `json:"data"`
			}
			if err := json.Unmarshal(b, &r); err != nil {
				return resilience.Permanent{Err: fmt.Errorf("embeddings: decode: %w", err)}
			}
			if len(r.Data) != len(texts) {
				return fmt.Errorf("embeddings: got %d vectors for %d inputs", len(r.Data), len(texts))
			}
			out = make([][]float32, len(texts))
			for _, d := range r.Data {
				if d.Index < 0 || d.Index >= len(out) {
					return resilience.Permanent{Err: fmt.Errorf("embeddings: bad index %d", d.Index)}
				}
				Normalize(d.Embedding)
				out[d.Index] = d.Embedding
			}
			return nil
		})
	})
	return out, err
}
