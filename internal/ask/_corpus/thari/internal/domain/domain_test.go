package domain

import (
	"math/rand/v2"
	"testing"
	"time"
)

func TestApplyProgress(t *testing.T) {
	q := 1000
	cases := []struct {
		name  string
		done  []int
		stage string
		delta int
		want  int
		ok    bool
	}{
		{"fabric up to the quantity", []int{0, 0, 0, 0, 0}, "fabric", 1000, 1000, true},
		{"fabric beyond the quantity", []int{900, 0, 0, 0, 0}, "fabric", 200, 0, false},
		{"cut only what has fabric", []int{600, 0, 0, 0, 0}, "cutting", 601, 0, false},
		{"cut within fabric", []int{600, 0, 0, 0, 0}, "cutting", 600, 600, true},
		{"stitch ahead of cutting", []int{1000, 300, 0, 0, 0}, "stitching", 301, 0, false},
		{"correction below zero", []int{1000, 300, 0, 0, 0}, "cutting", -301, 0, false},
		{"correction below the next stage", []int{1000, 300, 200, 0, 0}, "cutting", -150, 0, false},
		{"valid correction", []int{1000, 300, 200, 0, 0}, "cutting", -100, 200, true},
		{"unknown stage", []int{0, 0, 0, 0, 0}, "dyeing", 1, 0, false},
		{"zero", []int{0, 0, 0, 0, 0}, "fabric", 0, 0, false},
	}
	for _, c := range cases {
		got, err := ApplyProgress(c.done, q, c.stage, c.delta)
		if (err == nil) != c.ok || c.ok && got != c.want {
			t.Errorf("%s: got %d, %v", c.name, got, err)
		}
	}
}

// Any sequence of accepted updates keeps the stages ordered:
// quantity >= fabric >= cutting >= ... >= packing >= 0.
func TestProgressInvariantUnderRandomUpdates(t *testing.T) {
	r := rand.New(rand.NewPCG(1, 2))
	for run := 0; run < 2000; run++ {
		q := 1 + r.IntN(5000)
		done := make([]int, len(Stages))
		for step := 0; step < 100; step++ {
			i := r.IntN(len(Stages))
			delta := r.IntN(q) - q/4
			n, err := ApplyProgress(done, q, Stages[i], delta)
			if err != nil {
				continue
			}
			done[i] = n
			prev := q
			for j, d := range done {
				if d < 0 || d > prev {
					t.Fatalf("run %d: stage %d = %d after %v", run, j, d, done)
				}
				prev = d
			}
		}
	}
}

func TestCurrentStageAndCompletion(t *testing.T) {
	if s := CurrentStage([]int{100, 100, 40, 0, 0}, 100); s != "stitching" {
		t.Fatal(s)
	}
	if s := CurrentStage([]int{100, 100, 100, 100, 100}, 100); s != "ready" {
		t.Fatal(s)
	}
	if c := Completion([]int{100, 100, 50, 0, 0}, 100); c != 0.5 {
		t.Fatal(c)
	}
}

func TestRisk(t *testing.T) {
	d := func(s string) time.Time { x, _ := time.Parse("2006-01-02", s); return x }
	from, due := d("2026-10-01"), d("2026-10-31")
	cases := []struct {
		today string
		comp  float64
		want  Risk
	}{
		{"2026-10-10", 0.30, OnTrack}, // 30% of the window used, 30% done
		{"2026-10-16", 0.30, AtRisk},  // half the window used, 30% done
		{"2026-10-29", 0.75, AtRisk},  // under 3 days left, under 80%
		{"2026-10-29", 0.95, OnTrack},
		{"2026-11-01", 0.99, Late},
	}
	for _, c := range cases {
		if got := RiskOf("open", from, due, d(c.today), c.comp); got != c.want {
			t.Errorf("%s at %.2f: %s, want %s", c.today, c.comp, got, c.want)
		}
	}
	if RiskOf("shipped", from, due, d("2026-12-01"), 1) != Done {
		t.Fatal("shipped order reported late")
	}
}

func TestAQL(t *testing.T) {
	cases := []struct {
		lot, sample, major, minor int
		letter                    string
	}{
		{5, 5, 0, 0, "A"}, {60, 13, 1, 1, "E"}, {500, 50, 3, 5, "H"}, {1000, 80, 5, 7, "J"},
		{3000, 125, 7, 10, "K"}, {5000, 200, 10, 14, "L"}, {20000, 315, 14, 21, "M"},
	}
	for _, c := range cases {
		p := PlanFor(c.lot)
		if p.SampleSize != c.sample || p.AcceptMajor != c.major || p.AcceptMinor != c.minor || p.CodeLetter != c.letter {
			t.Errorf("lot %d: %+v", c.lot, p)
		}
	}
	p := PlanFor(5000)
	if p.Judge(0, 10, 14) != "pass" || p.Judge(0, 11, 0) != "fail" || p.Judge(1, 0, 0) != "fail" {
		t.Fatal("judging")
	}
	// Sample never exceeds the lot, and acceptance numbers never decrease as lots grow.
	prev := PlanFor(9)
	for lot := 9; lot < 600000; lot += 37 {
		p := PlanFor(lot)
		if p.SampleSize > lot || p.AcceptMajor < prev.AcceptMajor || p.SampleSize < prev.SampleSize {
			t.Fatalf("lot %d: %+v after %+v", lot, p, prev)
		}
		prev = p
	}
}

// The full permission matrix, so a change to who may do what shows up here.
func TestPermissions(t *testing.T) {
	const factory, otherFactory, buyerA, buyerB = 1, 99, 10, 11
	admin := Principal{UserID: 1, OrgID: factory, Role: Admin, FactoryID: factory}
	merch := Principal{UserID: 2, OrgID: factory, Role: Merchandiser, FactoryID: factory}
	prod := Principal{UserID: 3, OrgID: factory, Role: Production, FactoryID: factory}
	buyer := Principal{UserID: 4, OrgID: buyerA, Role: Buyer, FactoryID: factory}
	want := map[Action][4]bool{ // admin, merchandiser, production, buyer, on buyer A's order
		ViewOrder: {true, true, true, true}, ViewPrice: {true, true, false, false},
		CreateOrder: {true, true, false, false}, EditOrder: {true, true, false, false},
		AddProgress: {true, true, true, false}, CorrectProgress: {true, true, false, false},
		Inspect: {true, false, true, false}, Comment: {true, true, true, true},
		UploadFile: {true, true, false, false}, Ship: {true, true, false, false},
		ManageUsers: {true, false, false, false}, ViewAudit: {true, false, false, false},
		ViewInternal: {true, true, true, false},
	}
	for a, w := range want {
		for i, p := range []Principal{admin, merch, prod, buyer} {
			if got := p.Can(a, factory, buyerA); got != w[i] {
				t.Errorf("%s as %s: %v", a, p.Role, got)
			}
		}
	}
	if buyer.Can(ViewOrder, factory, buyerB) {
		t.Error("buyer sees another buyer's order")
	}
	if admin.Can(ViewOrder, otherFactory, buyerA) {
		t.Error("staff see another factory's order")
	}
}
