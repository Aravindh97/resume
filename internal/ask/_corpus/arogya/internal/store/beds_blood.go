package store

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/Aravindh97/arogya/internal/auth"
	"github.com/Aravindh97/arogya/internal/domain"
)

// ---- Beds ------------------------------------------------------------------

// querier is a pool or a transaction.
type querier interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}

func admissions(ctx context.Context, q querier, where string, args ...any) ([]Admission, error) {
	rows, err := q.Query(ctx, `select a.id, a.patient_id, p.name, p.mrn, u.name, w.name, a.ward_id, coalesce(b.label, ''), a.bed_id, a.reason, a.urgency, a.requested_at, a.admitted_at, a.discharged_at
		from admissions a join patients p on p.id = a.patient_id join users u on u.id = a.doctor_id join wards w on w.id = a.ward_id left join beds b on b.id = a.bed_id where `+where, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Admission{}
	for rows.Next() {
		var a Admission
		if err := rows.Scan(&a.ID, &a.PatientID, &a.Patient, &a.MRN, &a.Doctor, &a.Ward, &a.WardID, &a.Bed, &a.BedID, &a.Reason, &a.Urgency, &a.RequestedAt, &a.AdmittedAt, &a.DischargedAt); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

type Bed struct {
	ID       int64  `json:"id"`
	Label    string `json:"label"`
	Occupied bool   `json:"occupied"`
	Patient  string `json:"patient,omitempty"` // only for staff of this ward (and admins)
}

type Ward struct {
	ID       int64       `json:"id"`
	Name     string      `json:"name"`
	Kind     string      `json:"kind"`
	Beds     []Bed       `json:"beds"`
	Free     int         `json:"free"`
	Requests []Admission `json:"requests"` // waiting for a bed in this ward
}

// Wards is the bed board. Everyone on staff sees counts; patient names on
// beds and requests are shown only to the ward's nurses, admitting doctors
// and admins.
func (s *Store) Wards(ctx context.Context, u *auth.User) ([]Ward, error) {
	if u.Role == domain.Patient {
		return nil, ErrForbidden
	}
	var out []Ward
	err := s.tx(ctx, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `select id, name, kind from wards order by id`)
		if err != nil {
			return err
		}
		for rows.Next() {
			var w Ward
			rows.Scan(&w.ID, &w.Name, &w.Kind)
			out = append(out, w)
		}
		rows.Close()
		for i := range out {
			w := &out[i]
			mine := u.Role == domain.Admin || u.Role == domain.Doctor || u.WardID != nil && *u.WardID == w.ID
			rows, err := tx.Query(ctx, `select b.id, b.label, a.id is not null, coalesce(p.name, '') from beds b
				left join admissions a on a.bed_id = b.id and a.discharged_at is null left join patients p on p.id = a.patient_id
				where b.ward_id = $1 order by b.id`, w.ID)
			if err != nil {
				return err
			}
			for rows.Next() {
				var b Bed
				rows.Scan(&b.ID, &b.Label, &b.Occupied, &b.Patient)
				if !mine {
					b.Patient = ""
				}
				if !b.Occupied {
					w.Free++
				}
				w.Beds = append(w.Beds, b)
			}
			rows.Close()
			w.Requests, err = admissions(ctx, tx, `a.ward_id = $1 and a.bed_id is null and a.discharged_at is null
				order by case a.urgency when 'emergency' then 0 when 'urgent' then 1 else 2 end, a.requested_at`, w.ID)
			if err != nil {
				return err
			}
			if !mine {
				for k := range w.Requests {
					w.Requests[k].Patient, w.Requests[k].MRN = "", ""
				}
			}
		}
		return nil
	})
	return out, err
}

type AdmitRequest struct {
	PatientID int64  `json:"patient_id"`
	WardID    int64  `json:"ward_id"`
	Reason    string `json:"reason"`
	Urgency   string `json:"urgency"`
}

// RequestAdmission: a doctor caring for the patient asks for a bed.
func (s *Store) RequestAdmission(ctx context.Context, u *auth.User, in AdmitRequest) (*Admission, error) {
	if u.Role != domain.Doctor {
		return nil, ErrForbidden
	}
	if in.Urgency != "routine" && in.Urgency != "urgent" && in.Urgency != "emergency" {
		return nil, problem("urgency", "choose routine, urgent or emergency")
	}
	if strings.TrimSpace(in.Reason) == "" || len(in.Reason) > 300 {
		return nil, problem("reason", "a reason is required")
	}
	var id int64
	err := s.tx(ctx, func(tx pgx.Tx) error {
		reason, err := s.access(ctx, tx, u, in.PatientID, "update", "", "admission request")
		if err != nil {
			return err
		}
		if reason != "care_team" {
			return ErrForbidden
		}
		err = tx.QueryRow(ctx, `insert into admissions (patient_id, doctor_id, ward_id, reason, urgency, requested_at) values ($1, $2, $3, $4, $5, $6) returning id`,
			in.PatientID, u.ID, in.WardID, strings.TrimSpace(in.Reason), in.Urgency, s.Now()).Scan(&id)
		if uniqueViolation(err, "admissions_one_per_patient") {
			return conflict("this patient already has an open admission")
		}
		if err != nil {
			return err
		}
		return notify(ctx, tx, "beds")
	})
	if err != nil {
		return nil, err
	}
	a, err := admissions(ctx, s.DB, `a.id = $1`, id)
	if err != nil || len(a) == 0 {
		return nil, err
	}
	return &a[0], nil
}

// AssignBed puts a waiting admission into a free bed of its ward. Two
// nurses giving the last bed to two patients at the same moment: the
// unique index on live admissions per bed lets exactly one succeed.
func (s *Store) AssignBed(ctx context.Context, u *auth.User, admissionID, bedID int64) error {
	if u.Role != domain.Nurse && u.Role != domain.Admin {
		return ErrForbidden
	}
	return s.tx(ctx, func(tx pgx.Tx) error {
		var ward, bedWard int64
		var bed *int64
		if err := tx.QueryRow(ctx, `select ward_id, bed_id from admissions where id = $1 and discharged_at is null`, admissionID).Scan(&ward, &bed); errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		} else if err != nil {
			return err
		}
		if u.Role == domain.Nurse && *u.WardID != ward {
			return ErrForbidden
		}
		if bed != nil {
			return conflict("this patient already has a bed")
		}
		if err := tx.QueryRow(ctx, `select ward_id from beds where id = $1`, bedID).Scan(&bedWard); err != nil || bedWard != ward {
			return problem("bed_id", "choose a bed in this ward")
		}
		_, err := tx.Exec(ctx, `update admissions set bed_id = $2, admitted_at = $3 where id = $1`, admissionID, bedID, s.Now())
		if uniqueViolation(err, "admissions_one_per_bed") {
			return conflict("that bed was just taken; choose another")
		}
		if err != nil {
			return err
		}
		return notify(ctx, tx, "beds")
	})
}

