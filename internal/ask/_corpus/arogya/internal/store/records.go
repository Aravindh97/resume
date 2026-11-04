package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/Aravindh97/arogya/internal/auth"
	"github.com/Aravindh97/arogya/internal/domain"
)

// relationship works out the user's care relationship to a patient.
func (s *Store) relationship(ctx context.Context, q pgx.Tx, u *auth.User, patientID int64) (domain.Relationship, error) {
	var r domain.Relationship
	switch u.Role {
	case domain.Patient:
		r.OwnRecord = u.PatientID != nil && *u.PatientID == patientID
	case domain.Doctor:
		err := q.QueryRow(ctx, `select exists (select 1 from visits where patient_id = $1 and doctor_id = $2 and day >= $3 and status <> 'cancelled')
			or exists (select 1 from encounters where patient_id = $1 and doctor_id = $2)
			or exists (select 1 from admissions where patient_id = $1 and doctor_id = $2 and discharged_at is null)`, patientID, u.ID, s.today().AddDate(0, 0, -1)).Scan(&r.CareTeam)
		if err != nil {
			return r, err
		}
	case domain.Nurse:
		if u.WardID != nil {
			if err := q.QueryRow(ctx, `select exists (select 1 from admissions where patient_id = $1 and ward_id = $2 and discharged_at is null)`, patientID, *u.WardID).Scan(&r.AdmittedWard); err != nil {
				return r, err
			}
		}
	}
	return r, nil
}

// access checks and logs one access to a patient's record. The log entry is
// written in the same transaction as whatever the access does, so there is
// never a read or a change without its log line.
func (s *Store) access(ctx context.Context, tx pgx.Tx, u *auth.User, patientID int64, action, breakGlass, detail string) (string, error) {
	var exists bool
	if err := tx.QueryRow(ctx, `select exists (select 1 from patients where id = $1)`, patientID).Scan(&exists); err != nil {
		return "", err
	}
	if !exists {
		return "", ErrNotFound
	}
	rel, err := s.relationship(ctx, tx, u, patientID)
	if err != nil {
		return "", err
	}
	ok, reason, rerr := domain.RecordAccess(u.Role, rel, breakGlass)
	if !ok {
		if u.Role == domain.Doctor || u.Role == domain.Nurse {
			return "", &Problem{Field: "break_glass", Msg: rerr.Error(), Code: 403}
		}
		return "", ErrForbidden
	}
	if reason == "break_glass" {
		detail = strings.TrimSpace(breakGlass)
	}
	if _, err := tx.Exec(ctx, `insert into access_log (patient_id, user_id, action, reason, detail) values ($1, $2, $3, $4, $5)`, patientID, u.ID, action, reason, detail); err != nil {
		return "", err
	}
	if reason == "break_glass" {
		if err := notify(ctx, tx, "glass"); err != nil {
			return "", err
		}
	}
	return reason, nil
}

type Encounter struct {
	ID            int64          `json:"id"`
	At            time.Time      `json:"at"`
	Doctor        string         `json:"doctor"`
	Department    string         `json:"department"`
	Complaint     string         `json:"complaint"`
	Diagnosis     string         `json:"diagnosis"`
	Notes         string         `json:"notes"`
	Prescriptions []Prescription `json:"prescriptions"`
}

type Prescription struct {
	Drug string `json:"drug"`
	Dose string `json:"dose"`
	Days int    `json:"days"`
}

type Observation struct {
	Code  string    `json:"code"`
	Name  string    `json:"name"`
	Value float64   `json:"value"`
	Unit  string    `json:"unit"`
	At    time.Time `json:"at"`
}

type Admission struct {
	ID           int64      `json:"id"`
	PatientID    int64      `json:"patient_id"`
	Patient      string     `json:"patient"`
	MRN          string     `json:"mrn"`
	Doctor       string     `json:"doctor"`
	Ward         string     `json:"ward"`
	WardID       int64      `json:"ward_id"`
	Bed          string     `json:"bed,omitempty"`
	BedID        *int64     `json:"bed_id,omitempty"`
	Reason       string     `json:"reason"`
	Urgency      string     `json:"urgency"`
	RequestedAt  time.Time  `json:"requested_at"`
	AdmittedAt   *time.Time `json:"admitted_at,omitempty"`
	DischargedAt *time.Time `json:"discharged_at,omitempty"`
}

