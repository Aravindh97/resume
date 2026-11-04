package domain

import (
	"math/rand/v2"
	"sort"
	"testing"
	"time"
)

func d(s string) time.Time { t, _ := time.Parse("2006-01-02", s); return t }

// The red-cell table, written out in full: rows are recipients, columns
// donors O- O+ A- A+ B- B+ AB- AB+.
func TestRedCellTable(t *testing.T) {
	want := map[string]string{
		"O-": "10000000", "O+": "11000000", "A-": "10100000", "A+": "11110000",
		"B-": "10001000", "B+": "11001100", "AB-": "10101010", "AB+": "11111111",
	}
	for r, row := range want {
		for i, dg := range Groups {
			if got := Compatible("rbc", r, dg); got != (row[i] == '1') {
				t.Errorf("rbc %s <- %s: %v", r, dg, got)
			}
		}
	}
}

func TestPlasmaTable(t *testing.T) {
	// AB plasma to anyone; O plasma only to O.
	for _, r := range Groups {
		if !Compatible("plasma", r, "AB+") {
			t.Errorf("AB plasma refused for %s", r)
		}
		if Compatible("plasma", r, "O-") != (r == "O-" || r == "O+") {
			t.Errorf("O plasma for %s", r)
		}
	}
}

func units(spec ...string) []Unit {
	var out []Unit
	for i := 0; i < len(spec); i += 2 {
		out = append(out, Unit{ID: int64(i/2 + 1), Group: spec[i], Component: "rbc", ExpiresOn: d(spec[i+1])})
	}
	return out
}

func TestPickOrder(t *testing.T) {
	today := d("2026-11-04")
	stock := units("A+", "2026-11-20", "O+", "2026-11-06", "A+", "2026-11-08", "O-", "2026-11-05", "A+", "2026-11-03", "B+", "2026-11-05")
	got := Pick(Recipient{"A+", "male", 60}, "rbc", "routine", stock, 4, today)
	// own group first, soonest expiry first; the expired A+ and incompatible B+ never
	ids := []int64{}
	for _, u := range got {
		ids = append(ids, u.ID)
	}
	if want := []int64{3, 1, 4, 2}; len(ids) != 4 || ids[0] != want[0] || ids[1] != want[1] || ids[2] != want[2] || ids[3] != want[3] {
		t.Fatalf("picked %v", ids)
	}
}

func TestRhNegativeWomenAreProtected(t *testing.T) {
	today := d("2026-11-04")
	stock := []Unit{{1, "O+", "platelets", d("2026-11-08")}, {2, "O-", "platelets", d("2026-11-07")}}
	young := Recipient{"O-", "female", 28}
	if got := Pick(young, "platelets", "urgent", stock, 2, today); len(got) != 1 || got[0].Group != "O-" {
		t.Fatalf("Rh-negative woman got %v", got)
	}
	if got := Pick(young, "platelets", "emergency", stock, 2, today); len(got) != 2 {
		t.Fatal("in an emergency Rh-positive platelets are allowed")
	}
	if got := Pick(Recipient{"O-", "female", 60}, "platelets", "urgent", stock, 2, today); len(got) != 2 {
		t.Fatal("protection is for women under 50")
	}
	rbc := units("O+", "2026-11-10", "O-", "2026-11-30")
	if got := Pick(Recipient{"O-", "male", 28}, "rbc", "emergency", rbc, 2, today); len(got) != 1 || got[0].Group != "O-" {
		t.Fatal("Rh-positive red cells to an Rh-negative patient")
	}
}

// Whatever is in stock, Pick never returns an incompatible, expired, wrong
// component or duplicate unit, nor more than asked.
func TestPickProperties(t *testing.T) {
	r := rand.New(rand.NewPCG(5, 5))
	today := d("2026-11-04")
	comps := []string{"rbc", "plasma", "platelets"}
	for run := 0; run < 5000; run++ {
		var stock []Unit
		for i := 0; i < r.IntN(40); i++ {
			stock = append(stock, Unit{ID: int64(i), Group: Groups[r.IntN(8)], Component: comps[r.IntN(3)], ExpiresOn: today.AddDate(0, 0, r.IntN(40)-5)})
		}
		rec := Recipient{Groups[r.IntN(8)], []string{"male", "female"}[r.IntN(2)], r.IntN(90)}
		comp, n := comps[r.IntN(3)], 1+r.IntN(5)
		got := Pick(rec, comp, []string{"routine", "urgent", "emergency"}[r.IntN(3)], stock, n, today)
		seen := map[int64]bool{}
		if len(got) > n {
			t.Fatal("too many")
		}
		for _, u := range got {
			if seen[u.ID] || u.Component != comp || !u.ExpiresOn.After(today) || !Compatible(comp, rec.Group, u.Group) {
				t.Fatalf("run %d: bad unit %+v for %+v", run, u, rec)
			}
			seen[u.ID] = true
		}
	}
}

func TestDonorInterval(t *testing.T) {
	today := d("2026-11-04")
	last := d("2026-08-01") // 95 days ago
	if ok, _ := DonorCanGive("male", &last, today); !ok {
		t.Fatal("man after 95 days")
	}
	if ok, next := DonorCanGive("female", &last, today); ok || next != d("2026-11-29") {
		t.Fatalf("woman after 95 days: next %s", next.Format("2006-01-02"))
	}
	if ok, _ := DonorCanGive("female", nil, today); !ok {
		t.Fatal("first-time donor")
	}
}

func at(h, m int) *time.Time {
	t := d("2026-11-04").Add(time.Duration(h)*time.Hour + time.Duration(m)*time.Minute)
	return &t
}