func (s *Store) Discharge(ctx context.Context, u *auth.User, admissionID int64) error {
	if !is(u, domain.Nurse, domain.Doctor, domain.Admin) {
		return ErrForbidden
	}
	return s.tx(ctx, func(tx pgx.Tx) error {
		var ward int64
		var doctor int64
		if err := tx.QueryRow(ctx, `select ward_id, doctor_id from admissions where id = $1 and discharged_at is null`, admissionID).Scan(&ward, &doctor); errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		} else if err != nil {
			return err
		}
		if u.Role == domain.Nurse && *u.WardID != ward || u.Role == domain.Doctor && u.ID != doctor {
			return ErrForbidden
		}
		if _, err := tx.Exec(ctx, `update admissions set discharged_at = $2 where id = $1`, admissionID, s.Now()); err != nil {
			return err
		}
		return notify(ctx, tx, "beds")
	})
}

// ---- Blood bank --------------------------------------------------------------

type Stock struct {
	Group     string `json:"group"`
	Component string `json:"component"`
	Units     int    `json:"units"`
	Expiring  int    `json:"expiring"` // within 3 days
}

type BloodRequest struct {
	ID        int64     `json:"id"`
	PatientID int64     `json:"patient_id"`
	Patient   string    `json:"patient"`
	MRN       string    `json:"mrn"`
	Doctor    string    `json:"doctor"`
	Group     string    `json:"blood_group"`
	Component string    `json:"component"`
	Units     int       `json:"units"`
	Urgency   string    `json:"urgency"`
	Status    string    `json:"status"`
	Reserved  []string  `json:"reserved"` // unit numbers
	CreatedAt time.Time `json:"created_at"`
}

