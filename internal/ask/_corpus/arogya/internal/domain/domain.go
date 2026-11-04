// Package domain holds Arogya's clinical and operational rules, separate
// from HTTP and SQL so each can be tested on its own.
//
// The medical rules here are the standard textbook ones, simplified for a
// demo on synthetic data. They are not clinical guidance.
package domain

import (
	"fmt"
	"sort"
	"strings"
	"time"
)

// ---- Blood ----------------------------------------------------------------

var Groups = []string{"O-", "O+", "A-", "A+", "B-", "B+", "AB-", "AB+"}

func split(g string) (abo string, rhPos bool) {
	return strings.TrimRight(g, "+-"), strings.HasSuffix(g, "+")
}

// redCellOK: donor red cells carry no antigen the recipient lacks.
func redCellOK(recipient, donor string) bool {
	ra, rpos := split(recipient)
	da, dpos := split(donor)
	if dpos && !rpos {
		return false
	}
	switch da {
	case "O":
		return true
	case "A", "B":
		return ra == da || ra == "AB"
	case "AB":
		return ra == "AB"
	}
	return false
}

// plasmaOK: donor plasma carries no antibody against the recipient's cells
// (the reverse of red cells; Rh does not matter for plasma).
func plasmaOK(recipient, donor string) bool {
	ra, _ := split(recipient)
	da, _ := split(donor)
	switch da {
	case "AB":
		return true
	case "A", "B":
		return ra == da || ra == "O"
	case "O":
		return ra == "O"
	}
	return false
}

// Compatible reports whether a unit of the donor group can go to the recipient.
// Platelets follow the plasma rule here, which is the usual preference.
func Compatible(component, recipient, donor string) bool {
	if component == "rbc" {
		return redCellOK(recipient, donor)
	}
	return plasmaOK(recipient, donor)
}

// Recipient is what allocation needs to know about the patient.
type Recipient struct {
	Group string
	Sex   string
	Age   int
}

// Unit is a blood bag in stock.
type Unit struct {
	ID        int64
	Group     string
	Component string
	ExpiresOn time.Time
}

// Pick chooses units for a request. Only compatible, unexpired units of the
// right component qualify (Rh-negative patients never get Rh-positive red
// cells here). Platelets from Rh-positive donors carry a few red cells, so
// Rh-negative girls and women under 50 get Rh-negative platelets unless it
// is an emergency, to avoid anti-D antibodies that can harm a later
// pregnancy. Among what qualifies, the
// patient's own group comes first, then other compatible groups; within
// each, the unit that expires soonest goes first, so stock is not wasted.
func Pick(r Recipient, component, urgency string, units []Unit, n int, today time.Time) []Unit {
	_, rpos := split(r.Group)
	protect := component == "platelets" && !rpos && r.Sex == "female" && r.Age < 50 && urgency != "emergency"
	var ok []Unit
	for _, u := range units {
		if u.Component != component || !u.ExpiresOn.After(today) || !Compatible(component, r.Group, u.Group) {
			continue
		}
		if _, upos := split(u.Group); protect && upos {
			continue
		}
		ok = append(ok, u)
	}
	sort.SliceStable(ok, func(i, j int) bool {
		si, sj := ok[i].Group == r.Group, ok[j].Group == r.Group
		if si != sj {
			return si
		}
		if !ok[i].ExpiresOn.Equal(ok[j].ExpiresOn) {
			return ok[i].ExpiresOn.Before(ok[j].ExpiresOn)
		}
		return ok[i].ID < ok[j].ID
	})
	if len(ok) > n {
		ok = ok[:n]
	}
	return ok
}

// DonorCanGive: whole-blood donors wait 90 days between donations (men) or
// 120 days (women), as in India's national blood transfusion guidelines.
func DonorCanGive(sex string, lastDonated *time.Time, today time.Time) (bool, time.Time) {
	if lastDonated == nil {
		return true, today
	}
	gap := 90
	if sex == "female" {
		gap = 120
	}
	next := lastDonated.AddDate(0, 0, gap)
	return !today.Before(next), next
}

// ---- OPD queue --------------------------------------------------------------

// QueueItem is a waiting visit, as the queue logic sees it.
type QueueItem struct {
	ID      int64
	Kind    string // booked | walkin
	Token   int
	Slot    *time.Time // booked: appointment time
	Arrived *time.Time // booked: checked in at reception; walk-in: when the token was issued
}

