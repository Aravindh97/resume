// Package crdt is the shared-notes data type: every device applies the
// same operations, in any order, and ends up with exactly the same notes.
//
// Note bodies are an RGA sequence (replicated growable array). Every
// character has a unique id (lamport, replica). An insert names the
// character it goes after, and a delete marks a character as removed
// (a tombstone). Two devices typing at the same place at the same time
// both keep their text, ordered by id, identically everywhere. Titles and
// "this note is deleted" are last-writer-wins registers ordered by the same
// ids.
//
// This package is compiled into the server and, as WebAssembly, into the
// browser, so the merge logic exists exactly once.
package crdt

import (
	"errors"
	"fmt"
	"sort"
	"strings"
	"unicode/utf8"
)

// ID orders every operation: by Lamport clock, then by replica.
type ID struct {
	L uint64 `json:"l"`
	R string `json:"r"`
}

func (a ID) Less(b ID) bool { return a.L < b.L || a.L == b.L && a.R < b.R }
func (a ID) IsZero() bool   { return a.L == 0 && a.R == "" }

// Op is one change. Replica+Seq identify it for syncing (Seq counts up from
// 1 on each replica); ID orders it.
type Op struct {
	Replica string `json:"replica"`
	Seq     uint64 `json:"seq"`
	ID      ID     `json:"id"`
	Note    string `json:"note"`
	Kind    string `json:"kind"`              // create | ins | del | title | trash
	After   ID     `json:"after,omitzero"`    // ins: the character this text goes after (zero = start)
	Text    string `json:"text,omitempty"`    // ins: characters, ids ID.L, ID.L+1, ...; title: the title
	Targets []ID   `json:"targets,omitempty"` // del: characters removed
	Trash   bool   `json:"trash,omitempty"`   // trash: true deletes the note, false restores it
}

const (
	MaxText    = 10000 // characters in one insert or title
	MaxTargets = 10000
)

var ErrInvalid = errors.New("invalid operation")

// Validate checks an op's shape (not whether its dependencies are present).
func (o Op) Validate() error {
	switch {
	case o.Replica == "" || len(o.Replica) > 64 || o.Seq == 0:
		return fmt.Errorf("%w: replica and seq are required", ErrInvalid)
	case o.ID.R != o.Replica || o.ID.L == 0:
		return fmt.Errorf("%w: id must belong to the replica", ErrInvalid)
	case o.Note == "" || len(o.Note) > 64:
		return fmt.Errorf("%w: note id is required", ErrInvalid)
	case !utf8.ValidString(o.Text) || utf8.RuneCountInString(o.Text) > MaxText:
		return fmt.Errorf("%w: text too long or not UTF-8", ErrInvalid)
	}
	switch o.Kind {
	case "create", "trash":
	case "title":
		if strings.ContainsAny(o.Text, "\n\r") {
			return fmt.Errorf("%w: titles are one line", ErrInvalid)
		}
	case "ins":
		if o.Text == "" {
			return fmt.Errorf("%w: empty insert", ErrInvalid)
		}
	case "del":
		if len(o.Targets) == 0 || len(o.Targets) > MaxTargets {
			return fmt.Errorf("%w: delete needs 1 to %d targets", ErrInvalid, MaxTargets)
		}
	default:
		return fmt.Errorf("%w: unknown kind %q", ErrInvalid, o.Kind)
	}
	return nil
}

type elem struct {
	id      ID
	ch      rune
	deleted bool
}

type note struct {
	id       string
	created  ID
	title    string
	titleID  ID
	trashed  bool
	trashID  ID
	elems    []elem
	index    map[ID]int // position of each element in elems (rebuilt lazily)
	dirty    bool
	lastEdit ID
}

func (n *note) pos(id ID) (int, bool) {
	if n.dirty {
		n.index = make(map[ID]int, len(n.elems))
		for i, e := range n.elems {
			n.index[e.id] = i
		}
		n.dirty = false
	}
	i, ok := n.index[id]
	return i, ok
}

// Doc is one replica's view of all notes in a space.
type Doc struct {
	Replica string
	clock   uint64
	seq     uint64
	notes   map[string]*note
	seen    map[string]uint64 // replica -> highest seq applied
	waiting []Op              // ops whose dependencies have not arrived yet
}

func New(replica string) *Doc {
	return &Doc{Replica: replica, notes: map[string]*note{}, seen: map[string]uint64{}}
}

// Seen reports whether an op was already applied (ops are idempotent).
func (d *Doc) Seen(o Op) bool { return d.seen[o.Replica] >= o.Seq }

