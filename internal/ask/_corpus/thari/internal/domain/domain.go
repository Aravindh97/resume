// Package domain holds Thari's business rules, free of HTTP and SQL so they
// can be tested directly: production stages and how progress may move,
// delivery risk, AQL inspection sampling, and who may do what.
package domain

import (
	"errors"
	"fmt"
	"time"
)

// Stages are the production steps a garment order moves through, in order.
// Each records how many pieces have completed it.
var Stages = []string{"fabric", "cutting", "stitching", "checking", "packing"}

var StageLabel = map[string]string{
	"fabric": "Fabric received", "cutting": "Cut", "stitching": "Stitched", "checking": "Checked", "packing": "Packed",
}

func StageIndex(s string) int {
	for i, x := range Stages {
		if x == s {
			return i
		}
	}
	return -1
}

var (
	ErrUnknownStage = errors.New("unknown stage")
	ErrNotAllowed   = errors.New("not allowed for your role")
)

// ProgressError explains why a progress update was refused.
type ProgressError struct{ Msg string }

func (e *ProgressError) Error() string { return e.Msg }

// ApplyProgress checks adding delta pieces to stage, given the current done
// counts per stage (indexed like Stages) and the order quantity. A stage can
// never get ahead of the one before it (nothing is stitched before it is
// cut), never exceed the order quantity, and never drop below the stage
// after it (a correction cannot un-cut pieces that were already stitched).
// It returns the new done count for the stage.
func ApplyProgress(done []int, quantity int, stage string, delta int) (int, error) {
	i := StageIndex(stage)
	if i < 0 {
		return 0, ErrUnknownStage
	}
	if delta == 0 {
		return 0, &ProgressError{"enter a number of pieces other than 0"}
	}
	n := done[i] + delta
	limit, limitName := quantity, "the order quantity"
	if i > 0 && done[i-1] < limit {
		limit, limitName = done[i-1], fmt.Sprintf("%s (%d)", StageLabel[Stages[i-1]], done[i-1])
	}
	switch {
	case n < 0:
		return 0, &ProgressError{fmt.Sprintf("only %d pieces are recorded as %s", done[i], StageLabel[stage])}
	case n > limit:
		return 0, &ProgressError{fmt.Sprintf("%s would be %d, more than %s", StageLabel[stage], n, limitName)}
	case i+1 < len(Stages) && n < done[i+1]:
		return 0, &ProgressError{fmt.Sprintf("%s cannot go below %s (%d)", StageLabel[stage], StageLabel[Stages[i+1]], done[i+1])}
	}
	return n, nil
}

// Completion is the share of all stage work done, 0..1.
func Completion(done []int, quantity int) float64 {
	if quantity <= 0 {
		return 0
	}
	t := 0
	for _, d := range done {
		t += d
	}
	return float64(t) / float64(quantity*len(Stages))
}

// CurrentStage is the first stage not yet complete, or "ready" when every
// stage has the full quantity.
func CurrentStage(done []int, quantity int) string {
	for i, d := range done {
		if d < quantity {
			return Stages[i]
		}
	}
	return "ready"
}

type Risk string

const (
	OnTrack Risk = "on_track"
	AtRisk  Risk = "at_risk"
	Late    Risk = "late"
	Done    Risk = "done"
)

// RiskOf compares progress with time. An open order is late once its due
// date has passed, and at risk when completion trails the share of the
// production window already used by more than 15 points, or when less than
// 3 days remain and it is under 80% complete.
func RiskOf(status string, orderedOn, due, today time.Time, completion float64) Risk {
	if status != "open" {
		return Done
	}
	if today.After(due) {
		return Late
	}
	window := due.Sub(orderedOn).Hours()
	used := 1.0
	if window > 0 {
		used = today.Sub(orderedOn).Hours() / window
	}
	if completion < used-0.15 || due.Sub(today) < 72*time.Hour && completion < 0.8 {
		return AtRisk
	}
	return OnTrack
}

// AQLPlan is a single-sampling plan for a final inspection.
type AQLPlan struct {
	LotSize     int    `json:"lot_size"`
	CodeLetter  string `json:"code_letter"`
	SampleSize  int    `json:"sample_size"`
	AcceptMajor int    `json:"accept_major"` // AQL 2.5
	AcceptMinor int    `json:"accept_minor"` // AQL 4.0
}

