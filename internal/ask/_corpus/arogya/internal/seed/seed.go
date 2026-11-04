// Package seed fills the database with a synthetic hospital: Arogya
// Hospital in Tiruppur, its staff, 60 patients with histories and lab
// trends, a morning OPD in progress, wards, blood stock and donors.
// Every person in it is made up.
package seed

import (
	"context"
	"fmt"
	"math/rand/v2"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Aravindh97/arogya/internal/auth"
)

type DemoUser struct{ Email, Name, Role, Note string }

var Users = []DemoUser{
	{"anand@arogya.demo", "Dr. Anand Iyer", "admin", "Medical superintendent: reviews emergency access, sees the access log"},
	{"malar@arogya.demo", "Malar Selvam", "receptionist", "Books appointments, issues walk-in tokens, checks patients in"},
	{"priya@arogya.demo", "Dr. Priya Raman", "doctor", "General Medicine: calls patients, writes notes, admits, orders blood"},
	{"senthil@arogya.demo", "Dr. Senthil Kumar", "doctor", "Cardiology: try opening a patient who isn't his"},
	{"kavitha@arogya.demo", "Kavitha Murugan", "nurse", "Ward A nurse: assigns beds, sees her ward's patients"},
	{"suresh@arogya.demo", "Suresh Babu", "bloodbank", "Blood bank: reserves compatible units, matches donors"},
	{"meena@arogya.demo", "Meena Subramaniam", "patient", "A patient: books online, sees her record and who opened it"},
}

// first alternates female and male names; personName relies on that.
var first = []string{"Lakshmi", "Murugan", "Selvi", "Karthik", "Revathi", "Arjun", "Divya", "Ganesh", "Kavya", "Saravanan", "Anitha", "Vignesh",
	"Priya", "Manikandan", "Deepa", "Rajesh", "Pooja", "Balaji", "Nithya", "Prakash", "Sangeetha", "Dinesh", "Malathi", "Ramesh", "Fathima", "Joseph"}
var last = []string{"Kumar", "Raman", "Subramaniam", "Natarajan", "Krishnan", "Velu", "Sundaram", "Pandian", "Arumugam", "Rajendran", "Basha", "Thomas"}
var areas = []string{"Avinashi Road", "Kumaran Road", "Palladam", "Mangalam", "Veerapandi", "Nallur", "Kangeyam Road", "Perumanallur"}

// Clock is the demo's hospital time: it starts at 10:15 IST on the day the
// data was seeded and runs at normal speed, so the OPD is always mid-morning
// when someone opens the demo.
type Clock struct {
	Start time.Time // real time of the last seed
	Day   time.Time // the hospital day, midnight IST
}

func (c Clock) Now() time.Time {
	return c.Day.Add(10*time.Hour + 15*time.Minute).Add(time.Since(c.Start))
}

