// Package seed fills an empty database with a demo factory: Kaveri Knits in
// Tiruppur, its buyers, staff and buyer accounts, and orders at every stage
// with a believable history. Dates are relative to today, so the demo always
// has orders due this week, at risk and late.
package seed

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"math/rand/v2"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Aravindh97/thari/internal/auth"
	"github.com/Aravindh97/thari/internal/domain"
)

type DemoUser struct {
	Email, Name, Role, Org string
}

var Users = []DemoUser{
	{"lakshmi@kaveriknits.demo", "Lakshmi Narayanan", domain.Admin, "Kaveri Knits"},
	{"arun@kaveriknits.demo", "Arun Kumar", domain.Merchandiser, "Kaveri Knits"},
	{"selvi@kaveriknits.demo", "Selvi Murugan", domain.Production, "Kaveri Knits"},
	{"jana@nordwind.demo", "Jana Becker", domain.Buyer, "Nordwind GmbH"},
	{"maya@urbanthread.demo", "Maya Collins", domain.Buyer, "Urban Thread Co."},
	{"kenji@sakura.demo", "Kenji Sato", domain.Buyer, "Sakura Apparel"},
}

var buyers = []struct{ name, city, prefix string }{
	{"Nordwind GmbH", "Hamburg", "NW"}, {"Urban Thread Co.", "London", "UT"}, {"Sakura Apparel", "Osaka", "SK"}, {"Maple & Pine", "Toronto", "MP"},
}

var styles = []struct{ style, desc, colour string }{
	{"Men's crew-neck T-shirt", "180 GSM single jersey, 100% combed cotton, reactive dyed", "Navy"},
	{"Women's V-neck T-shirt", "160 GSM single jersey, cotton/elastane 95/5", "Heather grey"},
	{"Kids' fleece hoodie", "320 GSM 3-thread fleece, brushed back, kangaroo pocket", "Mustard"},
	{"Men's pique polo", "220 GSM pique, rib collar and cuffs, 3 buttons", "White"},
	{"Women's track pants", "280 GSM French terry, elastic waist with drawcord", "Black"},
	{"Thermal leggings", "200 GSM interlock, cotton/modal", "Charcoal"},
	{"Baby romper", "180 GSM organic cotton interlock, snap closure", "Off-white"},
	{"Unisex sweatshirt", "300 GSM loopback, ribbed hem", "Forest green"},
	{"Men's boxer shorts", "160 GSM single jersey, covered waistband", "Assorted stripes"},
	{"Women's night dress", "150 GSM slub jersey, printed", "Floral print"},
}