type BloodBank struct {
	Stock    []Stock        `json:"stock"`
	Requests []BloodRequest `json:"requests"`
	Expired  int            `json:"expired_available"` // past expiry, still marked available: to discard
}

func (s *Store) BloodBank(ctx context.Context, u *auth.User) (*BloodBank, error) {
	if !is(u, domain.BloodBank, domain.Doctor, domain.Admin) {
		return nil, ErrForbidden
	}
	bb := &BloodBank{Stock: []Stock{}, Requests: []BloodRequest{}}
	today := s.today()
	rows, err := s.DB.Query(ctx, `select blood_group, component, count(*)::int, count(*) filter (where expires_on <= $1::date + 3)::int from blood_units
		where status = 'available' and expires_on > $1 group by 1, 2 order by 2, 1`, today)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var st Stock
		rows.Scan(&st.Group, &st.Component, &st.Units, &st.Expiring)
		bb.Stock = append(bb.Stock, st)
	}
	rows.Close()
	s.DB.QueryRow(ctx, `select count(*) from blood_units where status = 'available' and expires_on <= $1`, today).Scan(&bb.Expired)
	where := `true`
	args := []any{}
	if u.Role == domain.Doctor {
		where, args = `r.doctor_id = $1`, []any{u.ID}
	}
	rows, err = s.DB.Query(ctx, `select r.id, r.patient_id, p.name, p.mrn, u.name, r.blood_group, r.component, r.units, r.urgency, r.status, r.created_at,
		coalesce(array(select unit_no from blood_units where request_id = r.id and status = 'reserved' order by expires_on), '{}')
		from blood_requests r join patients p on p.id = r.patient_id join users u on u.id = r.doctor_id where `+where+`
		order by r.status in ('issued', 'cancelled'), case r.urgency when 'emergency' then 0 when 'urgent' then 1 else 2 end, r.created_at desc limit 50`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var r BloodRequest
		if err := rows.Scan(&r.ID, &r.PatientID, &r.Patient, &r.MRN, &r.Doctor, &r.Group, &r.Component, &r.Units, &r.Urgency, &r.Status, &r.CreatedAt, &r.Reserved); err != nil {
			return nil, err
		}
		bb.Requests = append(bb.Requests, r)
	}
	return bb, rows.Err()
}

type NewBloodRequest struct {
	PatientID int64  `json:"patient_id"`
	Component string `json:"component"`
	Units     int    `json:"units"`
	Urgency   string `json:"urgency"`
}

// RequestBlood: the treating doctor asks for units; the patient's recorded
// blood group is used, never a group typed into the request.
func (s *Store) RequestBlood(ctx context.Context, u *auth.User, in NewBloodRequest) (*BloodRequest, error) {
	if u.Role != domain.Doctor {
		return nil, ErrForbidden
	}
	if in.Component != "rbc" && in.Component != "plasma" && in.Component != "platelets" {
		return nil, problem("component", "choose red cells, plasma or platelets")
	}
	if in.Units < 1 || in.Units > 20 {
		return nil, problem("units", "1 to 20 units")
	}
	if in.Urgency != "routine" && in.Urgency != "urgent" && in.Urgency != "emergency" {
		return nil, problem("urgency", "choose an urgency")
	}
	var id int64
	err := s.tx(ctx, func(tx pgx.Tx) error {
		reason, err := s.access(ctx, tx, u, in.PatientID, "update", "", "blood request")
		if err != nil {
			return err
		}
		if reason != "care_team" {
			return ErrForbidden
		}
		var group *string
		tx.QueryRow(ctx, `select blood_group from patients where id = $1`, in.PatientID).Scan(&group)
		if group == nil {
			return problem("", "this patient's blood group is not recorded; send a grouping sample first")
		}
		if err := tx.QueryRow(ctx, `insert into blood_requests (patient_id, doctor_id, blood_group, component, units, urgency, created_at) values ($1, $2, $3, $4, $5, $6, $7) returning id`,
			in.PatientID, u.ID, *group, in.Component, in.Units, in.Urgency, s.Now()).Scan(&id); err != nil {
			return err
		}
		return notify(ctx, tx, "blood")
	})
	if err != nil {
		return nil, err
	}
	return s.request(ctx, id)
}

