package crdt

import (
	"reflect"
	"testing"
)

func fixed(ms *int64) *Clock { return NewClock(func() int64 { return *ms }) }

func TestConcurrentStockChangesAreNotLost(t *testing.T) {
	a, b := New("a", nil), New("b", nil)
	a.AddStock("rice_kg", 100)
	Sync(a, b)
	a.AddStock("rice_kg", -30) // camp A hands out 30 offline
	b.AddStock("rice_kg", -50) // camp B hands out 50 offline
	b.AddStock("rice_kg", 20)  // and receives 20
	Sync(a, b)
	if a.State().Stock["rice_kg"] != 40 || a.Hash() != b.Hash() {
		t.Fatalf("a=%v b=%v", a.State(), b.State())
	}
}

func TestNewRequestSurvivesConcurrentClose(t *testing.T) {
	a, b := New("a", nil), New("b", nil)
	a.OpenNeed("insulin camp-2")
	Sync(a, b)
	b.CloseNeed("insulin camp-2") // b delivers the first request offline
	a.OpenNeed("insulin camp-2")  // meanwhile a new request comes in at a
	Sync(a, b)
	if got := a.State().Needs; !reflect.DeepEqual(got, []string{"insulin camp-2"}) {
		t.Fatalf("new request was erased: %v", got)
	}
	a.CloseNeed("insulin camp-2")
	Sync(a, b)
	if len(b.State().Needs) != 0 {
		t.Fatalf("still open: %v", b.State().Needs)
	}
}

func TestCloseArrivingBeforeAddViaRelay(t *testing.T) {
	// a opens, b sees it and closes it, c gets b's ops first... ops travel
	// per device in order, so c gets a's add whenever it gets b's close
	// only if b relays both. Check the relay path converges.
	a, b, c := New("a", nil), New("b", nil), New("c", nil)
	a.OpenNeed("boat")
	Sync(a, b)
	b.CloseNeed("boat")
	Sync(b, c) // c gets a's add and b's close from b
	Sync(a, c)
	for _, d := range []*Doc{a, b, c} {
		if len(d.State().Needs) != 0 {
			t.Fatalf("%s sees %v", d.Device, d.State().Needs)
		}
	}
}

func TestLastWriterWinsDespiteFastClock(t *testing.T) {
	// a's phone clock is 5 minutes fast. a sets status, b reads it and then
	// updates. b's edit happened later, so it must win, even though b's
	// physical clock reads earlier than a's timestamp.
	ta, tb := int64(1_000_000+300_000), int64(1_000_000)
	a, b := New("a", fixed(&ta)), New("b", fixed(&tb))
	a.SetField("camp-2.status", "flooded")
	Sync(a, b)
	tb += 1000
	b.SetField("camp-2.status", "evacuated")
	Sync(a, b)
	if a.State().Fields["camp-2.status"] != "evacuated" {
		t.Fatalf("got %q: clock skew let an older edit win", a.State().Fields["camp-2.status"])
	}
}

func TestDuplicatesAndGaps(t *testing.T) {
	a, b := New("a", nil), New("b", nil)
	a.AddStock("water_l", 5)
	a.AddStock("water_l", 7)
	ops := a.Missing(b.VV())
	b.Receive(ops)
	if n, _ := b.Receive(ops); n != 0 || b.State().Stock["water_l"] != 12 {
		t.Fatalf("duplicate applied: n=%d stock=%d", n, b.State().Stock["water_l"])
	}
	a.AddStock("water_l", 1)
	a.AddStock("water_l", 1)
	later := a.Missing(b.VV())
	if _, err := b.Receive(later[1:]); err == nil {
		t.Fatal("gap accepted")
	}
}

func TestSecondSyncMovesNothing(t *testing.T) {
	a, b := New("a", nil), New("b", nil)
	for i := 0; i < 50; i++ {
		a.AddStock("x", 1)
		b.OpenNeed("n")
	}
	if n, _ := Sync(a, b); n != 100 {
		t.Fatalf("first sync moved %d", n)
	}
	if n, _ := Sync(a, b); n != 0 {
		t.Fatalf("second sync moved %d", n)
	}
}

func TestOversubscriptionIsReported(t *testing.T) {
	a, b := New("a", nil), New("b", nil)
	a.AddStock("tarpaulin", 10)
	Sync(a, b)
	a.AddStock("tarpaulin", -8)
	b.AddStock("tarpaulin", -8)
	Sync(a, b)
	if c := a.Conflicts(); len(c) != 1 || c[0] != "tarpaulin oversubscribed by 6" {
		t.Fatalf("conflicts = %v", c)
	}
}
