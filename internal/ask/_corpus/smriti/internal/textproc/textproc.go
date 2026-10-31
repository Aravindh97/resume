// Package textproc is the tokenizer used by the in-memory index and the local
// embedder. It keeps Unicode combining marks inside tokens, which matters for
// Indic scripts: splitting on "non-letter" alone would cut Hindi and Tamil
// words at every vowel sign.
package textproc

import (
	"strings"
	"unicode"
)

var stop = map[string]bool{}

func init() {
	for _, w := range strings.Fields(`a an and are as at be by can do does for from has have how i if in
	is it its of on or our should that the their them then there these this to was we what when
	where which who will with you your my me per any all may must not no`) {
		stop[w] = true
	}
}

// Tokens lowercases, splits on anything that is not a letter, digit or mark,
// drops English stop words and applies a light plural/suffix stemmer to
// Latin-script words.
func Tokens(s string) []string {
	s = strings.ToLower(s)
	var out []string
	var cur []rune
	flush := func() {
		if len(cur) == 0 {
			return
		}
		w := string(cur)
		cur = cur[:0]
		if stop[w] {
			return
		}
		out = append(out, stem(w))
	}
	for _, r := range s {
		if unicode.IsLetter(r) || unicode.IsDigit(r) || unicode.IsMark(r) {
			cur = append(cur, r)
			continue
		}
		flush()
	}
	flush()
	return out
}

func stem(w string) string {
	for _, r := range w {
		if r > unicode.MaxLatin1 {
			return w // only stem Latin-script words
		}
	}
	switch {
	case len(w) > 5 && strings.HasSuffix(w, "ies"):
		return w[:len(w)-3] + "y"
	case len(w) > 5 && strings.HasSuffix(w, "ing"):
		return w[:len(w)-3]
	case len(w) > 4 && strings.HasSuffix(w, "ed"):
		return w[:len(w)-2]
	case len(w) > 3 && strings.HasSuffix(w, "s") && !strings.HasSuffix(w, "ss"):
		return w[:len(w)-1]
	}
	return w
}