// Seed adds the demo data if the database has no organisations yet.
func Seed(ctx context.Context, db *pgxpool.Pool, today time.Time, bcryptCost int) error {
	var n int
	if err := db.QueryRow(ctx, `select count(*) from orgs`).Scan(&n); err != nil {
		return err
	}
	if n > 0 {
		return nil
	}
	tx, err := db.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if err := seed(ctx, tx, today, bcryptCost); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// Reset deletes everything and seeds again (demo mode only).
func Reset(ctx context.Context, db *pgxpool.Pool, today time.Time, bcryptCost int) error {
	tx, err := db.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, `truncate files, inspections, events, progress, orders, sessions, users, orgs restart identity cascade`); err != nil {
		return err
	}
	if err := seed(ctx, tx, today, bcryptCost); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func seed(ctx context.Context, tx pgx.Tx, today time.Time, cost int) error {
	var factory int64
	if err := tx.QueryRow(ctx, `insert into orgs (name, kind, city) values ('Kaveri Knits', 'factory', 'Tiruppur') returning id`).Scan(&factory); err != nil {
		return err
	}
	orgs := map[string]int64{"Kaveri Knits": factory}
	var buyerIDs []int64
	for _, b := range buyers {
		var id int64
		if err := tx.QueryRow(ctx, `insert into orgs (name, kind, city, factory_id) values ($1, 'buyer', $2, $3) returning id`, b.name, b.city, factory).Scan(&id); err != nil {
			return err
		}
		orgs[b.name] = id
		buyerIDs = append(buyerIDs, id)
	}
	// Demo accounts get an unusable random password: they sign in with the
	// one-click demo login, which only exists when the server runs with DEMO=1.
	users := map[string]int64{}
	for _, u := range Users {
		h, err := auth.HashPassword(auth.RandomPassword(), cost)
		if err != nil {
			return err
		}
		var id int64
		if err := tx.QueryRow(ctx, `insert into users (org_id, email, name, role, password_hash, demo) values ($1, $2, $3, $4, $5, true) returning id`,
			orgs[u.Org], u.Email, u.Name, u.Role, h).Scan(&id); err != nil {
			return err
		}
		users[u.Role+":"+u.Org] = id
	}
	arun, selvi, lakshmi := users["merchandiser:Kaveri Knits"], users["production:Kaveri Knits"], users["admin:Kaveri Knits"]
	buyerUser := map[int64]int64{orgs["Nordwind GmbH"]: users["buyer:Nordwind GmbH"], orgs["Urban Thread Co."]: users["buyer:Urban Thread Co."], orgs["Sakura Apparel"]: users["buyer:Sakura Apparel"]}

	r := rand.New(rand.NewPCG(7, 7))
	day := func(d int) time.Time { return today.AddDate(0, 0, d) }
	ev := func(order int64, user int64, at time.Time, kind string, detail any, visible bool) error {
		b, _ := json.Marshal(detail)
		_, err := tx.Exec(ctx, `insert into events (factory_id, order_id, user_id, at, kind, detail, buyer_visible) values ($1, $2, $3, $4, $5, $6, $7)`,
			factory, order, user, at, kind, b, visible)
		return err
	}
	// Each order: when it was placed, how long it has, and how far along it is
	// relative to schedule (pace 1 = exactly on plan).
	type plan struct {
		start, length int
		pace          float64
		shipped       bool
	}
	plans := []plan{
		{-75, 50, 1, true}, {-70, 45, 1, true}, {-66, 50, 1, true}, {-60, 40, 1, true},
		{-55, 52, 0.55, false}, // late
		{-50, 48, 0.62, false}, // late
		{-45, 47, 0.97, false}, {-44, 48, 1.02, false}, {-40, 43, 0.9, false}, {-38, 42, 1.0, false},
		{-36, 44, 0.6, false}, {-34, 45, 1.1, false}, {-30, 40, 0.65, false}, {-28, 45, 1.0, false},
		{-25, 45, 0.95, false}, {-22, 40, 1.15, false}, {-20, 50, 0.7, false}, {-18, 45, 1.0, false},
		{-15, 40, 1.05, false}, {-12, 45, 0.9, false}, {-10, 50, 1.0, false}, {-8, 42, 1.2, false},
		{-6, 45, 1.0, false}, {-4, 50, 1.0, false}, {-3, 40, 1, false}, {-1, 48, 1, false},
	}
	for i, pl := range plans {
		b := i % len(buyers)
		st := styles[(i*3)%len(styles)]
		q := (4 + r.IntN(36)) * 250
		price := int64(18000 + r.IntN(42)*1000) // ₹180 to ₹600 per piece
		orderedOn, due := day(pl.start), day(pl.start+pl.length)
		po := fmt.Sprintf("%s-%d-%04d", buyers[b].prefix, orderedOn.Year(), 1000+i*37)
		status := "open"
		if pl.shipped {
			status = "shipped"
		}
		var id int64
		if err := tx.QueryRow(ctx, `insert into orders (factory_id, buyer_id, po_number, style, description, colour, quantity, unit_price_paise, ordered_on, due_date, status, version, created_at)
			values ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, 10, $12) returning id`,
			factory, buyerIDs[b], po, st.style, st.desc, st.colour, q, price, orderedOn, due, status, orderedOn.Add(10*time.Hour)).Scan(&id); err != nil {
			return err
		}
		if err := ev(id, arun, orderedOn.Add(10*time.Hour), "created", map[string]any{"po_number": po, "quantity": q, "due_date": due.Format(time.DateOnly)}, true); err != nil {
			return err
		}
		// Share of the plan reached by today, then per-stage counts that
		// overlap the way a real line does (cutting starts before all fabric is in).
		used := math.Min(1, today.Sub(orderedOn).Hours()/due.Sub(orderedOn).Hours())
		c := math.Min(1, used*pl.pace)
		if pl.shipped {
			c = 1
		}
		done := make([]int, len(domain.Stages))
		for s := range domain.Stages {
			f := math.Max(0, math.Min(1, (c*6.0-float64(s))*0.95))
			if c == 1 {
				f = 1
			}
			done[s] = int(math.Round(float64(q)*f/50)) * 50
			if s > 0 && done[s] > done[s-1] {
				done[s] = done[s-1]
			}
		}
		for s, stage := range domain.Stages {
			if _, err := tx.Exec(ctx, `insert into progress (order_id, stage, done) values ($1, $2, $3)`, id, stage, done[s]); err != nil {
				return err
			}
			// log the pieces in a few batches across the elapsed time
			left, batches := done[s], 1+r.IntN(3)
			for k := 0; k < batches && left > 0; k++ {
				part := left
				if k < batches-1 {
					part = left / 2 / 50 * 50
				}
				if part == 0 {
					continue
				}
				left -= part
				t0 := orderedOn.Add(time.Duration(float64(today.Sub(orderedOn)) * (0.15 + 0.8*float64(s*batches+k+1)/float64(len(domain.Stages)*batches+1))))
				if t0.After(today) {
					t0 = today.Add(-time.Hour)
				}
				if err := ev(id, selvi, t0, "progress", map[string]any{"stage": stage, "delta": part, "done": done[s] - left}, true); err != nil {
					return err
				}
			}
		}
		if bu, ok := buyerUser[buyerIDs[b]]; ok && i%3 == 0 {
			if err := ev(id, bu, orderedOn.AddDate(0, 0, 3).Add(14*time.Hour), "comment", map[string]any{"text": "Please confirm the shade matches the approved lab dip before bulk cutting."}, true); err != nil {
				return err
			}
			if err := ev(id, arun, orderedOn.AddDate(0, 0, 4).Add(11*time.Hour), "comment", map[string]any{"text": "Confirmed. Shade band approved, bulk fabric matches within tolerance."}, true); err != nil {
				return err
			}
		}
		if pl.pace < 0.7 {
			if err := ev(id, arun, today.AddDate(0, 0, -5), "comment", map[string]any{"text": "Fabric held at the dyeing unit for re-processing. Expect 4 days' delay; asked for Sunday shifts.", "internal": true}, false); err != nil {
				return err
			}
		}
		if done[len(done)-1] == q {
			plan := domain.PlanFor(q)
			at := due.AddDate(0, 0, -3)
			if !pl.shipped {
				at = today.AddDate(0, 0, -1)
			}
			if i == 1 { // one order failed its first inspection and was reworked
				if _, err := tx.Exec(ctx, `insert into inspections (order_id, inspector_id, at, lot_size, sample_size, critical, major, minor, result, notes) values ($1, $2, $3, $4, $5, 0, $6, 3, 'fail', 'Uneven side seams on size L; rework 2 cartons')`,
					id, selvi, at.AddDate(0, 0, -2), q, plan.SampleSize, plan.AcceptMajor+2); err != nil {
					return err
				}
				if err := ev(id, selvi, at.AddDate(0, 0, -2), "inspection", map[string]any{"result": "fail", "sample_size": plan.SampleSize, "lot_size": q, "critical": 0, "major": plan.AcceptMajor + 2, "minor": 3}, true); err != nil {
					return err
				}
			}
			if _, err := tx.Exec(ctx, `insert into inspections (order_id, inspector_id, at, lot_size, sample_size, critical, major, minor, result) values ($1, $2, $3, $4, $5, 0, $6, $7, 'pass')`,
				id, selvi, at, q, plan.SampleSize, plan.AcceptMajor/2, plan.AcceptMinor/2); err != nil {
				return err
			}
			if err := ev(id, selvi, at, "inspection", map[string]any{"result": "pass", "sample_size": plan.SampleSize, "lot_size": q, "critical": 0, "major": plan.AcceptMajor / 2, "minor": plan.AcceptMinor / 2}, true); err != nil {
				return err
			}
			if pl.shipped {
				if err := ev(id, lakshmi, due.AddDate(0, 0, -1), "shipped", map[string]any{"quantity": q}, true); err != nil {
					return err
				}
			}
		}
	}
	return nil
}