// Chart is a patient's record as a clinician sees it.
type Chart struct {
	Patient      Patient       `json:"patient"`
	Access       string        `json:"access"` // why this user could open it
	Encounters   []Encounter   `json:"encounters"`
	Observations []Observation `json:"observations"`
	Admissions   []Admission   `json:"admissions"`
	CanWrite     bool          `json:"can_write"`
}

// OpenRecord returns the chart and logs the access. breakGlass is the
// reason given for emergency access, when the user has no care
// relationship.
func (s *Store) OpenRecord(ctx context.Context, u *auth.User, patientID int64, breakGlass string) (*Chart, error) {
	c := &Chart{Encounters: []Encounter{}, Observations: []Observation{}, Admissions: []Admission{}}
	err := s.tx(ctx, func(tx pgx.Tx) error {
		reason, err := s.access(ctx, tx, u, patientID, "view", breakGlass, "")
		if err != nil {
			return err
		}
		c.Access = reason
		c.CanWrite = u.Role == domain.Doctor && reason == "care_team"
		p, err := s.scanPatient(tx.QueryRow(ctx, `select `+patientCols+` from patients p where p.id = $1`, patientID))
		if err != nil {
			return err
		}
		c.Patient = *p
		rows, err := tx.Query(ctx, `select e.id, e.at, u.name, coalesce(d.name, ''), e.complaint, e.diagnosis, e.notes,
			coalesce((select json_agg(json_build_object('drug', drug, 'dose', dose, 'days', days) order by id) from prescriptions where encounter_id = e.id), '[]')
			from encounters e join users u on u.id = e.doctor_id left join departments d on d.id = u.department_id where e.patient_id = $1 order by e.at desc`, patientID)
		if err != nil {
			return err
		}
		for rows.Next() {
			var e Encounter
			var rx []byte
			if err := rows.Scan(&e.ID, &e.At, &e.Doctor, &e.Department, &e.Complaint, &e.Diagnosis, &e.Notes, &rx); err != nil {
				rows.Close()
				return err
			}
			json.Unmarshal(rx, &e.Prescriptions)
			c.Encounters = append(c.Encounters, e)
		}
		rows.Close()
		rows, err = tx.Query(ctx, `select code, name, value, unit, at from observations where patient_id = $1 order by code, at`, patientID)
		if err != nil {
			return err
		}
		for rows.Next() {
			var o Observation
			if err := rows.Scan(&o.Code, &o.Name, &o.Value, &o.Unit, &o.At); err != nil {
				rows.Close()
				return err
			}
			c.Observations = append(c.Observations, o)
		}
		rows.Close()
		c.Admissions, err = admissions(ctx, tx, `a.patient_id = $1 order by a.requested_at desc`, patientID)
		return err
	})
	if err != nil {
		return nil, err
	}
	return c, nil
}

type NewEncounter struct {
	Complaint     string         `json:"complaint"`
	Diagnosis     string         `json:"diagnosis"`
	Notes         string         `json:"notes"`
	Prescriptions []Prescription `json:"prescriptions"`
	Observations  []Observation  `json:"observations"` // e.g. blood pressure, glucose measured today
}

// codes are the observations the demo records, with their LOINC codes.
var codes = map[string][2]string{
	"8480-6": {"Systolic blood pressure", "mm[Hg]"}, "8462-4": {"Diastolic blood pressure", "mm[Hg]"},
	"2339-0": {"Glucose", "mg/dL"}, "718-7": {"Haemoglobin", "g/dL"}, "4548-4": {"HbA1c", "%"}, "29463-7": {"Body weight", "kg"},
}

