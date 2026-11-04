package crdt

import (
	"fmt"
	"math/rand/v2"
	"strings"
	"testing"
)

func must[T any](v T, err error) T {
	if err != nil {
		panic(err)
	}
	return v
}

// sync gives b every op a has (and vice versa) through the logs.
func exchange(docs []*Doc, logs [][]Op) {
	for _, d := range docs {
		for _, l := range logs {
			for _, o := range l {
				must(d.Apply(o))
			}
		}
	}
}

func TestTypingAndDeleting(t *testing.T) {
	d := New("a")
	c := must(d.CreateNote("n1"))
	_ = c
	must(d.Edit("n1", "Hello world"))
	must(d.Edit("n1", "Hello brave new world"))
	must(d.Edit("n1", "Hello new world!"))
	if got := d.Text("n1"); got != "Hello new world!" {
		t.Fatal(got)
	}
	must(d.Edit("n1", ""))
	if d.Text("n1") != "" {
		t.Fatal("not emptied")
	}
	must(d.Edit("n1", "தறி and குறிப்பு")) // multi-byte characters
	if d.Text("n1") != "தறி and குறிப்பு" {
		t.Fatal(d.Text("n1"))
	}
}

// Two people edit the same sentence while offline; after syncing both see
// both edits, in the same order.
func TestConcurrentEditsMerge(t *testing.T) {
	a, b := New("phone"), New("laptop")
	var la, lb []Op
	la = append(la, must(a.CreateNote("n")))
	la = append(la, must(a.Edit("n", "Order 500 hoodies"))...)
	exchange([]*Doc{b}, [][]Op{la})
	// offline: phone changes the number, laptop adds a colour at the end
	lb = append(lb, must(b.Edit("n", "Order 500 hoodies in navy"))...)
	la2 := must(a.Edit("n", "Order 800 hoodies"))
	exchange([]*Doc{a, b}, [][]Op{la, la2, lb})
	if a.Text("n") != b.Text("n") || a.Text("n") != "Order 800 hoodies in navy" {
		t.Fatalf("phone %q, laptop %q", a.Text("n"), b.Text("n"))
	}
	// both type at the very same spot: both survive, same order everywhere
	x := must(a.Edit("n", "Order 800 hoodies in navy. Urgent"))
	y := must(b.Edit("n", "Order 800 hoodies in navy. By Friday"))
	exchange([]*Doc{a, b}, [][]Op{x, y})
	if a.Text("n") != b.Text("n") || !strings.Contains(a.Text("n"), "Urgent") || !strings.Contains(a.Text("n"), "By Friday") {
		t.Fatalf("phone %q, laptop %q", a.Text("n"), b.Text("n"))
	}
}

func TestOpsAreIdempotentAndWaitForDependencies(t *testing.T) {
	a := New("a")
	ops := []Op{must(a.CreateNote("n"))}
	ops = append(ops, must(a.Edit("n", "abc"))...)
	ops = append(ops, must(a.Edit("n", "abXc"))...)
	b := New("b")
	// deliver backwards, twice: everything waits until the create arrives
	for k := 0; k < 2; k++ {
		for i := len(ops) - 1; i >= 0; i-- {
			must(b.Apply(ops[i]))
		}
	}
	if b.Text("n") != "abXc" || b.Waiting() != 0 {
		t.Fatalf("%q waiting %d", b.Text("n"), b.Waiting())
	}
}

func TestTitleAndTrashAreLastWriterWins(t *testing.T) {
	a, b := New("a"), New("b")
	c := must(a.CreateNote("n"))
	must(b.Apply(c))
	t1 := must(a.SetTitle("n", "Shopping"))
	t2 := must(b.SetTitle("n", "Groceries")) // later id wins everywhere
	d1 := must(a.Trash("n", true))
	must(a.Apply(t2))
	must(b.Apply(t1))
	must(b.Apply(d1))
	if a.Fingerprint() != b.Fingerprint() {
		t.Fatalf("\n%s\n%s", a.Fingerprint(), b.Fingerprint())
	}
}

func TestValidation(t *testing.T) {
	bad := []Op{
		{Replica: "a", Seq: 1, ID: ID{1, "b"}, Note: "n", Kind: "create"}, // id from another replica
		{Replica: "a", Seq: 1, ID: ID{1, "a"}, Note: "n", Kind: "ins"},    // empty insert
		{Replica: "a", Seq: 1, ID: ID{1, "a"}, Note: "n", Kind: "title", Text: "a\nb"},
		{Replica: "a", Seq: 1, ID: ID{1, "a"}, Note: "n", Kind: "explode"},
		{Replica: "a", Seq: 1, ID: ID{1, "a"}, Note: "n", Kind: "ins", Text: strings.Repeat("x", MaxText+1)},
		{Replica: "a", Seq: 1, ID: ID{1, "a"}, Note: "n", Kind: "ins", Text: "\xff"},
	}
	for i, o := range bad {
		if _, err := New("z").Apply(o); err == nil {
			t.Errorf("case %d accepted", i)
		}
	}
}

