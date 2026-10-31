// Package embed provides the Embedder interface and an offline hashing
// embedder (feature hashing of words and character trigrams). It captures
// lexical and sub-word overlap, not meaning; production deployments plug in a
// real embedding model behind the same interface.
package embed

import (
	"context"
	"fmt"
	"hash/fnv"
	"math"

	"github.com/Aravindh97/smriti/internal/textproc"
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