func Reset(ctx context.Context, db *pgxpool.Pool, now time.Time, cost int) error {
	tx, err := db.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	// truncate does not fire the append-only row triggers
	if _, err := tx.Exec(ctx, `truncate outbox, donors, blood_units, blood_requests, admissions, beds, glass_reviews, access_log, prescriptions,
		observations, encounters, visits, sessions, users, patients, wards, departments restart identity cascade`); err != nil {
		return err
	}
	if err := seed(ctx, tx, now, cost); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func seed(ctx context.Context, tx pgx.Tx, now time.Time, cost int) error {
	var err error // the first error from the seed's many statements; later ones are skipped
	r := rand.New(rand.NewPCG(11, 11))
	loc := now.Location()
	y, m, d := now.Date()
	today := time.Date(y, m, d, 0, 0, 0, 0, loc)
	ex := func(sql string, a ...any) {
		if err == nil {
			if _, e := tx.Exec(ctx, sql, a...); e != nil {
				err = fmt.Errorf("%.60s…: %w", sql, e)
			}
		}
	}
	id := func(sql string, a ...any) int64 {
		var v int64
		if err == nil {
			if e := tx.QueryRow(ctx, sql, a...).Scan(&v); e != nil {
				err = fmt.Errorf("%.60s…: %w", sql, e)
			}
		}
		return v
	}
	depts := map[string]int64{}
	for _, n := range []string{"General Medicine", "Cardiology", "Paediatrics", "Orthopaedics"} {
		depts[n] = id(`insert into departments (name) values ($1) returning id`, n)
	}
	wards := map[string]int64{}
	for _, w := range [][3]any{{"Ward A (General)", "general", 12}, {"ICU", "icu", 6}, {"Children's ward", "paediatric", 8}} {
		wid := id(`insert into wards (name, kind) values ($1, $2) returning id`, w[0], w[1])
		wards[w[0].(string)] = wid
		for i := 1; i <= w[2].(int); i++ {
			ex(`insert into beds (ward_id, label) values ($1, $2)`, wid, fmt.Sprintf("%s-%02d", string(w[0].(string)[0]), i))
		}
	}
	// patients: index 0 is Meena, the demo patient
	type pat struct {
		id    int64
		name  string
		sex   string
		age   int
		group string
	}
	groups := []string{"O+", "O+", "O+", "B+", "B+", "A+", "A+", "AB+", "O-", "A-", "B-", "AB-"} // roughly India's mix
	var pats []pat
	// Names must be unique, and never a staff member's, so screens are unambiguous.
	used := map[string]bool{"Ayesha Khan": true, "Ravi Shankar": true} // the two doctors added below
	for _, u := range Users {
		used[strings.TrimPrefix(u.Name, "Dr. ")] = true
	}
	for i := 0; i < 60; i++ {
		sx := r.IntN(2)
		nm := personName(r, sx) + " " + last[r.IntN(len(last))]
		for used[nm] { // 156 combinations for about 60 patients: retry until unused
			nm = personName(r, sx) + " " + last[r.IntN(len(last))]
		}
		used[nm] = true
		p := pat{name: nm, sex: []string{"female", "male"}[sx], age: 4 + r.IntN(80), group: groups[r.IntN(len(groups))]}
		if i == 0 {
			p = pat{name: "Meena Subramaniam", sex: "female", age: 34, group: "O-"}
		}
		birth := today.AddDate(-p.age, -r.IntN(12), -r.IntN(28))
		p.id = id(`insert into patients (mrn, name, birth_date, sex, phone, area, blood_group, research_use) values ($1, $2, $3, $4, $5, $6, $7, $8) returning id`,
			fmt.Sprintf("AH-%d-%05d", today.Year()-r.IntN(4), 10001+i), p.name, birth, p.sex, fmt.Sprintf("9%09d", r.IntN(1e9)), areas[r.IntN(len(areas))], p.group, i%5 == 0)
		pats = append(pats, p)
	}
	users := map[string]int64{}
	for _, u := range Users {
		h, herr := auth.HashPassword(auth.RandomPassword(), cost)
		if herr != nil {
			return herr
		}
		var dept, ward, patient any
		switch u.Email {
		case "priya@arogya.demo":
			dept = depts["General Medicine"]
		case "senthil@arogya.demo":
			dept = depts["Cardiology"]
		case "kavitha@arogya.demo":
			ward = wards["Ward A (General)"]
		case "meena@arogya.demo":
			patient = pats[0].id
		}
		users[u.Email] = id(`insert into users (email, name, role, department_id, ward_id, patient_id, password_hash, demo) values ($1, $2, $3, $4, $5, $6, $7, true) returning id`,
			u.Email, u.Name, u.Role, dept, ward, patient, h)
	}
	more := [][4]any{{"ayesha@arogya.demo", "Dr. Ayesha Khan", "doctor", depts["Paediatrics"]}, {"ravi@arogya.demo", "Dr. Ravi Shankar", "doctor", depts["Orthopaedics"]}}
	for _, u := range more {
		h, _ := auth.HashPassword(auth.RandomPassword(), cost)
		users[u[0].(string)] = id(`insert into users (email, name, role, department_id, password_hash) values ($1, $2, $3, $4, $5) returning id`, u[0], u[1], u[2], u[3], h)
	}
	h, _ := auth.HashPassword(auth.RandomPassword(), cost)
	icuNurse := id(`insert into users (email, name, role, ward_id, password_hash) values ('joseph@arogya.demo', 'Joseph Thomas', 'nurse', $1, $2) returning id`, wards["ICU"], h)
	_ = icuNurse
	doctors := []int64{users["priya@arogya.demo"], users["senthil@arogya.demo"], users["ayesha@arogya.demo"], users["ravi@arogya.demo"]}
	priya, senthil := doctors[0], doctors[1]

	// histories: chronic patients with lab trends, months of visits
	diag := []string{"Type 2 diabetes mellitus", "Essential hypertension", "Iron deficiency anaemia", "Viral fever", "Acute gastroenteritis", "Lumbar strain", "Asthma"}
	for i, p := range pats {
		doc := doctors[r.IntN(len(doctors))]
		if i == 0 {
			doc = priya
		}
		cond := diag[r.IntN(len(diag))]
		if i == 0 {
			cond = "Iron deficiency anaemia"
		}
		n := 1 + r.IntN(4)
		for k := n; k >= 1; k-- {
			at := today.AddDate(0, -k*2, -r.IntN(10)).Add(time.Duration(9+r.IntN(4)) * time.Hour)
			eid := id(`insert into encounters (patient_id, doctor_id, at, complaint, diagnosis, notes) values ($1, $2, $3, $4, $5, $6) returning id`,
				p.id, doc, at, map[bool]string{true: "Review", false: "Tiredness, giddiness"}[k < n], cond, "Advised diet and follow-up.")
			switch cond {
			case "Type 2 diabetes mellitus":
				ex(`insert into observations (patient_id, code, name, value, unit, at) values ($1, '4548-4', 'HbA1c', $2, '%', $3), ($1, '2339-0', 'Glucose', $4, 'mg/dL', $3)`,
					p.id, 9.2-0.5*float64(n-k)+r.Float64()*0.3, at, 190-18*float64(n-k)+float64(r.IntN(20)))
				ex(`insert into prescriptions (encounter_id, drug, dose, days) values ($1, 'Metformin 500 mg', '1-0-1 after food', 60)`, eid)
			case "Essential hypertension":
				ex(`insert into observations (patient_id, code, name, value, unit, at) values ($1, '8480-6', 'Systolic blood pressure', $2, 'mm[Hg]', $3), ($1, '8462-4', 'Diastolic blood pressure', $4, 'mm[Hg]', $3)`,
					p.id, 158-6*float64(n-k)+float64(r.IntN(6)), at, 98-3*float64(n-k)+float64(r.IntN(4)))
				ex(`insert into prescriptions (encounter_id, drug, dose, days) values ($1, 'Amlodipine 5 mg', '1-0-0', 60)`, eid)
			case "Iron deficiency anaemia":
				ex(`insert into observations (patient_id, code, name, value, unit, at) values ($1, '718-7', 'Haemoglobin', $2, 'g/dL', $3)`, p.id, 8.6+0.7*float64(n-k)+r.Float64()*0.2, at)
				ex(`insert into prescriptions (encounter_id, drug, dose, days) values ($1, 'Ferrous ascorbate', '0-1-0', 60)`, eid)
			}
			ex(`insert into access_log (patient_id, user_id, at, action, reason) values ($1, $2, $3, 'view', 'care_team'), ($1, $2, $3, 'update', 'care_team')`, p.id, doc, at)
		}
	}
	// Meena checks her own record now and then
	ex(`insert into access_log (patient_id, user_id, at, action, reason) values ($1, $2, $3, 'view', 'own_record')`, pats[0].id, users["meena@arogya.demo"], today.AddDate(0, 0, -9).Add(20*time.Hour))
	// one unreviewed emergency access, for the admin to review
	ex(`insert into access_log (patient_id, user_id, at, action, reason, detail) values ($1, $2, $3, 'view', 'break_glass', 'Brought to casualty with chest pain, unable to give history')`,
		pats[7].id, senthil, today.AddDate(0, 0, -1).Add(23*time.Hour+10*time.Minute))

	// Today's OPD, around a 10:15 clinic clock: slots from 9:00, some seen,
	// one with the doctor, booked patients waiting or not yet arrived,
	// walk-ins with tokens.
	clock := today.Add(10*time.Hour + 15*time.Minute)
	pi := 1
	for di, doc := range doctors {
		token := 0
		for slot := today.Add(9 * time.Hour); slot.Before(today.Add(13 * time.Hour)); slot = slot.Add(15 * time.Minute) {
			if r.IntN(10) < 3 && !(di == 0 && slot.Equal(today.Add(11*time.Hour))) {
				continue
			}
			token++
			p := pats[pi%len(pats)]
			pi++
			status, arrived, called, doneAt := "waiting", any(nil), any(nil), any(nil)
			switch {
			case slot.Before(clock.Add(-30 * time.Minute)):
				status, arrived, called, doneAt = "seen", slot.Add(-10*time.Minute), slot.Add(5*time.Minute), slot.Add(14*time.Minute)
				if r.IntN(6) == 0 {
					status, arrived, called, doneAt = "no_show", nil, nil, nil
				}
			case slot.Before(clock.Add(-10 * time.Minute)):
				if token%3 == 0 {
					status, arrived, called = "called", slot.Add(-12*time.Minute), clock.Add(-4*time.Minute)
				} else {
					arrived = slot.Add(-8 * time.Minute)
				}
			case slot.Before(clock.Add(40 * time.Minute)):
				if r.IntN(3) > 0 {
					arrived = clock.Add(-time.Duration(r.IntN(20)) * time.Minute)
				}
			}
			ex(`insert into visits (patient_id, doctor_id, day, slot, kind, token, status, arrived_at, called_at, done_at) values ($1, $2, $3, $4, 'booked', $5, $6, $7, $8, $9)
				on conflict do nothing`, p.id, doc, today, slot.Format("15:04"), token, status, arrived, called, doneAt)
		}
		// Walk-ins get tokens as they arrive, so later tokens arrived later.
		arrive := clock.Add(-time.Duration(30+r.IntN(10)) * time.Minute)
		for k := 0; k < 2+r.IntN(3); k++ {
			token++
			p := pats[pi%len(pats)]
			pi++
			ex(`insert into visits (patient_id, doctor_id, day, kind, token, arrived_at) values ($1, $2, $3, 'walkin', $4, $5) on conflict do nothing`,
				p.id, doc, today, token, arrive)
			arrive = arrive.Add(time.Duration(2+r.IntN(8)) * time.Minute)
		}
	}
	// make sure Meena has an 11:00 appointment with Dr. Priya (she becomes care team)
	ex(`update visits set patient_id = $1 where doctor_id = $2 and day = $3 and slot = '11:00'`, pats[0].id, priya, today)
	// Dr. Priya should be mid-consultation for the demo
	ex(`update visits set status = 'called', called_at = $3, arrived_at = coalesce(arrived_at, $3::timestamptz - interval '15 minutes')
		where id = (select id from visits where doctor_id = $1 and day = $2 and status in ('waiting', 'seen') and slot < '10:15' order by slot desc limit 1)
		and not exists (select 1 from visits where doctor_id = $1 and day = $2 and status = 'called')`, priya, today, clock.Add(-3*time.Minute))

	// wards: about three quarters full, two patients waiting for Ward A
	if err != nil {
		return err
	}
	bedRows, qerr := tx.Query(ctx, `select b.id, b.ward_id from beds b order by b.id`)
	if qerr != nil {
		return qerr
	}
	type bw struct{ bed, ward int64 }
	var beds []bw
	for bedRows.Next() {
		var x bw
		bedRows.Scan(&x.bed, &x.ward)
		beds = append(beds, x)
	}
	bedRows.Close()
	admitted := 0
	for i, b := range beds {
		if r.IntN(4) == 0 {
			continue
		}
		p := pats[10+i%45]
		ex(`insert into admissions (patient_id, doctor_id, ward_id, bed_id, reason, urgency, requested_at, admitted_at) values ($1, $2, $3, $4, $5, 'urgent', $6, $6) on conflict do nothing`,
			p.id, doctors[i%len(doctors)], b.ward, b.bed, diag[i%len(diag)], today.AddDate(0, 0, -1-r.IntN(5)).Add(15*time.Hour))
		admitted++
	}
	ex(`insert into admissions (patient_id, doctor_id, ward_id, reason, urgency, requested_at) values ($1, $2, $3, 'Dengue with low platelets, needs monitoring', 'urgent', $4)`,
		pats[56].id, priya, wards["Ward A (General)"], clock.Add(-50*time.Minute))
	ex(`insert into admissions (patient_id, doctor_id, ward_id, reason, urgency, requested_at) values ($1, $2, $3, 'Uncontrolled blood sugar, IV insulin', 'routine', $4)`,
		pats[57].id, priya, wards["Ward A (General)"], clock.Add(-20*time.Minute))

	// blood stock: whole-blood components with realistic shelf lives
	life := map[string]int{"rbc": 35, "plasma": 365, "platelets": 5}
	n := 0
	for _, g := range []string{"O+", "O+", "O+", "B+", "B+", "A+", "A+", "AB+", "O-", "A-", "B-", "AB-"} {
		for _, comp := range []string{"rbc", "rbc", "plasma", "platelets"} {
			for k := 0; k < 1+r.IntN(4); k++ {
				n++
				collected := today.AddDate(0, 0, -r.IntN(life[comp]))
				ex(`insert into blood_units (unit_no, blood_group, component, collected_on, expires_on) values ($1, $2, $3, $4, $5)`,
					fmt.Sprintf("BU-%d-%04d", today.Year(), 1000+n), g, comp, collected, collected.AddDate(0, 0, life[comp]))
			}
		}
	}
	ex(`insert into blood_units (unit_no, blood_group, component, collected_on, expires_on) values ($1, 'B+', 'rbc', $2, $3), ($4, 'O+', 'platelets', $5, $6)`,
		fmt.Sprintf("BU-%d-0998", today.Year()), today.AddDate(0, 0, -36), today.AddDate(0, 0, -1), fmt.Sprintf("BU-%d-0999", today.Year()), today.AddDate(0, 0, -6), today.AddDate(0, 0, -1))
	ex(`insert into blood_requests (patient_id, doctor_id, blood_group, component, units, urgency, created_at) values ($1, $2, $3, 'platelets', 2, 'urgent', $4)`,
		pats[56].id, priya, pats[56].group, clock.Add(-45*time.Minute))
	// donors
	for i := 0; i < 70; i++ {
		var last any
		if r.IntN(3) > 0 {
			last = today.AddDate(0, 0, -r.IntN(200))
		}
		ex(`insert into donors (name, blood_group, sex, phone, area, last_donated) values ($1, $2, $3, $4, $5, $6)`,
			personName(r, i%2)+" "+last2(r), groups[r.IntN(len(groups))], []string{"female", "male"}[i%2], fmt.Sprintf("9%09d", r.IntN(1e9)), areas[r.IntN(len(areas))], last)
	}
	_ = admitted
	return err
}

func last2(r *rand.Rand) string { return last[r.IntN(len(last))] }

// personName picks a first name of the given sex (0 female, 1 male).
func personName(r *rand.Rand, sex int) string { return first[2*r.IntN(len(first)/2)+sex] }
