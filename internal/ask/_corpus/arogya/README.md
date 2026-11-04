# Arogya

**Live demo: [arogya.onrender.com](https://arogya.onrender.com/)**: sign in with one click as the receptionist, a doctor, a ward nurse, the blood bank, a patient or the medical superintendent. Open the [waiting-room screen](https://arogya.onrender.com/board) in another window and watch it change as the receptionist gives tokens and the doctor calls patients. The free server sleeps when idle, so the first visit can take up to a minute.

[![The waiting-room screen: each doctor's current token, the next three, and how many are waiting](docs/board.png)](https://arogya.onrender.com/board)

*A personal project, built in my own time to learn. Not affiliated with or endorsed by my employer. Every patient, staff member and record is made up, and nothing here is medical advice. The blood-bank rules are simplified from published guidance and are not a substitute for a hospital's own procedures.*

A small hospital system for an outpatient department (OPD), the patient record, beds and the blood bank. Arogya (ஆரோக்கியம்) is Tamil for health; the demo is a hospital in Tiruppur.

Full stack:
- **Go** API on Postgres: sessions, role checks in every query, an append-only record-access log, row locks and unique indexes for the contested resources (slots, beds, blood units), and live updates through Postgres `LISTEN/NOTIFY` and Server-Sent Events.
- **React + TypeScript** front end (Vite, React Router, TanStack Query). TypeScript types are generated from the Go structs.
- **Tests:** Go unit and integration tests against a real Postgres, Vitest component tests, Playwright browser tests with axe accessibility checks, and a load test.

## What it does

| Who | What they do |
|---|---|
| **Receptionist** (Malar) | finds or registers a patient, books a 15-minute slot or gives a walk-in token, checks patients in |
| **Doctor** (Priya, Senthil) | sees their queue and calls the next patient; opens the record, writes the consultation and prescription, requests a bed, orders blood |
| **Nurse** (Kavitha, Ward A) | sees the beds on every ward, but patient names only on her own ward; gives a waiting patient a free bed |
| **Blood bank** (Suresh) | sees stock by group and component, reserves units for requests, issues them, finds and messages eligible donors |
| **Patient** (Meena) | books appointments, sees her record, **sees everyone who opened it and why**, and sets her consent choices |
| **Superintendent** (Dr. Anand) | reviews every emergency access, sees the full access log and the outgoing message queue |
| **Waiting-room screen** | public: shows token numbers only, never names |

![A doctor's queue: the patient with them now, and who is waiting](docs/queue.png)

## How the important parts work

**Who goes next is a written rule, tested by simulation** ([`internal/domain`](internal/domain/domain.go)). Booked patients who have checked in go in appointment order; after every two booked patients, one walk-in; any walk-in who has waited 45 minutes goes first; a booked patient 15 minutes late without checking in becomes a no-show. A test runs 300 simulated mornings with random arrivals and checks at every call that no booked patient was called while a walk-in had waited 45 minutes, and that walk-ins are called in token order. Calling the next patient takes a per-doctor transaction lock, so a double click can't call two people.

**Contested things are settled by the database.** The last slot of the morning has a unique index over live bookings: twenty receptionists booking it at once produce exactly one booking, and the rest are told it was taken. A bed has a unique index over current admissions, so two nurses giving the last bed to two patients can't both succeed. Blood units are reserved with `select … for update skip locked` in expiry order: in the test, ten emergency requests compete for scarce O- red cells at the same moment, and no unit is reserved twice.

**Opening a record is logged, and the patient can see the log** ([`internal/store/records.go`](internal/store/records.go)). A doctor may open a record if the patient has a visit or admission with them; a nurse, if the patient is on her ward; a patient, her own. Anyone else gets a refusal that offers emergency access ("break glass"): they must give a reason of at least 10 characters, the record opens marked as emergency access, and the entry goes to the superintendent's review list. Every open writes an access-log row with who, why they were allowed, and what they did. A trigger rejects any `update` or `delete` on the log and on the reviews. Opening a record is a `POST`, so it can't happen from a link or a prefetch.

![Emergency access asks for a reason first](docs/break-glass.png)

**The blood bank follows the compatibility rules, with a few protections.** Red cells, plasma and platelets each have their own compatibility table. Among compatible units the patient's own group goes first, then the oldest unit, so stock is used before it expires. Rh-negative patients only ever get Rh-negative red cells. Platelets from Rh-positive donors carry a few red cells, so Rh-negative girls and women under 50 get Rh-negative platelets too, unless it is an emergency. Donors are eligible again 90 days after giving (men) or 120 days (women), and their phone numbers are never shown: a message goes through the outbox instead.

![Blood stock by group and component, with units close to expiry](docs/blood.png)

**The waiting-room screen is public and shows numbers only.** It has its own endpoint and its own live stream, which carry tokens and counts and never a name; a test checks the board's response against every patient's name.

**Live updates come from the database.** Each change runs `pg_notify` inside its transaction, so a notice is sent only if the change commits. The server pushes it over Server-Sent Events, filtered by topic and role: a patient hears only about her own record, and only the superintendent hears about emergency access. A notice carries just a topic such as `beds` or `patient:12`; the browser then refetches through the normal API, which applies the full permission checks.

**Sessions and CSRF.** Passwords are hashed with bcrypt, logins are throttled, and only the SHA-256 of a session token is stored. The cookie is HttpOnly and SameSite=Lax. Every request that changes data must carry `X-Arogya: 1` and come from this site's Origin.

**FHIR export.** A doctor can download a patient's demographics and observations as a FHIR R4 Bundle (a Patient resource plus Observation resources with LOINC codes). The test checks the structure; it hasn't been run through an official FHIR validator.

![What a patient sees: her appointments, and every time someone opened her record](docs/patient.png)

## Tests

| Layer | What is checked |
|---|---|
| Domain rules (Go) | compatibility tables for each component; allocation order (own group, then oldest); Rh-negative platelet protection and its emergency exception; donor intervals; queue order, the 45-minute rule and no-shows; 300 simulated mornings; who may open a record |
| API against real Postgres (Go) | the last slot goes to exactly one of 20; a long-waiting walk-in is called first; the board shows no names; break-glass needs a reason and is logged; nurses see their ward only; the last bed goes to exactly one of two; blood is never double-allocated; blood rules through the API; FHIR; CSRF and roles; live topics reach only the right users; 30 simultaneous registrations get distinct MRNs |
| Components (Vitest) | times shown in hospital time in any browser time zone; the board updating on a live event; role menus and redirects; the break-glass flow sending the reason; the CSRF header on every request |
| Browser (Playwright) | a walk-in token and a called patient appear on the waiting-room screen without reloading, with no names; emergency access in the patient's own log; a nurse assigning a bed; the blood bank reserving units; axe accessibility checks on seven screens |

All of it runs in [CI](.github/workflows/ci.yml) with a Postgres service.

**Bugs the tests found.** The load test found that two receptionists registering patients at the same moment could get the same MRN: the first version numbered patients by counting rows, so under load about half the registrations failed. MRNs now come from a Postgres sequence ([`002_mrn_sequence.sql`](internal/db/migrations/002_mrn_sequence.sql)), with a test for it. The accessibility checks found a missing `main` landmark, unlabelled table headers and low-contrast text in the queue, all fixed.

Load, from [`docs/loadtest.txt`](docs/loadtest.txt): 50 users for 20 s, made up of 16 receptionists registering patients and giving walk-in tokens, 2 doctors calling patients non-stop, and 32 waiting-room screens. Server, database and load generator ran on one 8-core machine:

| Request | Count | p50 | p99 |
|---|---|---|---|
| register patient | 4,885 | 25 ms | 58 ms |
| walk-in token | 4,885 | 28 ms | 61 ms |
| call next patient | 669 | 28 ms | 63 ms |
| waiting-room board | 29,495 | 11 ms | 27 ms |

41,874 requests in total (about 2,100 per second), with no errors. Afterwards the load test checks that every token was given once per doctor and no doctor had two patients called at once: 0 problems across 4,270 visits.

## Run it

Needs Go 1.26, Node 24 and Postgres 16.

```sh
DATABASE_URL=postgres://... DEMO=1 go run ./cmd/arogya   # API on http://127.0.0.1:8080
cd web && npm ci && npm run build                        # builds ../dist
STATIC_DIR=dist DATABASE_URL=... DEMO=1 go run ./cmd/arogya
```

`DEMO=1` loads the synthetic hospital, adds the one-click logins, and runs a demo clock that starts each reset at 10:15 India time, so the OPD is always mid-morning. The data resets every 6 hours.

```sh
AROGYA_TEST_DATABASE_URL=... go test -race ./...           # Go tests
cd web && npm test                                         # component tests
AROGYA_E2E_DATABASE_URL=... npm run e2e                    # browser tests
go run ./cmd/loadtest -url http://127.0.0.1:8080 -users 50 -for 20s
```

## Deploy (free)

- **Database:** create a free Postgres on [Neon](https://neon.com) (no card) and copy its connection string.
- **Server:** on [Render](https://render.com), choose New → Blueprint → this repo; [`render.yaml`](render.yaml) builds the [`Dockerfile`](Dockerfile) (a 14 MB image). Paste the Neon string as `DATABASE_URL`. Migrations run at start-up.

## Design decisions

- **Why does the database decide who gets the last slot, bed or unit?** Checking availability in Go and then inserting leaves a gap where two requests both see "free". A unique index (slots, beds) or a row lock (blood units) closes it, and the tests run the race on purpose.
- **Why `skip locked` for blood?** Ten requests reserving at once shouldn't queue behind one another on the same rows. Each takes the oldest units nobody else is holding, so they run in parallel and still never share a unit.
- **Why let a doctor open any record at all?** Refusing outright fails the patient in an emergency. Break-glass access with a stated reason, a visible log and a review is the usual answer: it keeps the record available and makes misuse visible.
- **Why can the patient see the access log?** It's the strongest check on misuse, and it costs one query.
- **Why a demo clock?** A hospital demo at 3 a.m. would show an empty OPD. The clock starts each reset at 10:15 India time and moves forward in real time.

## Limits

- Runs as a demo only: without `DEMO=1` there are no accounts and no way to create them yet.
- One hospital per deployment.
- SMS reminders and donor messages go to an outbox table and are never sent.
- No billing, pharmacy stock, lab orders or discharge summaries.
- Blood-bank rules are simplified: there is no crossmatch step, no irradiated or CMV-negative products, and no paediatric dosing.
