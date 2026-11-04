package crdt

import (
	"strings"
	"testing"
)

// One keystroke in the middle of a note of the given size.
func benchKeystroke(b *testing.B, size int) {
	d := New("a")
	must(d.CreateNote("n"))
	text := strings.Repeat("abcdefghij", size/10)
	must(d.Edit("n", text))
	mid := len(text) / 2
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		text = text[:mid] + "x" + text[mid:]
		must(d.Edit("n", text))
	}
}

func BenchmarkKeystroke1k(b *testing.B)  { benchKeystroke(b, 1000) }
func BenchmarkKeystroke10k(b *testing.B) { benchKeystroke(b, 10000) }

// Applying a remote device's typing (one op per character) to a 10k note.
func BenchmarkApplyRemote(b *testing.B) {
	src := New("src")
	c := must(src.CreateNote("n"))
	base := must(src.Edit("n", strings.Repeat("abcdefghij", 1000)))
	var ops []Op
	text := src.Text("n")
	for i := 0; i < b.N; i++ {
		text = text[:5000] + "y" + text[5000:]
		ops = append(ops, must(src.Edit("n", text))...)
	}
	d := New("dst")
	must(d.Apply(c))
	for _, o := range base {
		must(d.Apply(o))
	}
	b.ResetTimer()
	for _, o := range ops {
		must(d.Apply(o))
	}
}
