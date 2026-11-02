package lottery

import (
	"errors"
	"fmt"
	"testing"
	"time"
)

func entries(n int) []Entry {
	es := make([]Entry, n)
	for i := range es {
		es[i] = Entry{ID: fmt.Sprintf("e%05d", i), AccountID: fmt.Sprintf("a%05d", i), Seats: 1 + i%4,
			At: time.Unix(int64(i), 0)}
	}
	return es
}

func TestVerifyDetectsTampering(t *testing.T) {
	es := entries(1000)
	r := []byte("beacon round randomness")
	c := Commit("show1", es, 42, time.Now())
	order := Order(es, r)
	if err := Verify(c, es, r, order); err != nil {
		t.Fatal(err)
	}
	// Operator swaps two positions.
	swapped := append([]string(nil), order...)
	swapped[0], swapped[7] = swapped[7], swapped[0]
	if err := Verify(c, es, r, swapped); !errors.Is(err, ErrOrder) {
		t.Fatalf("swap not detected: %v", err)
	}
	// Operator adds a friend after the list was committed.
	extra := append(append([]Entry(nil), es...), Entry{ID: "friend", AccountID: "x", Seats: 4})
	if err := Verify(c, extra, r, Order(extra, r)); !errors.Is(err, ErrCommitment) {
		t.Fatalf("late entry not detected: %v", err)
	}
	// Operator uses different randomness.
	if err := Verify(c, es, []byte("other"), order); !errors.Is(err, ErrOrder) {
		t.Fatalf("wrong randomness not detected: %v", err)
	}
}

func TestOrderIgnoresRegistrationTime(t *testing.T) {
	// Registering early or late must not change the queue position.
	es := entries(500)
	late := append([]Entry(nil), es...)
	for i := range late {
		late[i].At = late[i].At.Add(time.Duration(i) * time.Hour)
	}
	a, b := Order(es, []byte("r")), Order(late, []byte("r"))
	for i := range a {
		if a[i] != b[i] {
			t.Fatal("order depends on registration time")
		}
	}
}

func TestPositionIsUniform(t *testing.T) {
	// Over many beacon values, the first-registered entry should land in the
	// first quarter about 25% of the time, not more.
	es := entries(100)
	front := 0
	const runs = 4000
	for i := 0; i < runs; i++ {
		o := Order(es, []byte(fmt.Sprint(i)))
		for pos, id := range o {
			if id == "e00000" {
				if pos < 25 {
					front++
				}
				break
			}
		}
	}
	if p := float64(front) / runs; p < 0.22 || p > 0.28 {
		t.Fatalf("first registrant reached the front quarter %.3f of the time", p)
	}
}
