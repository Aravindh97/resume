// Package ask answers questions about the three projects by retrieving the
// most relevant passages of their source, READMEs and eval reports. It is
// extractive: it returns the passages themselves with file, line range and a
// GitHub link pinned to the snapshot commit. Nothing is generated, so nothing
// can be made up.
package ask

import (
	"embed"
	"fmt"
	"io/fs"
	"math"
	"path"
	"sort"
	"strings"
	"unicode"
)

//go:embed all:_corpus
var corpus embed.FS

type Passage struct {
	Repo      string  `json:"repo"`
	File      string  `json:"file"`
	StartLine int     `json:"start_line"`
	EndLine   int     `json:"end_line"`
	Title     string  `json:"title"`
	Text      string  `json:"text"`
	URL       string  `json:"url"`
	Score     float64 `json:"score"`
}

type Index struct {
	passages []Passage
	toks     [][]string
	df       map[string]int
	avg      float64
}

const github = "https://github.com/Aravindh97"

func Load() (*Index, error) {
	ix := &Index{df: map[string]int{}}
	commits := map[string]string{}
	err := fs.WalkDir(corpus, "_corpus", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		rel := strings.TrimPrefix(p, "_corpus/")
		repo, file, _ := strings.Cut(rel, "/")
		b, err := corpus.ReadFile(p)
		if err != nil {
			return err
		}
		if file == ".commit" {
			commits[repo] = strings.TrimSpace(string(b))
			return nil
		}
		for _, ps := range split(repo, file, string(b)) {
			ix.passages = append(ix.passages, ps)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	total := 0
	for i := range ix.passages {
		p := &ix.passages[i]
		ref := commits[p.Repo]
		if ref == "" {
			ref = "main"
		}
		p.URL = fmt.Sprintf("%s/%s/blob/%s/%s#L%d-L%d", github, p.Repo, ref, p.File, p.StartLine, p.EndLine)
		t := tokens(p.Title + " " + p.Title + " " + p.File + " " + p.Text) // title counted twice
		ix.toks = append(ix.toks, t)
		total += len(t)
		seen := map[string]bool{}
		for _, w := range t {
			if !seen[w] {
				seen[w] = true
				ix.df[w]++
			}
		}
	}
	if len(ix.passages) == 0 {
		return nil, fmt.Errorf("empty corpus: run scripts/snapshot-corpus.sh")
	}
	ix.avg = float64(total) / float64(len(ix.passages))
	return ix, nil
}

func (ix *Index) Len() int { return len(ix.passages) }

// split cuts Markdown at headings and Go at top-level declarations (with
// their doc comments), keeping 1-based line numbers.
func split(repo, file, text string) []Passage {
	lines := strings.Split(text, "\n")
	var out []Passage
	start, title := 0, file
	flush := func(end int) {
		for end > start && strings.TrimSpace(lines[end-1]) == "" {
			end--
		}
		if end-start < 2 && end-start < len(lines) {
			if end <= start {
				return
			}
		}
		// Long sections are cut into windows so one passage stays readable.
		for s := start; s < end; s += 40 {
			e := min(end, s+40)
			body := strings.Join(lines[s:e], "\n")
			if strings.TrimSpace(body) == "" {
				continue
			}
			out = append(out, Passage{Repo: repo, File: file, StartLine: s + 1, EndLine: e, Title: title, Text: body})
		}
	}
	isGo := strings.HasSuffix(file, ".go")
	isMD := strings.HasSuffix(file, ".md")
	for i, l := range lines {
		boundary := ""
		switch {
		case isMD && strings.HasPrefix(l, "#"):
			boundary = strings.TrimSpace(strings.TrimLeft(l, "#"))
		case isGo && (strings.HasPrefix(l, "func ") || strings.HasPrefix(l, "type ")):
			boundary = strings.TrimSuffix(strings.TrimSpace(strings.SplitN(l, "{", 2)[0]), "(")
			// include the doc comment above the declaration
			j := i
			for j > start && strings.HasPrefix(lines[j-1], "//") {
				j--
			}
			if j > start {
				flush(j)
				start = j
			}
			title = boundary
			continue
		}
		if boundary != "" && i > start {
			flush(i)
			start, title = i, boundary
		} else if boundary != "" {
			title = boundary
		}
	}
	flush(len(lines))
	return out
}

var stop = map[string]bool{}

func init() {
	for _, w := range strings.Fields(`a an and are as at be by can do does for from has have how i if in is it its of
	on or the that this to was what when where which who why will with you your does did into than then there these`) {
		stop[w] = true
	}
}

func tokens(s string) []string {
	var out []string
	for _, w := range strings.FieldsFunc(strings.ToLower(s), func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r)
	}) {
		if len(w) < 2 || stop[w] {
			continue
		}
		if len(w) > 4 && strings.HasSuffix(w, "s") && !strings.HasSuffix(w, "ss") {
			w = w[:len(w)-1]
		}
		out = append(out, w)
	}
	return out
}

// Search returns the top k passages by BM25 (k1=1.2, b=0.75). Markdown and
// eval reports get a small boost because they explain; code proves.
// synonyms maps how people ask to the words the docs and code use.
var synonyms = map[string][]string{
	"down":      {"outage", "stopped", "unavailable", "retry"},
	"duplicate": {"idempotent", "replay", "upsert"},
	"retry":     {"retrie", "backoff", "idempotent"},
	"writer":    {"concurrent", "lock", "advisory"},
	"match":     {"resolution", "resolve", "gstin", "pan"},
	"vendor":    {"company", "gstin"},
	"result":    {"eval", "hit", "f1", "mrr"},
	"crash":     {"kill", "checkpoint", "restart"},
	"fact":      {"memory", "memorie", "supersede"},
	"validate":  {"valid", "checksum"},
}

var repos = map[string]bool{"granth": true, "smriti": true, "sutra": true}

func (ix *Index) Search(q string, k int) []Passage {
	var qt []string
	only := ""
	for _, w := range tokens(q) {
		if repos[w] {
			only = w // "how does granth ..." searches granth only
			continue
		}
		qt = append(qt, w)
		qt = append(qt, synonyms[w]...)
	}
	if len(qt) == 0 {
		return nil
	}
	n := float64(len(ix.passages))
	type hit struct {
		i int
		s float64
	}
	var hits []hit
	for i, t := range ix.toks {
		if only != "" && ix.passages[i].Repo != only {
			continue
		}
		tf := map[string]int{}
		for _, w := range t {
			tf[w]++
		}
		var s float64
		for _, w := range qt {
			f := float64(tf[w])
			if f == 0 {
				continue
			}
			df := float64(ix.df[w])
			s += math.Log(1+(n-df+0.5)/(df+0.5)) * f * 2.2 / (f + 1.2*(0.25+0.75*float64(len(t))/ix.avg))
		}
		if s == 0 {
			continue
		}
		if ext := path.Ext(ix.passages[i].File); ext == ".md" || ext == ".txt" {
			s *= 1.15
		}
		if strings.HasSuffix(ix.passages[i].File, "_test.go") {
			s *= 0.9
		}
		hits = append(hits, hit{i, s})
	}
	sort.Slice(hits, func(a, b int) bool { return hits[a].s > hits[b].s })
	var out []Passage
	for _, h := range hits {
		if len(out) == k {
			break
		}
		p := ix.passages[h.i]
		p.Score = math.Round(h.s*100) / 100
		out = append(out, p)
	}
	return out
}
