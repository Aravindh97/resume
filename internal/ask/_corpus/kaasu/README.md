# Kaasu

**Live demo: [kaasu.onrender.com](https://kaasu.onrender.com/)**: sign in with one click as one of four people or as Kaasu's operations reviewer. The demo payment PIN is shown on the sign-in page. Open two people in two browsers and pay one from the other to watch the money arrive. The free server sleeps when idle, so the first visit can take up to a minute.

[![The wallet: balance, money requests waiting to be paid, and activity](docs/home.png)](https://kaasu.onrender.com/)

*A personal project, built in my own time to learn. Not affiliated with or endorsed by my employer. **All money is simulated**: nothing connects to UPI or a bank, and the people are made up. The statement analyser's findings come from fixed rules and are not financial advice.*

A personal finance app: a wallet with UPI-style payments, requests and bill splitting; a bank-statement analyser that can run entirely in your browser; budgets, savings goals and a month-end report. Kaasu (காசு) is Tamil for money.

Full stack:
- **Go** API on Postgres: a double-entry ledger, exactly-once payments with idempotency keys, row locks for balances and limits, a fraud rule, an append-only audit log, a month-end job, and live updates through Postgres `LISTEN/NOTIFY` and Server-Sent Events.
- **Go compiled to WebAssembly**: the statement analyser runs on the server or in the browser from the same Go code.
- **React + TypeScript** front end (Vite, React Router, TanStack Query). TypeScript types are generated from the Go structs.
- **Tests:** Go unit and integration tests against a real Postgres, Vitest component tests, Playwright browser tests with axe accessibility checks, and a load test.

## What it does

| Area | What you can do |
|---|---|
| **Wallet** | Add money (a simulated UPI top-up); send to a mobile number or UPI ID after seeing the registered name; request money; split a bill; download a statement for any dates |
| **Safety** | A 4-digit PIN for every payment, locked for 30 minutes after 3 wrong tries; daily limits (₹25,000 sent, ₹50,000 added); a warning for a payee you haven't paid before; a fraud rule; a log of every security event on your account |
| **Statement analyser** | Open a CSV statement from your bank. It finds regular payments and price rises, subscriptions, spending spikes, bank charges with how they're usually avoided, and possible double payments. Correct a category once and it's learned for that merchant |
| **Privacy mode** (on by default) | The statement is parsed and analysed in your browser. Only the summary is saved, never the transactions |
| **Planning** | Monthly budgets by category; savings goals that hold money aside; SIP and EMI calculators; a report for each month |
| **Operations** (Selva) | Review accounts the fraud rule paused, clear them with a written note, read the audit log, and run the ledger check |

![The statement analyser: findings, months and categories, and the rows behind each finding](docs/analyse.png)

## How the important parts work

**Every rupee is in a double-entry ledger** ([`internal/store/store.go`](internal/store/store.go)). Amounts are whole paise in `bigint`, never floats. Each movement is a transaction with two entries that sum to zero, one out of an account and one into another. A wallet top-up comes from a "bank" account standing for the outside world, so the bank's balance is minus everything ever added and all balances together are always zero. Only one function changes balances. It locks both accounts in id order, so two people paying each other at once can't deadlock, and it refuses to overdraw. A check constraint keeps every non-bank balance at zero or above, a deferred trigger re-checks that each transaction balances when it commits, and triggers reject any update or delete of the ledger. The operations page recomputes every balance from the entries.

**Payments happen exactly once.** Each payment carries an `Idempotency-Key`, generated once per action by the browser and reused if the network fails before an answer arrives. The server inserts the key in the same transaction as the money movement. A retry with the same key waits for the first to commit, then gets the first answer back with `replay: true`. If the first attempt fails, its key rolls back too, so the retry runs normally. The same key with a different payment is refused. The PIN is left out of what's stored with the key: a 4-digit PIN is easy to recover from a hash.

**One person's payments are checked one at a time.** A payment locks the sender's row (`for no key update`), then checks the pause flag, the daily limit and the fraud rule, then moves money. Twenty payments of ₹300 against a ₹2,000 balance, all sent at once, give exactly six payments and a ₹200 balance.

**The fraud rule.** A stolen phone or a scam usually means many payments to new people in a short time. The fifth payment to a payee first paid in the last day, within 10 minutes, is refused, and the account is paused in the same transaction. The refusal is stored under the payment's key, so a retry gets the same answer. The operations page hears about it live. Clearing the pause needs a written note of at least 10 characters, which goes into the audit log, and payments before the review don't count towards the rule again.

![Every payment asks for the PIN](docs/pin.png)

**The statement analyser is one Go package** ([`internal/analyse`](internal/analyse)) with no I/O, compiled for the server and, with `GOOS=js GOARCH=wasm`, for the browser:
- **Parsing.** It finds the header row below the bank's preamble and understands several layouts: Withdrawal/Deposit columns, Debit/Credit columns, a signed amount, an amount plus a Dr/Cr column, or "1,250.00 Dr". Dates are read day first, as Indian banks write them. Lines that aren't transactions are listed with the reason they were skipped.
- **Merchants.** A narration like `UPI/312345678901/SWIGGY/swiggy@icici/Payment` becomes `SWIGGY`: reference numbers, bank codes and payment words are dropped.
- **Categories.** Keyword rules give the first guess. A correction saves "this merchant is this category", which re-categorises every row from that merchant and every later statement.
- **Findings.** A payment is recurring if there are at least three, 26 to 35 days apart (or 6 to 8), with earlier amounts within 15% of each other. A price rise needs a fixed price that went up, so a varying electricity bill doesn't count. A spike is a category in the latest full month at least 1.5 times its earlier average and ₹1,000 more. Bank charges are grouped by kind with a general note on avoiding them. A possible double payment is the same amount to the same merchant within a day.

Each finding lists the statement lines behind it, and the page shows those rows when you pick it.

**Privacy mode keeps the rows on your device.** The page downloads the analyser (3.7 MB, 1.0 MB compressed), reads the file in the tab, and sends only the summary: totals by month and category, and the findings' titles and amounts. A browser test watches every request and checks no narration leaves the page. The server bounds the summary's size, since it can't check it against rows it never sees. When you correct a category, the merchant name and category are sent so the rule is remembered. The browser re-analyses and replaces the saved summary. Server-mode imports keep their rows, so budgets can count them.

![Budgets, savings goals and the SIP and EMI calculators](docs/plan.png)

**Month-end reports** are made by a job that runs every hour on every server instance. A transaction-level advisory lock stops two runs working at once, and the `(user, month)` primary key makes a second report impossible. In the test, five runs at once make exactly one report per member, and the report's total out matches the ledger.

**The statement download is safe to open in a spreadsheet.** A note like `=HYPERLINK(...)` is written as `'=HYPERLINK(...)`, so it stays text (CSV injection). The download reads back through Kaasu's own analyser with the same totals as the activity list.

**Sessions and CSRF.** Passwords and PINs are hashed with bcrypt, logins are throttled, and only the SHA-256 of a session token is stored. Sessions last 2 hours. The cookie is HttpOnly and SameSite=Lax. Every request that changes data must carry `X-Kaasu: 1` and come from this site's Origin. The content security policy allows WebAssembly to compile (`wasm-unsafe-eval`) but not JavaScript `eval`.

![The operations reviewer: paused accounts, the ledger check and the audit log](docs/ops.png)

## Tests

| Layer | What is checked |
|---|---|
| Money (Go) | parsing rupee amounts as banks and people write them; Indian digit grouping; 5,000 random round trips; bill splits that add up exactly |
| Analyser (Go) | five bank layouts; skipped-line reporting; merchant names; categories, including that income words on money out aren't income; every expected finding in the sample statement; on 200 random statements, totals by month and category always equal the rows; learned rules |
| API against real Postgres (Go) | 20 concurrent payments can't overdraw; 10 concurrent retries with one key move money once; a key reused for a different payment is refused; the PIN isn't stored; 3 wrong PINs lock payments; daily limits; the fraud rule, its retry, and the review; a request paid from two devices at once is paid once; split shares add up; goals keep money in the ledger; server and device imports, learning, and isolation between users; five simultaneous month-end runs; the statement download; CSRF and roles; live notices reach the right person; compressed static files |
| Components (Vitest) | money formatting and parsing; splits matching the server; SIP and EMI against known values; a network retry reuses its key while a refusal doesn't retry; the send flow with a wrong PIN and a retry; privacy mode sending only the summary; role redirects |
| Browser (Playwright) | a payment appears on the other person's phone without reloading; paying a request after a wrong PIN; the fraud rule pausing an account and the reviewer clearing it live; privacy mode in a real browser with WebAssembly, watching every request, plus a learned correction; axe accessibility checks on seven screens |

All of it runs in [CI](.github/workflows/ci.yml) with a Postgres service, and every Go test leaves the ledger check passing.

**Bugs the tests found:**
- **Deadlocks in concurrent payments.** The concurrency test found that locking a user's row `for update` deadlocked with the inserts that reference it, which take a key-share lock on the same row. It now uses `for no key update`.
- **Negative amounts.** The money round-trip test found that `-₹5` couldn't be read back, because the sign came before the currency symbol.
- **Merchant names.** The analyser test found `HOTSTAR SUBSCRIPTION`, which mixed the payee with the note. Merchants now come from one part of the narration.
- **Price rises.** Varying electricity bills were reported as price rises. A price rise now needs a fixed price.
- **The PIN dialog.** A browser run found the PIN dialog's form nested inside the payment form, so pressing Pay did nothing.
- **Slow payments.** The load test found that hashing PINs with the password cost (bcrypt cost 10) took 54 ms of CPU per payment check and slowed every other request: 136 requests a second, with payment p99 at 1.1 s. A 4-digit PIN has only 10,000 values, so no hash cost protects a leaked hash; the 3-tries lock does. PINs now use cost 8, which takes 14 ms.

Load, from [`docs/loadtest.txt`](docs/loadtest.txt): the three friends paying each other from 8 devices each (24 sending at once), a fifth of payments sent twice with the same key, and 30 sessions reading wallets. Server, database and load generator ran on one 8-core machine for 20 s:

| Request | Count | p50 | p99 |
|---|---|---|---|
| send payment | 2,446 | 195 ms | 262 ms |
| wallet | 3,930 | 74 ms | 112 ms |
| activity | 3,930 | 79 ms | 115 ms |

10,306 requests (515 a second), no errors. 2,015 payments went through, 431 were sent twice, and no key was ever answered with two different payments. The ledger check passed afterwards. Payment latency is high on purpose: each person's payments go one at a time, so a person's 8 devices queue behind each other. Payments by different people run in parallel.

Statement analysis of 1,500 rows takes about 8 ms on the server (`go test -bench Large ./internal/analyse`) and about 55 ms in Chrome through WebAssembly.

## Run it

Needs Go 1.26, Node 24 and Postgres 16.

```sh
DATABASE_URL=postgres://... DEMO=1 go run ./cmd/kaasu     # API on http://127.0.0.1:8080
cd web && ./build-wasm.sh && npm ci && npm run build       # analyser and page into ../dist
STATIC_DIR=dist DATABASE_URL=... DEMO=1 go run ./cmd/kaasu
```

`DEMO=1` loads four months of made-up activity, a sample statement, goals, budgets and reports, adds the one-click logins, and resets every 6 hours.

```sh
KAASU_TEST_DATABASE_URL=... go test -race -p 1 ./...      # Go tests
cd web && npm test                                         # component tests
KAASU_E2E_DATABASE_URL=... npm run e2e                     # browser tests
go run ./cmd/loadtest -url http://127.0.0.1:8080 -devices 8 -readers 30 -for 20s
```

## Deploy (free)

- **Database:** create a free Postgres on [Neon](https://neon.com) (no card) and copy its connection string.
- **Server:** on [Render](https://render.com), choose New → Blueprint → this repo; [`render.yaml`](render.yaml) builds the [`Dockerfile`](Dockerfile) (an 18 MB image with the analyser inside). Paste the Neon string as `DATABASE_URL`. Migrations run at start-up.

## Design decisions

- **Why a ledger instead of a balance column?** A balance alone can't explain itself. With entries, every balance can be recomputed and checked, and a statement is just a query.
- **Why idempotency keys, when the button is disabled while sending?** The button stops double clicks. It can't stop a phone that sent the payment, lost the connection, and asks again. Only the server can tell a retry from a second payment, and only if the request says which action it belongs to.
- **Why pause the account instead of just refusing the payment?** If someone else has the phone, refusing one payment just leads to a sixth attempt. A pause stops everything until a person checks, and receiving money still works.
- **Why run the analyser in the browser?** A bank statement shows where someone lives, shops and banks. Most of what the analyser shows doesn't need the server, so by default the server never sees the rows. Compiling the same Go package to WebAssembly means there's one implementation to test, not a Go one and a TypeScript one that drift apart.
- **Why rules and not a model for categories?** The rules are readable and easy to correct, and corrections take effect at once. A model would need training data this demo doesn't have.

## Limits

- All money is simulated. There is no UPI, bank or KYC integration.
- Runs as a demo only: without `DEMO=1` there's no sign-up yet.
- The analyser reads CSV statements. PDF statements need converting first (many banks offer CSV or Excel downloads).
- Findings are rules with fixed thresholds. They can miss things or flag things that are fine, and they aren't financial advice.
- The SIP and EMI calculators are the standard formulas. Real returns vary, and a lender's EMI can differ by fees and rounding.
- The limits and the fraud rule are set to look plausible; they aren't any regulator's rules.