// AddEncounter records a consultation by the treating doctor.
func (s *Store) AddEncounter(ctx context.Context, u *auth.User, patientID int64, in NewEncounter) error {
	if u.Role != domain.Doctor {
		return ErrForbidden
	}
	if strings.TrimSpace(in.Diagnosis) == "" || len(in.Diagnosis) > 300 || len(in.Notes) > 4000 || len(in.Complaint) > 300 {
		return problem("diagnosis", "a diagnosis is required (up to 300 characters)")
	}
	if len(in.Prescriptions) > 20 || len(in.Observations) > 20 {
		return problem("", "too many items")
	}
	return s.tx(ctx, func(tx pgx.Tx) error {
		reason, err := s.access(ctx, tx, u, patientID, "update", "", "consultation")
		if err != nil {
			return err
		}
		if reason != "care_team" {
			return ErrForbidden
		}
		var id int64
		if err := tx.QueryRow(ctx, `insert into encounters (patient_id, doctor_id, at, complaint, diagnosis, notes) values ($1, $2, $3, $4, $5, $6) returning id`,
			patientID, u.ID, s.Now(), strings.TrimSpace(in.Complaint), strings.TrimSpace(in.Diagnosis), strings.TrimSpace(in.Notes)).Scan(&id); err != nil {
			return err
		}
		for _, p := range in.Prescriptions {
			if strings.TrimSpace(p.Drug) == "" || p.Days < 1 || p.Days > 365 {
				return problem("prescriptions", "each prescription needs a drug and 1 to 365 days")
			}
			if _, err := tx.Exec(ctx, `insert into prescriptions (encounter_id, drug, dose, days) values ($1, $2, $3, $4)`, id, p.Drug, p.Dose, p.Days); err != nil {
				return err
			}
		}
		for _, o := range in.Observations {
			c, ok := codes[o.Code]
			if !ok {
				return problem("observations", "unknown observation code %s", o.Code)
			}
			if _, err := tx.Exec(ctx, `insert into observations (patient_id, code, name, value, unit, at) values ($1, $2, $3, $4, $5, $6)`, patientID, o.Code, c[0], o.Value, c[1], s.Now()); err != nil {
				return err
			}
		}
		return notify(ctx, tx, fmt.Sprintf("patient:%d", patientID))
	})
}

// AccessEntry is one line of "who opened my record".
type AccessEntry struct {
	ID       int64     `json:"id"`
	At       time.Time `json:"at"`
	User     string    `json:"user"`
	Role     string    `json:"role"`
	Action   string    `json:"action"`
	Reason   string    `json:"reason"`
	Detail   string    `json:"detail"`
	Patient  string    `json:"patient,omitempty"`
	MRN      string    `json:"mrn,omitempty"`
	Review   string    `json:"review,omitempty"` // justified | not_justified, for break-glass
	Reviewer string    `json:"reviewer,omitempty"`
}

const accessCols = `l.id, l.at, u.name, u.role, l.action, l.reason, l.detail, p.name, p.mrn, coalesce(g.outcome, ''), coalesce(ru.name, '')`
const accessFrom = ` from access_log l join users u on u.id = l.user_id join patients p on p.id = l.patient_id
	left join glass_reviews g on g.access_id = l.id left join users ru on ru.id = g.reviewer_id`