func (s *Store) request(ctx context.Context, id int64) (*BloodRequest, error) {
	var r BloodRequest
	err := s.DB.QueryRow(ctx, `select r.id, r.patient_id, p.name, p.mrn, u.name, r.blood_group, r.component, r.units, r.urgency, r.status, r.created_at,
		coalesce(array(select unit_no from blood_units where request_id = r.id and status in ('reserved', 'issued') order by expires_on), '{}')
		from blood_requests r join patients p on p.id = r.patient_id join users u on u.id = r.doctor_id where r.id = $1`, id).
		Scan(&r.ID, &r.PatientID, &r.Patient, &r.MRN, &r.Doctor, &r.Group, &r.Component, &r.Units, &r.Urgency, &r.Status, &r.CreatedAt, &r.Reserved)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	return &r, err
}

// Reserve chooses units for a request with domain.Pick and reserves them.
// Candidate rows are locked with SKIP LOCKED, so two reservations running
// at once each take different units instead of waiting or taking the same
// bag; each re-checks status under the lock.
func (s *Store) Reserve(ctx context.Context, u *auth.User, requestID int64) (*BloodRequest, error) {
	if !is(u, domain.BloodBank, domain.Admin) {
		return nil, ErrForbidden
	}
	err := s.tx(ctx, func(tx pgx.Tx) error {
		var r BloodRequest
		var birth time.Time
		var sex string
		if err := tx.QueryRow(ctx, `select r.blood_group, r.component, r.units, r.urgency, r.status, p.birth_date, p.sex from blood_requests r join patients p on p.id = r.patient_id where r.id = $1 for update of r`,
			requestID).Scan(&r.Group, &r.Component, &r.Units, &r.Urgency, &r.Status, &birth, &sex); errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		} else if err != nil {
			return err
		}
		if r.Status != "open" && r.Status != "partial" {
			return conflict("this request is %s", r.Status)
		}
		var have int
		tx.QueryRow(ctx, `select count(*) from blood_units where request_id = $1 and status = 'reserved'`, requestID).Scan(&have)
		need := r.Units - have
		today := s.today()
		rows, err := tx.Query(ctx, `select id, blood_group, component, expires_on from blood_units where status = 'available' and component = $1 and expires_on > $2
			order by expires_on, id for update skip locked`, r.Component, today)
		if err != nil {
			return err
		}
		var stock []domain.Unit
		for rows.Next() {
			var x domain.Unit
			rows.Scan(&x.ID, &x.Group, &x.Component, &x.ExpiresOn)
			stock = append(stock, x)
		}
		rows.Close()
		picked := domain.Pick(domain.Recipient{Group: r.Group, Sex: sex, Age: domain.Age(birth, today)}, r.Component, r.Urgency, stock, need, today)
		ids := make([]int64, len(picked))
		for i, p := range picked {
			ids[i] = p.ID
		}
		if _, err := tx.Exec(ctx, `update blood_units set status = 'reserved', request_id = $2 where id = any($1) and status = 'available'`, ids, requestID); err != nil {
			return err
		}
		status := "reserved"
		if have+len(picked) < r.Units {
			status = "partial"
		}
		if _, err := tx.Exec(ctx, `update blood_requests set status = $2 where id = $1`, requestID, status); err != nil {
			return err
		}
		return notify(ctx, tx, "blood")
	})
	if err != nil {
		return nil, err
	}
	return s.request(ctx, requestID)
}

// Issue hands reserved units over to the ward.
func (s *Store) Issue(ctx context.Context, u *auth.User, requestID int64) (*BloodRequest, error) {
	if !is(u, domain.BloodBank, domain.Admin) {
		return nil, ErrForbidden
	}
	err := s.tx(ctx, func(tx pgx.Tx) error {
		var status string
		var units int
		if err := tx.QueryRow(ctx, `select status, units from blood_requests where id = $1 for update`, requestID).Scan(&status, &units); errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		} else if err != nil {
			return err
		}
		if status != "reserved" {
			return conflict("only a fully reserved request can be issued (this one is %s)", status)
		}
		if _, err := tx.Exec(ctx, `update blood_units set status = 'issued' where request_id = $1 and status = 'reserved'`, requestID); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `update blood_requests set status = 'issued' where id = $1`, requestID); err != nil {
			return err
		}
		return notify(ctx, tx, "blood")
	})
	if err != nil {
		return nil, err
	}
	return s.request(ctx, requestID)
}