// Cursors follow the text around them when other devices edit.
func TestAnchors(t *testing.T) {
	a, b := New("a"), New("b")
	ops := []Op{must(a.CreateNote("n"))}
	ops = append(ops, must(a.Edit("n", "hello world"))...)
	exchange([]*Doc{b}, [][]Op{ops})
	anchor := a.Anchor("n", 5) // after "hello"
	remote := must(b.Edit("n", "Oh, hello world"))
	exchange([]*Doc{a}, [][]Op{remote})
	if off := a.Offset("n", anchor); off != 9 {
		t.Fatalf("cursor at %d, want 9", off)
	}
}

// Random sessions: 3 to 5 replicas edit random notes offline in rounds, and
// ops reach each replica in a different random order with duplicates. Every
// replica must end identical. A run fails on the first divergence.
func TestRandomConvergence(t *testing.T) {
	words := []string{"fabric ", "cut ", "stitch ", "pack ", "ship ", "\n", "குறி ", "!", "500 "}
	runs := 1500
	if testing.Short() {
		runs = 200
	}
	for run := 0; run < runs; run++ {
		r := rand.New(rand.NewPCG(uint64(run), 99))
		n := 3 + r.IntN(3)
		docs := make([]*Doc, n)
		logs := make([][]Op, n)
		for i := range docs {
			docs[i] = New(fmt.Sprintf("r%d", i))
		}
		notes := []string{"n0"}
		logs[0] = append(logs[0], must(docs[0].CreateNote("n0")))
		for round := 0; round < 6; round++ {
			for i, d := range docs {
				for k := 0; k < 1+r.IntN(4); k++ {
					switch x := r.IntN(10); {
					case x == 0:
						id := fmt.Sprintf("n%d-%d-%d", i, round, k)
						logs[i] = append(logs[i], must(d.CreateNote(id)))
						notes = append(notes, id)
					case x == 1:
						if nid := notes[r.IntN(len(notes))]; d.notes[nid] != nil {
							logs[i] = append(logs[i], must(d.SetTitle(nid, words[r.IntN(len(words))])))
						}
					case x == 2:
						if nid := notes[r.IntN(len(notes))]; d.notes[nid] != nil {
							logs[i] = append(logs[i], must(d.Trash(nid, r.IntN(2) == 0)))
						}
					default:
						nid := notes[r.IntN(len(notes))]
						if d.notes[nid] == nil {
							continue
						}
						old := []rune(d.Text(nid))
						p := r.IntN(len(old) + 1)
						q := min(len(old), p+r.IntN(6))
						if r.IntN(3) == 0 {
							q = p // pure insert
						}
						nw := string(old[:p]) + words[r.IntN(len(words))] + string(old[q:])
						if r.IntN(4) == 0 {
							nw = string(old[:p]) + string(old[q:]) // pure delete
						}
						logs[i] = append(logs[i], must(d.Edit(nid, nw))...)
					}
				}
			}
			// partial sync: each replica receives a random subset of others' ops,
			// shuffled and with duplicates
			for i, d := range docs {
				for j := range docs {
					if i == j || r.IntN(2) == 0 {
						continue
					}
					l := append([]Op(nil), logs[j]...)
					r.Shuffle(len(l), func(a, b int) { l[a], l[b] = l[b], l[a] })
					for _, o := range l[:(len(l)+1)/2] {
						must(d.Apply(o))
						if r.IntN(5) == 0 {
							must(d.Apply(o))
						}
					}
				}
			}
		}
		// final full sync in random orders
		for _, d := range docs {
			var all []Op
			for _, l := range logs {
				all = append(all, l...)
			}
			r.Shuffle(len(all), func(a, b int) { all[a], all[b] = all[b], all[a] })
			for _, o := range all {
				must(d.Apply(o))
			}
		}
		want := docs[0].Fingerprint()
		for i, d := range docs {
			if d.Waiting() != 0 {
				t.Fatalf("run %d: replica %d still waiting on %d ops", run, i, d.Waiting())
			}
			if got := d.Fingerprint(); got != want {
				t.Fatalf("run %d: replica %d diverged\n%s\nvs\n%s", run, i, got, want)
			}
		}
	}
}

func BenchmarkTyping(b *testing.B) {
	d := New("a")
	must(d.CreateNote("n"))
	text := ""
	for i := 0; i < b.N; i++ {
		text += "x"
		must(d.Edit("n", text))
		if len(text) > 5000 {
			text = ""
			must(d.Edit("n", ""))
		}
	}
}
