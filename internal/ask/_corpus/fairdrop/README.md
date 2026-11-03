# fairdrop

**Live demo: [aravindh97.github.io/fairdrop](https://aravindh97.github.io/fairdrop/)**: rush a show with up to 20,000 buyers and see that no seat is sold twice, run the naive version and watch seats sell many times over, then run a queue lottery on real drand randomness and try to rig it. The Go code runs in your browser as WebAssembly; see [Live demo](#live-demo) for what differs from the full version.

[![fairdrop running in the browser](docs/screenshot.png)](https://aravindh97.github.io/fairdrop/)

*A personal project, built in my own time to learn. Not affiliated with or endorsed by my employer.*

Ticketing for high-demand releases (first-day film shows, cricket, concerts) in Go and PostgreSQL. It solves two problems:

1. **No seat is ever sold twice**, even with tens of thousands of buyers clicking at once, payments timing out, and people double-clicking "Pay".
2. **The queue is provably fair.** The queue order comes from public randomness that didn't exist when registration closed, so anyone can rebuild it and check that the operator didn't move anyone.

## Results

The load test ran 50,000 simulated buyers against one Postgres 16 instance (Docker, local machine). The venue had 2,000 seats in 40 rows of 50, with 200 buyers in flight at a time, and the test was run with 3 seeds. Full output is in [`docs/`](docs/).

| | safe store (this repo) | naive read-then-write |
|---|---|---|
| seats sold twice or more | **0** (3 of 3 runs) | **156,874** extra bookings for 2,000 seats |
| families of 3+ not seated together | **0** | not attempted |
| charges = bookings + refunds | **balanced** in every run | n/a |
| unexpected errors | **0** | 0 (it fails silently) |
| throughput | 4,100–5,000 buyers/s | 981 buyers/s |
| hold latency p50 / p99 | ~20 ms / 510–660 ms | 210 ms / 380 ms |
| confirm latency p99 | ~310 ms | n/a |

The naive version is the common bug: read which seats look free, then write. The fixed 1 ms gap in the code stands in for the payment call. In the run, 45,349 buyers "booked" 158,874 seats in a 2,000-seat hall.

What the simulated buyers do, so that every failure path gets hit:
- 12% abandon their hold. The seats come back when the 2-second test hold expires.
- 5% have their card declined.
- 8% hit a payment timeout and retry with the same idempotency key.
- 3% double-click Pay, sending two Confirm calls at the same time.
- Groups of 3 or more must sit together.

A real hold would last minutes. It's short here so abandoned seats get resold within the test.

## How it works

**Holding seats.** One SQL statement claims exactly the requested seats, or none of them:

```sql
WITH c AS (SELECT seat_id FROM seats
           WHERE show_id=$1 AND seat_id = ANY($5)
             AND (status='free' OR (status='held' AND hold_expires < now))
           ORDER BY seat_id FOR UPDATE SKIP LOCKED)
UPDATE seats s SET status='held', hold_id=$3, hold_expires=$4 FROM c WHERE ...
```

- If fewer rows changed than were requested, the transaction rolls back and the buyer tries the next-best block.
- `ORDER BY ... FOR UPDATE` locks seats in a fixed order, which rules out deadlocks. The first version deadlocked under load; that error was caught in testing.
- `SKIP LOCKED` means a buyer never waits behind another buyer. They see fewer seats and move on.
- **Expired holds need no cleanup job to be correct.** The condition `hold_expires < now` makes an abandoned seat claimable straight away. A background sweep only tidies the table.

**Groups.** `HoldBest` looks for the best run of adjacent free seats in one row: nearest the front first, then nearest the centre of the run. If another buyer takes that block first, it tries the next one. For groups that must sit together it returns `ErrNoBlock` rather than splitting them. The audit query checks every confirmed group of 3 or more for gaps.

**Paying exactly once** (`Confirm`):
1. Insert the idempotency key into `payments` before charging. A retry or a double-click hits the unique key and gets the first result back (`Replay: true`), so only one request ever reaches the payment provider.
2. Check that the hold is still active and belongs to this buyer.
3. Charge, passing the same key to the provider. Stripe and Razorpay both deduplicate by key.
4. Book with a conditional update ("only if I still hold these seats"). If the hold lapsed during payment, refund.

The final guard is `bookings PRIMARY KEY (show_id, seat_id)`. Even a bug in steps 1–4 can't sell a seat twice. The audit checks money as well as seats: charges must equal bookings plus refunds.

**Fair queue** (`internal/lottery`, `internal/beacon`):
1. Registration is open for a window, so registering in the first second or the last gives the same chance and bots gain nothing from speed.
2. When it closes, the operator publishes the entry list, its sha256, and a [drand](https://drand.love) round number in the future. drand is randomness published every 3 seconds by an independent network.
3. When that round appears, each entry's position is `sha256(randomness ‖ entry id)`, sorted.

The operator can't choose the order, because the list was fixed before the randomness existed. They can't choose the randomness either, because drand is run by others. `fairdrop verify DIR` refetches the round from drand itself and recomputes everything.

Tested against the real drand network:
- A 50,000-entry sale verified `OK`.
- Swapping two queue positions failed at position 1.
- Adding one entry after closing failed the commitment check.

There's a 1,000-entry sale in [`docs/example-sale`](docs/example-sale) that you can verify yourself.

## Live demo

The page at https://aravindh97.github.io/fairdrop/ is this repo's Go code compiled to WebAssembly (`cmd/web`), with a plain HTML/JS frontend in `web/`. `.github/workflows/pages.yml` builds and publishes it on every push. GitHub Pages only serves static files, so anything that needs a database or a server runs against in-memory versions in the browser; the numbers in this README come from the full version.

```bash
./web/build.sh dist && python3 -m http.server -d dist 8000   # http://127.0.0.1:8000
```

## Run it

```bash
cp .env.example .env && docker compose up -d && set -a && . ./.env && set +a
go run ./cmd/fairdrop loadtest -buyers 50000 -rows 40 -per-row 50
go run ./cmd/fairdrop loadtest -naive -buyers 50000 -rows 40 -per-row 50
go run ./cmd/fairdrop lottery -entries 50000 -out sale && go run ./cmd/fairdrop verify sale
go run ./cmd/fairdrop verify docs/example-sale
go test -race -tags integration ./...
```

The integration tests cover:
- 200 buyers racing for one seat, with exactly 1 winning
- holds being all-or-nothing
- an expired hold being reclaimed, with the late confirm rejected
- 10 concurrent confirms with one key giving one booking
- a family placed in the one row that has room
- an impossible group size refused

## Limits

- **drand signatures aren't checked locally.** `beacon` trusts the HTTPS response from `api.drand.sh`. Checking the BLS signature needs a BLS12-381 library. Any drand client can cross-check a round.
- **Multiple accounts.** The lottery stops speed bots but not one person with many accounts. That needs one entry per verified account (phone or KYC), which isn't built here.
- **Single database.** All numbers come from one Postgres instance on a laptop. Larger venues or many shows would shard by show: each show's seats live in one database, so the locking logic is unchanged.
- **Simulated payments.** The payment provider is a fake that deduplicates by key, as real providers do.

## Design decisions

- **Why not `SELECT ... FOR UPDATE` and then `UPDATE`?** You can, but you must lock in a fixed order or you get deadlocks. My first version deadlocked. Doing it in one statement with `SKIP LOCKED` also means no buyer waits on another.
- **What if the hold expires while the card is being charged?** The final update only books if this hold still owns the seats. If it doesn't, the charge is refunded and the outcome is `refunded`. The money audit checks this.
- **Why store the idempotency key before charging, not after?** If you store it after, two concurrent retries can both charge.
- **How would you scale to 1 million buyers?** Put a queue in front: the lottery admits buyers in batches. Shard by show. Keep seat maps in memory per show, with Postgres as the source of truth.
- **Could the operator cheat the lottery?** Only by changing the list after committing, which the hash catches, or by controlling drand, which is run by independent organisations. They could withhold the result entirely, but buyers would see that.
