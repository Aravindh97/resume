// Package store is Arogya's data access. Every function takes the signed-in
// user and checks what they may do; records a user may not see are
// reported as not found.
package store

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Aravindh97/arogya/internal/auth"
	"github.com/Aravindh97/arogya/internal/domain"
)

var (
	ErrNotFound  = errors.New("not found")
	ErrForbidden = errors.New("not allowed for your role")
)

// Problem is a request the rules refuse, with the field it is about.
type Problem struct {
	Field string `json:"field,omitempty"`
	Msg   string `json:"error"`
	Code  int    `json:"-"` // HTTP status; 422 if zero
}

func (e *Problem) Error() string { return e.Msg }

func problem(field, format string, a ...any) error {
	return &Problem{Field: field, Msg: fmt.Sprintf(format, a...)}
}
func conflict(format string, a ...any) error {
	return &Problem{Msg: fmt.Sprintf(format, a...), Code: 409}
}

type Store struct {
	DB  *pgxpool.Pool
	Now func() time.Time // wall clock, Asia/Kolkata
}

func (s *Store) today() time.Time {
	n := s.Now()
	y, m, d := n.Date()
	return time.Date(y, m, d, 0, 0, 0, 0, n.Location())
}

func (s *Store) tx(ctx context.Context, f func(pgx.Tx) error) error {
	tx, err := s.DB.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if err := f(tx); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// notify tells every server instance (and through them, browsers) that a
// topic changed. It is sent when the transaction commits.
func notify(ctx context.Context, tx pgx.Tx, topics ...string) error {
	for _, t := range topics {
		if _, err := tx.Exec(ctx, `select pg_notify('arogya', $1)`, t); err != nil {
			return err
		}
	}
	return nil
}

func uniqueViolation(err error, constraint string) bool {
	var pe *pgconn.PgError
	return errors.As(err, &pe) && pe.Code == "23505" && (constraint == "" || pe.ConstraintName == constraint)
}

func is(u *auth.User, roles ...string) bool {
	for _, r := range roles {
		if u.Role == r {
			return true
		}
	}
	return false
}

// ---- Patients (demographics only; clinical records are in records.go) ----

type Patient struct {
	ID           int64  `json:"id"`
	MRN          string `json:"mrn"`
	Name         string `json:"name"`
	BirthDate    string `json:"birth_date"`
	Age          int    `json:"age"`
	Sex          string `json:"sex"`
	Phone        string `json:"phone"`
	Area         string `json:"area"`
	BloodGroup   string `json:"blood_group,omitempty"`
	SMSReminders bool   `json:"sms_reminders"`
	ResearchUse  bool   `json:"research_use"`
}

const patientCols = `p.id, p.mrn, p.name, p.birth_date, p.sex, p.phone, p.area, coalesce(p.blood_group, ''), p.sms_reminders, p.research_use`

func (s *Store) scanPatient(row pgx.Row) (*Patient, error) {
	var p Patient
	var birth time.Time
	err := row.Scan(&p.ID, &p.MRN, &p.Name, &birth, &p.Sex, &p.Phone, &p.Area, &p.BloodGroup, &p.SMSReminders, &p.ResearchUse)
	p.BirthDate, p.Age = birth.Format(time.DateOnly), domain.Age(birth, s.today())
	return &p, err
}

// SearchPatients finds patients by name, MRN or phone, for reception and
// clinical staff. It returns demographics only; opening the clinical
// record is a separate, logged step.
func (s *Store) SearchPatients(ctx context.Context, u *auth.User, q string) ([]Patient, error) {
	if !is(u, domain.Receptionist, domain.Doctor, domain.Nurse, domain.BloodBank, domain.Admin) {
		return nil, ErrForbidden
	}
	q = strings.TrimSpace(q)
	if len(q) < 2 {
		return []Patient{}, nil
	}
	like := "%" + strings.NewReplacer(`\`, `\\`, "%", `\%`, "_", `\_`).Replace(q) + "%"
	rows, err := s.DB.Query(ctx, `select `+patientCols+` from patients p where p.name ilike $1 or p.mrn ilike $1 or p.phone like $1 order by p.name limit 20`, like)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Patient{}
	for rows.Next() {
		p, err := s.scanPatient(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *p)
	}
	return out, rows.Err()
}

type NewPatient struct {
	Name      string `json:"name"`
	BirthDate string `json:"birth_date"`
	Sex       string `json:"sex"`
	Phone     string `json:"phone"`
	Area      string `json:"area"`
}

func (s *Store) RegisterPatient(ctx context.Context, u *auth.User, in NewPatient) (*Patient, error) {
	if !is(u, domain.Receptionist, domain.Admin) {
		return nil, ErrForbidden
	}
	in.Name, in.Phone, in.Area = strings.TrimSpace(in.Name), strings.TrimSpace(in.Phone), strings.TrimSpace(in.Area)
	b, err := time.Parse(time.DateOnly, in.BirthDate)
	switch {
	case in.Name == "" || len(in.Name) > 100:
		return nil, problem("name", "name is required")
	case err != nil || b.After(s.today()) || b.Before(s.today().AddDate(-120, 0, 0)):
		return nil, problem("birth_date", "enter a valid date of birth")
	case in.Sex != "female" && in.Sex != "male" && in.Sex != "other":
		return nil, problem("sex", "choose one")
	case len(strings.Trim(in.Phone, "+0123456789 ")) > 0 || len(in.Phone) < 10:
		return nil, problem("phone", "enter a 10-digit mobile number")
	}
	var id int64
	err = s.tx(ctx, func(tx pgx.Tx) error {
		// The number comes from a sequence, so concurrent registrations never collide.
		return tx.QueryRow(ctx, `insert into patients (mrn, name, birth_date, sex, phone, area)
			values ('AH-' || $1::int || '-' || lpad(nextval('patient_mrn_seq')::text, 5, '0'), $2, $3, $4, $5, $6) returning id`,
			s.today().Year(), in.Name, b, in.Sex, in.Phone, in.Area).Scan(&id)
	})
	if err != nil {
		return nil, err
	}
	return s.scanPatient(s.DB.QueryRow(ctx, `select `+patientCols+` from patients p where p.id = $1`, id))
}

// ---- OPD ---------------------------------------------------------------

type Doctor struct {
	ID         int64    `json:"id"`
	Name       string   `json:"name"`
	Department string   `json:"department"`
	FreeSlots  []string `json:"free_slots"` // HH:MM on the asked day
}

func (s *Store) Doctors(ctx context.Context, day time.Time) ([]Doctor, error) {
	rows, err := s.DB.Query(ctx, `select u.id, u.name, d.name, coalesce(array(select to_char(v.slot, 'HH24:MI') from visits v
		where v.doctor_id = u.id and v.day = $1 and v.kind = 'booked' and v.status <> 'cancelled'), '{}')
		from users u join departments d on d.id = u.department_id where u.role = 'doctor' and u.active order by d.name, u.name`, day)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	now := s.Now()
	out := []Doctor{}
	for rows.Next() {
		var d Doctor
		var taken []string
		if err := rows.Scan(&d.ID, &d.Name, &d.Department, &taken); err != nil {
			return nil, err
		}
		busy := map[string]bool{}
		for _, t := range taken {
			busy[t] = true
		}
		d.FreeSlots = []string{}
		for _, t := range domain.Slots(day) {
			if !busy[t.Format("15:04")] && t.After(now) {
				d.FreeSlots = append(d.FreeSlots, t.Format("15:04"))
			}
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

type Visit struct {
	ID        int64      `json:"id"`
	PatientID int64      `json:"patient_id"`
	Patient   string     `json:"patient"`
	MRN       string     `json:"mrn"`
	DoctorID  int64      `json:"doctor_id"`
	Doctor    string     `json:"doctor"`
	Day       string     `json:"day"`
	Slot      string     `json:"slot,omitempty"`
	Kind      string     `json:"kind"`
	Token     int        `json:"token"`
	Status    string     `json:"status"`
	ArrivedAt *time.Time `json:"arrived_at,omitempty"`
	CalledAt  *time.Time `json:"called_at,omitempty"`
	Why       string     `json:"why,omitempty"` // why this patient was called next
}

const visitCols = `v.id, v.patient_id, p.name, p.mrn, v.doctor_id, u.name, v.day::text, coalesce(to_char(v.slot, 'HH24:MI'), ''), v.kind, v.token, v.status, v.arrived_at, v.called_at`
const visitFrom = ` from visits v join patients p on p.id = v.patient_id join users u on u.id = v.doctor_id`

func scanVisit(row pgx.Row) (*Visit, error) {
	var v Visit
	err := row.Scan(&v.ID, &v.PatientID, &v.Patient, &v.MRN, &v.DoctorID, &v.Doctor, &v.Day, &v.Slot, &v.Kind, &v.Token, &v.Status, &v.ArrivedAt, &v.CalledAt)
	return &v, err
}

func (s *Store) visits(ctx context.Context, where string, args ...any) ([]Visit, error) {
	rows, err := s.DB.Query(ctx, `select `+visitCols+visitFrom+` where `+where, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Visit{}
	for rows.Next() {
		v, err := scanVisit(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *v)
	}
	return out, rows.Err()
}

// lockDoctorDay serialises token numbers and queue decisions for one
// doctor's day.
func lockDoctorDay(ctx context.Context, tx pgx.Tx, doctor int64, day time.Time) error {
	_, err := tx.Exec(ctx, `select pg_advisory_xact_lock(hashtextextended($1, 0))`, fmt.Sprintf("opd:%d:%s", doctor, day.Format(time.DateOnly)))
	return err
}

type Booking struct {
	PatientID int64  `json:"patient_id"` // ignored for patients booking for themselves
	DoctorID  int64  `json:"doctor_id"`
	Day       string `json:"day"`
	Slot      string `json:"slot"` // HH:MM; empty for a walk-in token (reception only, today)
}

// Book takes a slot, or issues a walk-in token. Two people booking the last
// free slot at the same moment: the unique index lets exactly one succeed.
func (s *Store) Book(ctx context.Context, u *auth.User, in Booking) (*Visit, error) {
	if u.Role == domain.Patient {
		in.PatientID = *u.PatientID
		if in.Slot == "" {
			return nil, problem("slot", "choose a time")
		}
	} else if !is(u, domain.Receptionist, domain.Admin) {
		return nil, ErrForbidden
	}
	day, err := time.ParseInLocation(time.DateOnly, in.Day, s.Now().Location())
	if err != nil || day.Before(s.today()) || day.After(s.today().AddDate(0, 0, 30)) {
		return nil, problem("day", "choose a day in the next 30 days")
	}
	var slotAt *time.Time
	kind := "walkin"
	if in.Slot != "" {
		kind = "booked"
		for _, t := range domain.Slots(day) {
			if t.Format("15:04") == in.Slot {
				t := t
				slotAt = &t
			}
		}
		if slotAt == nil {
			return nil, problem("slot", "not a bookable time")
		}
		if !slotAt.After(s.Now()) {
			return nil, problem("slot", "that time has passed")
		}
	} else if !day.Equal(s.today()) {
		return nil, problem("day", "walk-in tokens are for today")
	}
	var id int64
	err = s.tx(ctx, func(tx pgx.Tx) error {
		var ok bool
		tx.QueryRow(ctx, `select exists (select 1 from users where id = $1 and role = 'doctor' and active)`, in.DoctorID).Scan(&ok)
		if !ok {
			return problem("doctor_id", "choose a doctor")
		}
		if err := lockDoctorDay(ctx, tx, in.DoctorID, day); err != nil {
			return err
		}
		var token int
		if err := tx.QueryRow(ctx, `select coalesce(max(token), 0) + 1 from visits where doctor_id = $1 and day = $2`, in.DoctorID, day).Scan(&token); err != nil {
			return err
		}
		var arrived *time.Time
		if kind == "walkin" {
			n := s.Now()
			arrived = &n
		}
		var slot any
		if slotAt != nil {
			slot = slotAt.Format("15:04")
		}
		err := tx.QueryRow(ctx, `insert into visits (patient_id, doctor_id, day, slot, kind, token, arrived_at) values ($1, $2, $3, $4, $5, $6, $7) returning id`,
			in.PatientID, in.DoctorID, day, slot, kind, token, arrived).Scan(&id)
		switch {
		case uniqueViolation(err, "visits_one_per_slot"):
			return conflict("someone else just took %s; choose another time", in.Slot)
		case uniqueViolation(err, "visits_one_per_patient"):
			return conflict("this patient already has a visit with this doctor that day")
		case err != nil:
			return err
		}
		var name, phone, doctor string
		var sms bool
		tx.QueryRow(ctx, `select p.name, p.phone, p.sms_reminders, u.name from patients p, users u where p.id = $1 and u.id = $2`, in.PatientID, in.DoctorID).Scan(&name, &phone, &sms, &doctor)
		if sms { // consent to reminders
			msg := fmt.Sprintf("Arogya Hospital: %s, your token %d with %s", name, token, doctor)
			if slotAt != nil {
				msg += fmt.Sprintf(" on %s at %s. Please check in at reception 10 minutes early.", day.Format("2 Jan"), in.Slot)
			} else {
				msg += " today. Watch the screen in the waiting hall."
			}
			if _, err := tx.Exec(ctx, `insert into outbox (patient_id, phone, body) values ($1, $2, $3)`, in.PatientID, phone, msg); err != nil {
				return err
			}
		}
		return notify(ctx, tx, fmt.Sprintf("opd:%d", in.DoctorID))
	})
	if err != nil {
		return nil, err
	}
	return scanVisit(s.DB.QueryRow(ctx, `select `+visitCols+visitFrom+` where v.id = $1`, id))
}

// CheckIn records a booked patient's arrival at reception.
func (s *Store) CheckIn(ctx context.Context, u *auth.User, visitID int64) error {
	if !is(u, domain.Receptionist, domain.Admin) {
		return ErrForbidden
	}
	return s.tx(ctx, func(tx pgx.Tx) error {
		var doctor int64
		err := tx.QueryRow(ctx, `update visits set arrived_at = $2 where id = $1 and status = 'waiting' and arrived_at is null and day = $3 returning doctor_id`,
			visitID, s.Now(), s.today()).Scan(&doctor)
		if errors.Is(err, pgx.ErrNoRows) {
			return problem("", "this visit is not waiting for check-in today")
		}
		if err != nil {
			return err
		}
		return notify(ctx, tx, fmt.Sprintf("opd:%d", doctor))
	})
}

// Cancel frees a booked slot (the patient's own, or by reception).
func (s *Store) Cancel(ctx context.Context, u *auth.User, visitID int64) error {
	return s.tx(ctx, func(tx pgx.Tx) error {
		q := `update visits set status = 'cancelled' where id = $1 and status = 'waiting'`
		args := []any{visitID}
		switch {
		case u.Role == domain.Patient:
			q += ` and patient_id = $2`
			args = append(args, *u.PatientID)
		case !is(u, domain.Receptionist, domain.Admin):
			return ErrForbidden
		}
		var doctor int64
		if err := tx.QueryRow(ctx, q+` returning doctor_id`, args...).Scan(&doctor); errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		} else if err != nil {
			return err
		}
		return notify(ctx, tx, fmt.Sprintf("opd:%d", doctor))
	})
}

// Queue is a doctor's day: who is with them, who waits, who was seen.
type Queue struct {
	Doctor  string  `json:"doctor"`
	Current *Visit  `json:"current"`
	Waiting []Visit `json:"waiting"`
	Done    []Visit `json:"done"`
}

func (s *Store) Queue(ctx context.Context, u *auth.User, doctorID int64) (*Queue, error) {
	if u.Role == domain.Doctor {
		doctorID = u.ID
	} else if !is(u, domain.Receptionist, domain.Admin) {
		return nil, ErrForbidden
	}
	all, err := s.visits(ctx, `v.doctor_id = $1 and v.day = $2 and v.status <> 'cancelled' order by v.token`, doctorID, s.today())
	if err != nil {
		return nil, err
	}
	q := &Queue{Waiting: []Visit{}, Done: []Visit{}}
	s.DB.QueryRow(ctx, `select name from users where id = $1`, doctorID).Scan(&q.Doctor)
	for _, v := range all {
		v := v
		switch v.Status {
		case "called":
			q.Current = &v
		case "waiting":
			q.Waiting = append(q.Waiting, v)
		default:
			q.Done = append(q.Done, v)
		}
	}
	return q, nil
}

// CallNext finishes the current patient and calls the next one, chosen by
// domain.Next, marking booked patients who never arrived as no-shows.
func (s *Store) CallNext(ctx context.Context, u *auth.User) (*Visit, error) {
	if u.Role != domain.Doctor {
		return nil, ErrForbidden
	}
	var picked int64
	var why string
	err := s.tx(ctx, func(tx pgx.Tx) error {
		day, now := s.today(), s.Now()
		if err := lockDoctorDay(ctx, tx, u.ID, day); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `update visits set status = 'seen', done_at = $3 where doctor_id = $1 and day = $2 and status = 'called'`, u.ID, day, now); err != nil {
			return err
		}
		rows, err := tx.Query(ctx, `select id, kind, token, slot, arrived_at from visits where doctor_id = $1 and day = $2 and status = 'waiting'`, u.ID, day)
		if err != nil {
			return err
		}
		var items []domain.QueueItem
		for rows.Next() {
			var q domain.QueueItem
			var arrived *time.Time
			var slotStr *string
			if err := rows.Scan(&q.ID, &q.Kind, &q.Token, &slotStr, &arrived); err != nil {
				rows.Close()
				return err
			}
			if slotStr != nil {
				t, _ := time.ParseInLocation("15:04:05", *slotStr, now.Location())
				st := day.Add(time.Duration(t.Hour())*time.Hour + time.Duration(t.Minute())*time.Minute)
				q.Slot = &st
			}
			q.Arrived = arrived
			items = append(items, q)
		}
		rows.Close()
		// how many booked patients in a row were called just before
		run := 0
		kinds, err := tx.Query(ctx, `select kind from visits where doctor_id = $1 and day = $2 and called_at is not null order by called_at desc limit 5`, u.ID, day)
		if err != nil {
			return err
		}
		for kinds.Next() {
			var k string
			kinds.Scan(&k)
			if k != "booked" {
				break
			}
			run++
		}
		kinds.Close()
		p, w, noShows := domain.Next(items, now, run)
		if len(noShows) > 0 {
			if _, err := tx.Exec(ctx, `update visits set status = 'no_show' where id = any($1)`, noShows); err != nil {
				return err
			}
			if _, err := tx.Exec(ctx, `insert into outbox (patient_id, phone, body) select p.id, p.phone, 'Arogya Hospital: we missed you at your appointment today. Reply or call 0421-000000 to book again.'
				from visits v join patients p on p.id = v.patient_id where v.id = any($1) and p.sms_reminders`, noShows); err != nil {
				return err
			}
		}
		if p != nil {
			picked, why = p.ID, w
			if _, err := tx.Exec(ctx, `update visits set status = 'called', called_at = $2 where id = $1`, p.ID, now); err != nil {
				return err
			}
		}
		return notify(ctx, tx, fmt.Sprintf("opd:%d", u.ID))
	})
	if err != nil || picked == 0 {
		return nil, err
	}
	v, err := scanVisit(s.DB.QueryRow(ctx, `select `+visitCols+visitFrom+` where v.id = $1`, picked))
	if v != nil {
		v.Why = why
	}
	return v, err
}

// BoardRow is one doctor on the waiting-room screen. It shows token
// numbers only, never names, because the screen faces a public hall.
type BoardRow struct {
	DoctorID   int64  `json:"doctor_id"`
	Doctor     string `json:"doctor"`
	Department string `json:"department"`
	Now        int    `json:"now_serving"` // 0 = nobody yet
	Next       []int  `json:"next"`
	Waiting    int    `json:"waiting"`
}

func (s *Store) Board(ctx context.Context) ([]BoardRow, error) {
	day := s.today()
	rows, err := s.DB.Query(ctx, `select u.id, u.name, d.name,
		coalesce((select token from visits where doctor_id = u.id and day = $1 and status = 'called'), 0),
		coalesce(array(select token from visits where doctor_id = u.id and day = $1 and status = 'waiting' and (kind = 'walkin' or arrived_at is not null) order by coalesce(slot, '23:59'), token limit 3), '{}'),
		(select count(*) from visits where doctor_id = u.id and day = $1 and status = 'waiting')::int
		from users u join departments d on d.id = u.department_id where u.role = 'doctor' and u.active order by d.name`, day)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []BoardRow{}
	for rows.Next() {
		var b BoardRow
		var next []int32
		if err := rows.Scan(&b.DoctorID, &b.Doctor, &b.Department, &b.Now, &next, &b.Waiting); err != nil {
			return nil, err
		}
		b.Next = []int{}
		for _, t := range next {
			b.Next = append(b.Next, int(t))
		}
		out = append(out, b)
	}
	return out, rows.Err()
}

// TodayAtReception lists today's visits for check-in.
func (s *Store) TodayAtReception(ctx context.Context, u *auth.User) ([]Visit, error) {
	if !is(u, domain.Receptionist, domain.Admin) {
		return nil, ErrForbidden
	}
	return s.visits(ctx, `v.day = $1 and v.status in ('waiting', 'called') order by coalesce(v.slot, '23:59'), v.token`, s.today())
}

// MyVisits lists a patient's own upcoming and recent visits.
func (s *Store) MyVisits(ctx context.Context, u *auth.User) ([]Visit, error) {
	if u.Role != domain.Patient {
		return nil, ErrForbidden
	}
	return s.visits(ctx, `v.patient_id = $1 and v.day >= $2 order by v.day, v.slot nulls last`, *u.PatientID, s.today().AddDate(0, 0, -30))
}