// aqlRows: ISO 2859-1, general inspection level II, single sampling, normal
// inspection, with the table's arrows resolved, at AQL 2.5 (major defects)
// and 4.0 (minor), the levels most garment buyers use. Critical defects are
// never accepted. For lots of 8 pieces or fewer every piece is inspected.
var aqlRows = []struct {
	maxLot       int
	letter       string
	sample       int
	major, minor int
}{
	{15, "B", 3, 0, 0}, {25, "C", 5, 0, 0}, {50, "D", 8, 0, 1}, {90, "E", 13, 1, 1},
	{150, "F", 20, 1, 2}, {280, "G", 32, 2, 3}, {500, "H", 50, 3, 5}, {1200, "J", 80, 5, 7},
	{3200, "K", 125, 7, 10}, {10000, "L", 200, 10, 14}, {35000, "M", 315, 14, 21},
	{150000, "N", 500, 21, 21}, {500000, "P", 800, 21, 21}, {1 << 31, "Q", 1250, 21, 21},
}

func PlanFor(lot int) AQLPlan {
	if lot <= 8 {
		return AQLPlan{LotSize: lot, CodeLetter: "A", SampleSize: lot}
	}
	for _, r := range aqlRows {
		if lot <= r.maxLot {
			return AQLPlan{lot, r.letter, min(r.sample, lot), r.major, r.minor}
		}
	}
	panic("unreachable")
}

// Judge returns pass when critical defects are zero and major and minor
// defects are within the plan's acceptance numbers.
func (p AQLPlan) Judge(critical, major, minor int) string {
	if critical == 0 && major <= p.AcceptMajor && minor <= p.AcceptMinor {
		return "pass"
	}
	return "fail"
}

// Roles.
const (
	Admin        = "admin"
	Merchandiser = "merchandiser"
	Production   = "production"
	Buyer        = "buyer"
)

var Roles = []string{Admin, Merchandiser, Production, Buyer}

type Action string

const (
	ViewOrder       Action = "view_order"
	ViewPrice       Action = "view_price"
	CreateOrder     Action = "create_order"
	EditOrder       Action = "edit_order"
	AddProgress     Action = "add_progress"
	CorrectProgress Action = "correct_progress" // negative deltas
	Inspect         Action = "inspect"
	Comment         Action = "comment"
	UploadFile      Action = "upload_file"
	Ship            Action = "ship"
	ManageUsers     Action = "manage_users"
	ViewAudit       Action = "view_audit"
	ViewInternal    Action = "view_internal" // internal notes and events hidden from buyers
)

var allowed = map[string]map[Action]bool{
	Admin: {ViewOrder: true, ViewPrice: true, CreateOrder: true, EditOrder: true, AddProgress: true, CorrectProgress: true,
		Inspect: true, Comment: true, UploadFile: true, Ship: true, ManageUsers: true, ViewAudit: true, ViewInternal: true},
	Merchandiser: {ViewOrder: true, ViewPrice: true, CreateOrder: true, EditOrder: true, AddProgress: true, CorrectProgress: true,
		Comment: true, UploadFile: true, Ship: true, ViewInternal: true},
	Production: {ViewOrder: true, AddProgress: true, Inspect: true, Comment: true, ViewInternal: true},
	Buyer:      {ViewOrder: true, Comment: true},
}

// Principal is the signed-in user as the rules see them.
type Principal struct {
	UserID int64  `json:"id"`
	OrgID  int64  `json:"org_id"`
	Role   string `json:"role"`
	// FactoryID is the factory whose orders the user works with: their own
	// org for factory staff, the factory a buyer orders from for buyers.
	FactoryID int64 `json:"factory_id"`
}

// Can reports whether p may do a on an order of the given factory and buyer.
// Buyers only ever see their own company's orders; factory staff only their
// factory's.
func (p Principal) Can(a Action, factoryID, buyerID int64) bool {
	if !allowed[p.Role][a] || factoryID != p.FactoryID {
		return false
	}
	if p.Role == Buyer {
		return buyerID == p.OrgID
	}
	return true
}

// CanDo is Can for actions that are not about one order (users, audit, new orders).
func (p Principal) CanDo(a Action) bool { return allowed[p.Role][a] }