// The rules for who goes next.
const (
	BookedPerWalkin = 2                // after this many booked patients in a row, a waiting walk-in goes
	WalkinMaxWait   = 45 * time.Minute // a walk-in waiting this long goes next regardless
	NoShowAfter     = 15 * time.Minute // a booked patient not checked in this long after their slot is a no-show
)

// Next picks who the doctor sees next and says why. lastBookedRun is how
// many booked patients were called in a row just before. It also returns
// booked visits that should now be marked no-show.
func Next(waiting []QueueItem, now time.Time, lastBookedRun int) (pick *QueueItem, why string, noShows []int64) {
	var booked, walkins []QueueItem
	for _, q := range waiting {
		switch {
		case q.Kind == "walkin":
			walkins = append(walkins, q)
		case q.Arrived != nil && !q.Slot.After(now.Add(10*time.Minute)): // checked in, and the slot is due (or 10 min early)
			booked = append(booked, q)
		case q.Arrived == nil && now.Sub(*q.Slot) > NoShowAfter:
			noShows = append(noShows, q.ID)
		}
	}
	sort.Slice(booked, func(i, j int) bool { return booked[i].Slot.Before(*booked[j].Slot) })
	sort.Slice(walkins, func(i, j int) bool { return walkins[i].Token < walkins[j].Token })
	switch {
	case len(walkins) > 0 && now.Sub(*walkins[0].Arrived) >= WalkinMaxWait:
		return &walkins[0], fmt.Sprintf("walk-in waiting %d min", int(now.Sub(*walkins[0].Arrived).Minutes())), noShows
	case len(booked) > 0 && (lastBookedRun < BookedPerWalkin || len(walkins) == 0):
		return &booked[0], "appointment at " + booked[0].Slot.Format("15:04"), noShows
	case len(walkins) > 0:
		why := "next walk-in"
		if len(booked) > 0 {
			why = fmt.Sprintf("walk-in turn after %d appointments", lastBookedRun)
		}
		return &walkins[0], why, noShows
	}
	return nil, "", noShows
}

// Slots are the bookable appointment times for a day: 09:00 to 12:45,
// every 15 minutes.
func Slots(day time.Time) []time.Time {
	var out []time.Time
	for t := day.Add(9 * time.Hour); t.Before(day.Add(13 * time.Hour)); t = t.Add(15 * time.Minute) {
		out = append(out, t)
	}
	return out
}

// ---- Who may see a record -----------------------------------------------------

const (
	Admin        = "admin"
	Receptionist = "receptionist"
	Doctor       = "doctor"
	Nurse        = "nurse"
	BloodBank    = "bloodbank"
	Patient      = "patient"
)

// Relationship is what links a user to a patient right now.
type Relationship struct {
	OwnRecord    bool // the user is this patient
	CareTeam     bool // doctor: a visit with them today or ahead, a past encounter, or an admission under them
	AdmittedWard bool // nurse: the patient is admitted to their ward
}

// RecordAccess decides whether a user may open a patient's clinical record
// and records why. Receptionists, blood bank staff and admins never read
// clinical records (admins audit access instead). Doctors and nurses
// without a care relationship can break the glass in an emergency: they
// must give a reason, they get access, and the access is flagged for
// review and shown to the patient.
func RecordAccess(role string, rel Relationship, breakGlassReason string) (ok bool, reason string, err error) {
	switch {
	case role == Patient && rel.OwnRecord:
		return true, "own_record", nil
	case role == Doctor && rel.CareTeam:
		return true, "care_team", nil
	case role == Nurse && rel.AdmittedWard:
		return true, "admitted_ward", nil
	case role == Doctor || role == Nurse:
		r := strings.TrimSpace(breakGlassReason)
		if r == "" {
			return false, "", fmt.Errorf("you are not caring for this patient; emergency access needs a reason")
		}
		if len(r) < 10 {
			return false, "", fmt.Errorf("describe the emergency (at least 10 characters)")
		}
		return true, "break_glass", nil
	}
	return false, "", fmt.Errorf("your role cannot open clinical records")
}

// Age in whole years on a date.
func Age(birth, on time.Time) int {
	a := on.Year() - birth.Year()
	if on.YearDay() < birth.YearDay() {
		a--
	}
	return a
}