// Apply integrates a remote or local op. Ops already applied are ignored;
// ops whose dependencies are missing wait until they arrive. It returns
// whether anything changed.
func (d *Doc) Apply(o Op) (bool, error) {
	if err := o.Validate(); err != nil {
		return false, err
	}
	if d.Seen(o) || d.isWaiting(o) {
		return false, nil
	}
	if !d.ready(o) {
		d.waiting = append(d.waiting, o)
		return false, nil
	}
	d.integrate(o)
	// applying one op may unblock others
	for progress := true; progress; {
		progress = false
		rest := d.waiting[:0]
		for _, w := range d.waiting {
			if d.Seen(w) {
				continue
			}
			if d.ready(w) {
				d.integrate(w)
				progress = true
			} else {
				rest = append(rest, w)
			}
		}
		d.waiting = rest
	}
	return true, nil
}

func (d *Doc) isWaiting(o Op) bool {
	for _, w := range d.waiting {
		if w.Replica == o.Replica && w.Seq == o.Seq {
			return true
		}
	}
	return false
}

// Waiting is the number of ops held back for missing dependencies.
func (d *Doc) Waiting() int { return len(d.waiting) }

// ready: a replica's ops apply in seq order, and an op on a note needs the
// note and every character it refers to.
func (d *Doc) ready(o Op) bool {
	if d.seen[o.Replica] != o.Seq-1 {
		return false
	}
	if o.Kind == "create" {
		return true
	}
	n := d.notes[o.Note]
	if n == nil {
		return false
	}
	switch o.Kind {
	case "ins":
		if !o.After.IsZero() {
			if _, ok := n.pos(o.After); !ok {
				return false
			}
		}
	case "del":
		for _, t := range o.Targets {
			if _, ok := n.pos(t); !ok {
				return false
			}
		}
	}
	return true
}

func (d *Doc) integrate(o Op) {
	d.seen[o.Replica] = o.Seq
	top := o.ID.L
	if o.Kind == "ins" {
		top += uint64(utf8.RuneCountInString(o.Text)) - 1
	}
	if top > d.clock {
		d.clock = top
	}
	n := d.notes[o.Note]
	switch o.Kind {
	case "create":
		if n == nil {
			d.notes[o.Note] = &note{id: o.Note, created: o.ID, lastEdit: o.ID, index: map[ID]int{}}
		}
		return
	case "title":
		if n.titleID.Less(o.ID) {
			n.title, n.titleID = o.Text, o.ID
		}
	case "trash":
		if n.trashID.Less(o.ID) {
			n.trashed, n.trashID = o.Trash, o.ID
		}
	case "ins":
		d.insert(n, o)
	case "del":
		for _, t := range o.Targets {
			i, _ := n.pos(t)
			n.elems[i].deleted = true
		}
	}
	if n.lastEdit.Less(o.ID) {
		n.lastEdit = o.ID
	}
}

// insert places a run of characters after o.After. Going right from there,
// it skips characters with a larger id: those were inserted concurrently at
// the same place (or after them) and win the left position everywhere, so
// every replica ends up with the same order.
func (d *Doc) insert(n *note, o Op) {
	i := 0
	if !o.After.IsZero() {
		p, _ := n.pos(o.After)
		i = p + 1
	}
	first := ID{o.ID.L, o.ID.R}
	for i < len(n.elems) && first.Less(n.elems[i].id) {
		i++
	}
	runes := []rune(o.Text)
	run := make([]elem, len(runes))
	for k, r := range runes {
		run[k] = elem{id: ID{o.ID.L + uint64(k), o.ID.R}, ch: r}
	}
	n.elems = append(n.elems[:i], append(run, n.elems[i:]...)...)
	n.dirty = true
}

// next makes the next local op's id and sequence number.
func (d *Doc) next(width uint64) (ID, uint64) {
	d.clock++
	id := ID{d.clock, d.Replica}
	d.clock += width - 1
	d.seq++
	return id, d.seq
}

// SetSeq restores this replica's sequence after a reload.
func (d *Doc) SetSeq(seq uint64) {
	if seq > d.seq {
		d.seq = seq
	}
}

func (d *Doc) local(o Op) (Op, error) {
	o.Replica = d.Replica
	if _, err := d.Apply(o); err != nil {
		d.seq--
		return Op{}, err
	}
	return o, nil
}

// CreateNote makes a new note and returns the op.
func (d *Doc) CreateNote(noteID string) (Op, error) {
	id, seq := d.next(1)
	return d.local(Op{Seq: seq, ID: id, Note: noteID, Kind: "create"})
}

func (d *Doc) SetTitle(noteID, title string) (Op, error) {
	id, seq := d.next(1)
	return d.local(Op{Seq: seq, ID: id, Note: noteID, Kind: "title", Text: strings.ReplaceAll(title, "\n", " ")})
}

func (d *Doc) Trash(noteID string, trash bool) (Op, error) {
	id, seq := d.next(1)
	return d.local(Op{Seq: seq, ID: id, Note: noteID, Kind: "trash", Trash: trash})
}