func scanAccess(rows pgx.Rows) ([]AccessEntry, error) {
	defer rows.Close()
	out := []AccessEntry{}
	for rows.Next() {
		var a AccessEntry
		if err := rows.Scan(&a.ID, &a.At, &a.User, &a.Role, &a.Action, &a.Reason, &a.Detail, &a.Patient, &a.MRN, &a.Review, &a.Reviewer); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// WhoViewed is a patient's own access log, including the patient's own views.
func (s *Store) WhoViewed(ctx context.Context, u *auth.User) ([]AccessEntry, error) {
	if u.Role != domain.Patient {
		return nil, ErrForbidden
	}
	rows, err := s.DB.Query(ctx, `select `+accessCols+accessFrom+` where l.patient_id = $1 order by l.id desc limit 200`, *u.PatientID)
	if err != nil {
		return nil, err
	}
	return scanAccess(rows)
}

// AccessLog is every access, newest first, for admins; glassOnly filters
// to emergency accesses not yet reviewed.
func (s *Store) AccessLog(ctx context.Context, u *auth.User, glassOnly bool, before int64) ([]AccessEntry, error) {
	if u.Role != domain.Admin {
		return nil, ErrForbidden
	}
	if before <= 0 {
		before = 1 << 62
	}
	q := `select ` + accessCols + accessFrom + ` where l.id < $1`
	if glassOnly {
		q += ` and l.reason = 'break_glass' and g.access_id is null`
	}
	rows, err := s.DB.Query(ctx, q+` order by l.id desc limit 50`, before)
	if err != nil {
		return nil, err
	}
	return scanAccess(rows)
}

// ReviewGlass records an admin's review of an emergency access, once.
func (s *Store) ReviewGlass(ctx context.Context, u *auth.User, accessID int64, outcome, note string) error {
	if u.Role != domain.Admin {
		return ErrForbidden
	}
	if outcome != "justified" && outcome != "not_justified" {
		return problem("outcome", "choose justified or not justified")
	}
	return s.tx(ctx, func(tx pgx.Tx) error {
		var reason string
		var userID int64
		if err := tx.QueryRow(ctx, `select reason, user_id from access_log where id = $1`, accessID).Scan(&reason, &userID); errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		} else if err != nil {
			return err
		}
		if reason != "break_glass" {
			return problem("", "only emergency accesses are reviewed")
		}
		if userID == u.ID {
			return problem("", "someone else must review your own emergency access")
		}
		_, err := tx.Exec(ctx, `insert into glass_reviews (access_id, reviewer_id, outcome, note) values ($1, $2, $3, $4)`, accessID, u.ID, outcome, strings.TrimSpace(note))
		if uniqueViolation(err, "") {
			return conflict("already reviewed")
		}
		if err != nil {
			return err
		}
		return notify(ctx, tx, "glass")
	})
}

// MyRecord is the patient's own chart (logged like any other access).
func (s *Store) MyRecord(ctx context.Context, u *auth.User) (*Chart, error) {
	if u.Role != domain.Patient {
		return nil, ErrForbidden
	}
	return s.OpenRecord(ctx, u, *u.PatientID, "")
}

type Consents struct {
	SMSReminders bool `json:"sms_reminders"`
	ResearchUse  bool `json:"research_use"`
}

func (s *Store) SetConsents(ctx context.Context, u *auth.User, c Consents) error {
	if u.Role != domain.Patient {
		return ErrForbidden
	}
	_, err := s.DB.Exec(ctx, `update patients set sms_reminders = $2, research_use = $3 where id = $1`, *u.PatientID, c.SMSReminders, c.ResearchUse)
	return err
}

// FHIR exports a patient's demographics and observations as a FHIR R4
// Bundle (Patient plus Observation resources with LOINC codes), the format
// other hospital systems and India's ABDM health records use. The export
// itself is logged.
func (s *Store) FHIR(ctx context.Context, u *auth.User, patientID int64) (map[string]any, error) {
	var bundle map[string]any
	err := s.tx(ctx, func(tx pgx.Tx) error {
		if _, err := s.access(ctx, tx, u, patientID, "fhir_export", "", ""); err != nil {
			return err
		}
		p, err := s.scanPatient(tx.QueryRow(ctx, `select `+patientCols+` from patients p where p.id = $1`, patientID))
		if err != nil {
			return err
		}
		given, family, _ := strings.Cut(p.Name, " ")
		entries := []any{map[string]any{"fullUrl": "urn:uuid:patient-" + p.MRN, "resource": map[string]any{
			"resourceType": "Patient", "id": p.MRN,
			"identifier": []any{map[string]any{"system": "https://arogya.example/mrn", "value": p.MRN}},
			"name":       []any{map[string]any{"given": []string{given}, "family": family, "text": p.Name}},
			"gender":     map[string]string{"female": "female", "male": "male", "other": "other"}[p.Sex],
			"birthDate":  p.BirthDate,
			"telecom":    []any{map[string]any{"system": "phone", "value": p.Phone}},
		}}}
		rows, err := tx.Query(ctx, `select id, code, name, value, unit, at from observations where patient_id = $1 order by at`, patientID)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var id int64
			var o Observation
			if err := rows.Scan(&id, &o.Code, &o.Name, &o.Value, &o.Unit, &o.At); err != nil {
				return err
			}
			entries = append(entries, map[string]any{"fullUrl": fmt.Sprintf("urn:uuid:obs-%d", id), "resource": map[string]any{
				"resourceType": "Observation", "id": fmt.Sprintf("obs-%d", id), "status": "final",
				"code":              map[string]any{"coding": []any{map[string]any{"system": "http://loinc.org", "code": o.Code, "display": o.Name}}, "text": o.Name},
				"subject":           map[string]any{"reference": "urn:uuid:patient-" + p.MRN},
				"effectiveDateTime": o.At.Format(time.RFC3339),
				"valueQuantity":     map[string]any{"value": o.Value, "unit": o.Unit, "system": "http://unitsofmeasure.org", "code": o.Unit},
			}})
		}
		bundle = map[string]any{"resourceType": "Bundle", "type": "collection", "timestamp": s.Now().Format(time.RFC3339), "entry": entries}
		return rows.Err()
	})
	return bundle, err
}