func TestQueue(t *testing.T) {
	now := *at(10, 0)
	q := []QueueItem{
		{ID: 1, Kind: "booked", Token: 1, Slot: at(9, 45), Arrived: at(9, 30)},
		{ID: 2, Kind: "booked", Token: 2, Slot: at(10, 0), Arrived: at(9, 50)},
		{ID: 3, Kind: "booked", Token: 3, Slot: at(9, 30)},                     // never came: no-show
		{ID: 4, Kind: "booked", Token: 4, Slot: at(11, 0), Arrived: at(9, 55)}, // early; not yet
		{ID: 5, Kind: "walkin", Token: 21, Arrived: at(9, 40)},
	}
	p, why, ns := Next(q, now, 0)
	if p.ID != 1 || len(ns) != 1 || ns[0] != 3 {
		t.Fatalf("got %d (%s), no-shows %v", p.ID, why, ns)
	}
	if p, _, _ = Next(q[1:], now, 1); p.ID != 2 {
		t.Fatalf("second booked: %d", p.ID)
	}
	if p, why, _ = Next(q[1:], now, 2); p.ID != 5 {
		t.Fatalf("after 2 booked, a walk-in: %d %s", p.ID, why)
	}
	late := *at(10, 30) // walk-in has waited 50 min: goes first even mid-run
	if p, why, _ = Next(q[1:], late, 0); p.ID != 5 {
		t.Fatalf("long-waiting walk-in: %d %s", p.ID, why)
	}
	if p, _, _ := Next(nil, now, 0); p != nil {
		t.Fatal("empty queue")
	}
}

// Simulate many mornings. Two guarantees hold at every call: no booked
// patient is called while a walk-in has waited 45 minutes or more, and
// walk-ins are called in token order. Every booked patient who arrives is
// seen. (How long walk-ins wait overall depends on how many arrive.)
func TestQueueFairnessSimulation(t *testing.T) {
	r := rand.New(rand.NewPCG(9, 9))
	for run := 0; run < 300; run++ {
		var q []QueueItem
		for i, s := range Slots(d("2026-11-04")) {
			if r.IntN(10) < 8 {
				arr := s.Add(time.Duration(r.IntN(40)-25) * time.Minute)
				item := QueueItem{ID: int64(i + 1), Kind: "booked", Token: i + 1, Slot: &s}
				if r.IntN(10) > 0 {
					item.Arrived = &arr
				}
				q = append(q, item)
			}
		}
		// reception issues walk-in tokens in arrival order
		mins := make([]int, 4+r.IntN(10))
		for k := range mins {
			mins[k] = r.IntN(200)
		}
		sort.Ints(mins)
		for k, m := range mins {
			arr := d("2026-11-04").Add(9*time.Hour + time.Duration(m)*time.Minute)
			q = append(q, QueueItem{ID: int64(100 + k), Kind: "walkin", Token: 100 + k, Arrived: &arr})
		}
		now := d("2026-11-04").Add(9 * time.Hour)
		run2, lastWalkin := 0, 0
		seen := map[int64]bool{}
		for steps := 0; steps < 300 && now.Before(d("2026-11-04").Add(16*time.Hour)); steps++ {
			var waiting []QueueItem
			for _, x := range q {
				if !seen[x.ID] && (x.Arrived == nil || !x.Arrived.After(now)) {
					waiting = append(waiting, x)
				}
			}
			p, _, ns := Next(waiting, now, run2)
			for _, id := range ns {
				seen[id] = true
			}
			if p == nil {
				now = now.Add(5 * time.Minute)
				continue
			}
			seen[p.ID] = true
			if p.Kind == "walkin" {
				if p.Token < lastWalkin {
					t.Fatalf("run %d: walk-in %d called after %d", run, p.Token, lastWalkin)
				}
				lastWalkin = p.Token
				run2 = 0
			} else {
				for _, w := range waiting {
					if w.Kind == "walkin" && now.Sub(*w.Arrived) >= WalkinMaxWait {
						t.Fatalf("run %d: booked %d called while walk-in %d had waited %s", run, p.ID, w.Token, now.Sub(*w.Arrived))
					}
				}
				run2++
			}
			now = now.Add(time.Duration(6+r.IntN(8)) * time.Minute) // consultation
		}
		for _, x := range q {
			if x.Kind == "booked" && x.Arrived != nil && !seen[x.ID] {
				t.Fatalf("run %d: booked patient %d never seen", run, x.ID)
			}
		}
	}
}

func TestRecordAccess(t *testing.T) {
	cases := []struct {
		role   string
		rel    Relationship
		glass  string
		ok     bool
		reason string
	}{
		{Patient, Relationship{OwnRecord: true}, "", true, "own_record"},
		{Patient, Relationship{}, "", false, ""},
		{Doctor, Relationship{CareTeam: true}, "", true, "care_team"},
		{Doctor, Relationship{}, "", false, ""},
		{Doctor, Relationship{}, "short", false, ""},
		{Doctor, Relationship{}, "Unconscious in casualty, no family", true, "break_glass"},
		{Nurse, Relationship{AdmittedWard: true}, "", true, "admitted_ward"},
		{Nurse, Relationship{CareTeam: true}, "", false, ""}, // nurses' access comes from the ward
		{Receptionist, Relationship{}, "Need to check something", false, ""},
		{Admin, Relationship{}, "auditing a complaint", false, ""},
		{BloodBank, Relationship{}, "", false, ""},
	}
	for _, c := range cases {
		ok, reason, _ := RecordAccess(c.role, c.rel, c.glass)
		if ok != c.ok || reason != c.reason {
			t.Errorf("%s %+v %q: %v %q", c.role, c.rel, c.glass, ok, reason)
		}
	}
}