// Edit turns a change of a note's visible text from old to new into ops:
// one delete for the removed characters and one insert for the added ones.
// It works on the common prefix and suffix, which is what a text box
// change is (typing, pasting or deleting a selection).
func (d *Doc) Edit(noteID, newText string) ([]Op, error) {
	n := d.notes[noteID]
	if n == nil {
		return nil, fmt.Errorf("no note %q", noteID)
	}
	var live []elem
	for _, e := range n.elems {
		if !e.deleted {
			live = append(live, e)
		}
	}
	nw := []rune(newText)
	p := 0
	for p < len(live) && p < len(nw) && live[p].ch == nw[p] {
		p++
	}
	s := 0
	for s < len(live)-p && s < len(nw)-p && live[len(live)-1-s].ch == nw[len(nw)-1-s] {
		s++
	}
	var ops []Op
	if gone := live[p : len(live)-s]; len(gone) > 0 {
		targets := make([]ID, len(gone))
		for i, e := range gone {
			targets[i] = e.id
		}
		for len(targets) > 0 {
			chunk := targets[:min(len(targets), MaxTargets)]
			targets = targets[len(chunk):]
			id, seq := d.next(1)
			o, err := d.local(Op{Seq: seq, ID: id, Note: noteID, Kind: "del", Targets: chunk})
			if err != nil {
				return nil, err
			}
			ops = append(ops, o)
		}
	}
	added := nw[p : len(nw)-s]
	after := ID{}
	if p > 0 {
		after = live[p-1].id
	}
	for len(added) > 0 {
		chunk := added[:min(len(added), MaxText)]
		added = added[len(chunk):]
		id, seq := d.next(uint64(len(chunk)))
		o, err := d.local(Op{Seq: seq, ID: id, Note: noteID, Kind: "ins", After: after, Text: string(chunk)})
		if err != nil {
			return nil, err
		}
		ops = append(ops, o)
		after = ID{id.L + uint64(len(chunk)) - 1, id.R}
	}
	return ops, nil
}

// Text is a note's visible body.
func (d *Doc) Text(noteID string) string {
	n := d.notes[noteID]
	if n == nil {
		return ""
	}
	var b strings.Builder
	for _, e := range n.elems {
		if !e.deleted {
			b.WriteRune(e.ch)
		}
	}
	return b.String()
}

// Anchor names the character just before a cursor position, so the cursor
// can follow it when other devices' edits arrive. Zero means the start.
func (d *Doc) Anchor(noteID string, offset int) ID {
	n := d.notes[noteID]
	if n == nil || offset <= 0 {
		return ID{}
	}
	k := 0
	for _, e := range n.elems {
		if e.deleted {
			continue
		}
		k++
		if k == offset {
			return e.id
		}
	}
	return ID{}
}

// Offset is where an anchor is now: just after the anchored character, or,
// if it was deleted, after the nearest visible character before it.
func (d *Doc) Offset(noteID string, a ID) int {
	n := d.notes[noteID]
	if n == nil || a.IsZero() {
		return 0
	}
	k := 0
	for _, e := range n.elems {
		if !e.deleted {
			k++
		}
		if e.id == a {
			return k
		}
	}
	return 0
}

// Summary is what a notes list shows.
type Summary struct {
	ID       string `json:"id"`
	Title    string `json:"title"`
	Snippet  string `json:"snippet"`
	Trashed  bool   `json:"trashed"`
	Chars    int    `json:"chars"`
	LastEdit ID     `json:"last_edit"`
	Created  ID     `json:"created"`
}

// Notes lists every note, most recently edited first.
func (d *Doc) Notes() []Summary {
	out := make([]Summary, 0, len(d.notes))
	for id, n := range d.notes {
		t := d.Text(id)
		snip := strings.Join(strings.Fields(t), " ")
		if r := []rune(snip); len(r) > 120 {
			snip = string(r[:120]) + "…"
		}
		out = append(out, Summary{ID: id, Title: n.title, Snippet: snip, Trashed: n.trashed, Chars: utf8.RuneCountInString(t), LastEdit: n.lastEdit, Created: n.created})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].LastEdit != out[j].LastEdit {
			return out[j].LastEdit.Less(out[i].LastEdit)
		}
		return out[i].ID < out[j].ID
	})
	return out
}

// Fingerprint summarises the visible state, for checking two replicas agree.
func (d *Doc) Fingerprint() string {
	var b strings.Builder
	ns := d.Notes()
	sort.Slice(ns, func(i, j int) bool { return ns[i].ID < ns[j].ID })
	for _, s := range ns {
		fmt.Fprintf(&b, "%s|%s|%v|%s\n", s.ID, s.Title, s.Trashed, d.Text(s.ID))
	}
	return b.String()
}