// DiscardExpired marks every expired unit still on the shelf as discarded.
func (s *Store) DiscardExpired(ctx context.Context, u *auth.User) (int64, error) {
	if !is(u, domain.BloodBank, domain.Admin) {
		return 0, ErrForbidden
	}
	tag, err := s.DB.Exec(ctx, `update blood_units set status = 'discarded' where status = 'available' and expires_on <= $1`, s.today())
	return tag.RowsAffected(), err
}

type DonorMatch struct {
	ID          int64  `json:"id"`
	Name        string `json:"name"`
	Group       string `json:"blood_group"`
	Area        string `json:"area"`
	SameArea    bool   `json:"same_area"`
	LastDonated string `json:"last_donated,omitempty"`
}

// Donors lists donors who could give for a request now: compatible group,
// past the minimum interval since their last donation. Donors in the
// patient's area come first, then exact group matches. Phone numbers are
// not shown; contacting goes through the (simulated) SMS outbox.
func (s *Store) Donors(ctx context.Context, u *auth.User, requestID int64) ([]DonorMatch, error) {
	if !is(u, domain.BloodBank, domain.Admin) {
		return nil, ErrForbidden
	}
	var group, component, area string
	if err := s.DB.QueryRow(ctx, `select r.blood_group, r.component, p.area from blood_requests r join patients p on p.id = r.patient_id where r.id = $1`, requestID).Scan(&group, &component, &area); errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	} else if err != nil {
		return nil, err
	}
	rows, err := s.DB.Query(ctx, `select id, name, blood_group, sex, area, last_donated from donors order by name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var near, far []DonorMatch
	today := s.today()
	for rows.Next() {
		var d DonorMatch
		var sex string
		var last *time.Time
		rows.Scan(&d.ID, &d.Name, &d.Group, &sex, &d.Area, &last)
		if !domain.Compatible(component, group, d.Group) {
			continue
		}
		if ok, _ := domain.DonorCanGive(sex, last, today); !ok {
			continue
		}
		if last != nil {
			d.LastDonated = last.Format(time.DateOnly)
		}
		d.SameArea = strings.EqualFold(d.Area, area)
		if d.SameArea {
			near = append(near, d)
		} else {
			far = append(far, d)
		}
	}
	out := append(near, far...)
	exact := func(xs []DonorMatch) {
		for i := 1; i < len(xs); i++ {
			for j := i; j > 0 && xs[j].Group == group && xs[j-1].Group != group; j-- {
				xs[j], xs[j-1] = xs[j-1], xs[j]
			}
		}
	}
	exact(out[:len(near)])
	exact(out[len(near):])
	if len(out) > 20 {
		out = out[:20]
	}
	if out == nil {
		out = []DonorMatch{}
	}
	return out, rows.Err()
}

// AskDonors queues an SMS to chosen donors (simulated: written to the outbox).
func (s *Store) AskDonors(ctx context.Context, u *auth.User, requestID int64, donorIDs []int64) (int, error) {
	if !is(u, domain.BloodBank, domain.Admin) {
		return 0, ErrForbidden
	}
	if len(donorIDs) == 0 || len(donorIDs) > 20 {
		return 0, problem("donors", "choose 1 to 20 donors")
	}
	var group string
	if err := s.DB.QueryRow(ctx, `select blood_group from blood_requests where id = $1`, requestID).Scan(&group); err != nil {
		return 0, ErrNotFound
	}
	tag, err := s.DB.Exec(ctx, `insert into outbox (donor_id, phone, body) select id, phone, $2 from donors where id = any($1)`, donorIDs,
		fmt.Sprintf("Arogya Hospital blood bank: a patient needs %s blood. If you can donate today, please reply YES or call 0421-000000. Thank you.", group))
	return int(tag.RowsAffected()), err
}

type Message struct {
	ID    int64     `json:"id"`
	Phone string    `json:"phone"`
	Body  string    `json:"body"`
	At    time.Time `json:"at"`
}

// Outbox shows the simulated SMS messages (admin).
func (s *Store) Outbox(ctx context.Context, u *auth.User) ([]Message, error) {
	if u.Role != domain.Admin {
		return nil, ErrForbidden
	}
	rows, err := s.DB.Query(ctx, `select id, phone, body, at from outbox order by id desc limit 50`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Message{}
	for rows.Next() {
		var m Message
		rows.Scan(&m.ID, &m.Phone, &m.Body, &m.At)
		out = append(out, m)
	}
	return out, rows.Err()
}
