# Thari

**Live demo: [thari.onrender.com](https://thari.onrender.com/)**: sign in with one click as the factory admin, a merchandiser, a production supervisor or one of three buyers. Open two roles in two browsers to watch updates arrive live. The free server sleeps when idle, so the first visit can take up to a minute.

[![The dashboard for a merchandiser at Kaveri Knits](docs/dashboard.png)](https://thari.onrender.com/)

*A personal project, built in my own time to learn. Not affiliated with or endorsed by my employer. The demo factory, buyers and orders are made up.*

Order tracking for a garment factory, from fabric to packing, with a live view for buyers. Thari (தறி) is Tamil for loom; the demo factory is a knitwear unit in Tiruppur making T-shirts, hoodies and track pants for buyers in Hamburg, London and Osaka.

Full stack:
- **Go** API on Postgres, with sessions, role-based permissions, row-locked progress updates, an append-only audit log, and live updates through Postgres `LISTEN/NOTIFY` and Server-Sent Events.
- **React 19 + TypeScript**: React Router, TanStack Query, react-hook-form with zod, code-split pages, a phone layout for the factory floor.
- **Tests:** Go integration tests against real Postgres, Vitest component tests, and Playwright driving real browsers against the real server, with axe accessibility checks.

## What it does

| Who | Can |
|---|---|
| **Production** (Selvi) | log pieces through each stage from a phone; record final inspections; comment |
| **Merchandiser** (Arun) | create and edit orders and prices; correct counts; attach tech packs; ship |
| **Admin** (Lakshmi) | all of that, plus people and the audit log |
| **Buyer** (Jana, Maya, Kenji) | see only their own company's orders, live; comment; download files. Never prices or internal notes. |

- **Stages:** fabric received → cut → stitched → checked → packed, each counted in pieces. A stage can never get ahead of the one before it.
- **Risk:** each open order is on track, at risk or late, by comparing completion with the share of its production window already used.
- **Final inspection by AQL:** for the packed lot size the app gives the sample size and acceptance numbers. It uses ISO 2859-1, general inspection level II, AQL 2.5 for major defects and 4.0 for minor, the levels most garment buyers use. An order can't ship until a passed inspection covers the full quantity.

![An order: stage progress, AQL inspection, files and the timeline](docs/order.png)

## How the important parts work

**Permissions are in the queries, not just the UI** ([`internal/store`](internal/store/store.go)). Every query takes the signed-in user and adds their visibility rule to its `where` clause: a buyer's query is limited to `buyer_id = <their company>`. An order a user may not see is reported as not found, not forbidden, so ids reveal nothing. Field-level rules apply on the way out: prices are removed from responses for production and buyers. Internal notes and price changes are stored as events that buyers never receive, from the API or the live stream. The rules are one table in [`internal/domain`](internal/domain/domain.go), and a test checks the whole matrix.

**No lost updates from the floor.** Logging pieces locks the order row, reads the latest counts, checks the stage rule, writes the new count and an event, then commits. Two supervisors logging at the same moment are applied one after the other. The lock and the read are separate statements, which matters. In Postgres's default isolation level, a `select … for update` that waits for a lock rechecks the locked row, but its subqueries still see the data from before the wait. My first version read the counts in the same statement as the lock, and the concurrency test caught it: 40 concurrent updates were all "accepted", but only 8 stuck.

**Edits use optimistic locking.** Every order has a version. An edit made from an older version gets `409 Conflict` with the current order. The page then shows what changed and lets you save on top of the latest version.

**Live updates come from the database.** Each change runs `pg_notify` inside its transaction, so the notice is sent only if the change commits. Every server instance listens and pushes the notice over Server-Sent Events to the connected users allowed to see it. The browser then refetches the affected queries. Because notices come from Postgres, this works with any number of server instances. When a browser reconnects it refetches everything, in case it missed something.

**Sessions and CSRF.** Passwords are hashed with bcrypt, and a missing account still costs one bcrypt comparison, so login timing doesn't reveal which emails exist. Logins are throttled: 5 failures per email or per IP in 10 minutes. The session cookie is HttpOnly and SameSite=Lax, and only the SHA-256 of the token is stored. Every request that changes data must carry `X-Thari: 1` and come from this site's Origin. Another website can't add that header to a cross-site request without a CORS preflight, which the server never approves. Disabling an account or changing a password ends its sessions.

**The audit log is append-only.** A trigger rejects any `update` or `delete` on the events table.

**Uploads are checked by content, not name.** Only PDF, PNG and JPEG files, detected from the file's bytes, up to 2 MB. Downloads are sent as attachments with a sandboxing Content-Security-Policy, and only to users who can see the order.

## Tests

| What | How |
|---|---|
| Business rules | unit tests: stage rules (plus 2,000 random sequences that must keep stages in order), risk, AQL table, the full permission matrix |
| API against real Postgres | buyer isolation (lists, direct ids, files, comments); prices and internal notes hidden; 40 concurrent progress updates where exactly 20 fit; edit conflicts; shipping rules; append-only events; sessions, CSRF and login throttling; disabling accounts; upload type and size; live updates reaching only the right users; pagination visiting every order once; the SQL risk function matching the Go one on 3,000 random inputs |
| React components | Vitest + Testing Library, with the API faked: login redirects, role-based navigation, server validation errors shown on the right field, the edit-conflict flow |
| Real browsers | Playwright against the real server and database: a buyer's open order updates when production logs pieces on a phone; a buyer can't open another buyer's order by URL; internal notes never reach the buyer; axe finds no serious or critical WCAG 2 AA problems on six pages. axe first caught two badge colours below 4.5:1 contrast, which I darkened. |

Load, from [`docs/loadtest.txt`](docs/loadtest.txt): 50 users for 20 s (production logging pieces while merchandisers and buyers browse), with server, database and load generator on one 8-core machine:

| Request | Count | p50 | p99 |
|---|---|---|---|
| list orders | 31,827 | 8.1 ms | 11.6 ms |
| open an order | 31,827 | 19.4 ms | 25.3 ms |
| log progress | 3,981 | 14.6 ms | 19.5 ms |
| dashboard | 6,409 | 8.0 ms | 11.5 ms |

That is 3,702 requests a second with no errors. The 3 rejected progress updates were stages already full. Afterwards, no order had a stage ahead of the one before it. These numbers are for one machine; the free host has a fraction of a CPU.

![Logging pieces from a phone](docs/phone.png)

## Run it

```
docker run -d --name thari-pg -e POSTGRES_PASSWORD=dev -p 127.0.0.1:5432:5432 postgres:16-alpine
cd web && npm ci && npx vite build && cd ..
DATABASE_URL=postgres://postgres:dev@127.0.0.1:5432/postgres?sslmode=disable DEMO=1 STATIC_DIR=dist go run ./cmd/thari
# http://127.0.0.1:8080

THARI_TEST_DATABASE_URL=... go test -race -p 1 ./...     # Go tests (they reset that database)
cd web && npm test                                         # component tests
THARI_E2E_DATABASE_URL=... npm run e2e                     # browser tests
go run ./cmd/loadtest -url http://127.0.0.1:8080 -users 50 -for 20s
```

Without `DEMO=1` there are no demo accounts. Create the first admin with `go run ./cmd/thari create-admin EMAIL NAME "Factory name"`, which prints a one-time password.

## Deploy (free)

- **Database:** create a free Postgres on [Neon](https://neon.com) (no card) and copy its connection string.
- **Server:** on [Render](https://render.com), choose New → Blueprint → this repo; [`render.yaml`](render.yaml) builds the [`Dockerfile`](Dockerfile) (a 14 MB image). Paste the Neon string as `DATABASE_URL`. Migrations run at start-up.

## Design decisions

- **Why Server-Sent Events instead of WebSockets?** Updates only flow from server to browser; changes go through normal HTTP requests, where the CSRF and permission checks already are. SSE reconnects by itself and works through ordinary proxies.
- **Why refetch on an event instead of sending the data?** The event carries only ids. The browser then asks for what it shows through the normal API, which applies the normal permission rules. Nothing private can leak through the stream.
- **Why both a Go and a SQL version of the risk rule?** The page shows risk computed in Go. The list's risk filter has to run in SQL, or pagination breaks. A test checks the two agree on 3,000 random cases.
- **Why cursor pagination?** Offsets skip or repeat rows when orders change between pages; a `(due_date, id)` cursor doesn't.

## Limits

- One factory per deployment in practice (the schema has `factory_id` everywhere, but there is no sign-up flow for new factories).
- Files are stored in Postgres, which is fine at 2 MB per file for a demo; real use would put them in object storage.
- No email: new accounts get a one-time password to hand over in person.
- The AQL table covers the common levels only (normal inspection, level II, AQL 2.5 and 4.0), not tightened or reduced inspection.
