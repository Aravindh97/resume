# paisa

An exactly-once, double-entry ledger with UPI-style settlement reconciliation, in Go and PostgreSQL.

Payment systems fail in a few common ways:
- a request times out after the money moved, the client retries, and the customer is charged twice
- a crash halfway through leaves the books unbalanced
- the bank's settlement file disagrees with your records, and nobody notices

Paisa is built so that none of these can happen silently. This README shows the tests that check it.

## Results

All runs use Postgres 16 in Docker on a local machine, with 32 concurrent workers, 2,000 customer wallets and 50 merchants. Full output is in [`docs/`](docs/).

| Test | Result |
|---|---|
| 50,000 payments with injected faults (see below) | books balance, with **0** errors in every check: unbalanced entries, cached balances, total sum, negative balances, duplicate references |
| **kill -9** of the payment process 20 times at random moments, then restart | the ledger was consistent after **every** kill, and the final balances of all 2,068 accounts were **identical** to a run that was never killed (md5 over every account balance) |
| Settlement reconciliation against a bank file with planted errors | **477 of 477** planted mismatches found and classified correctly; running the fix a second time changed **0** entries |
| Hot account: one fee account vs 16 shards | **437 → 2,981 payments/s** (6.8×) at 5,000 payments; 3,360–4,640/s at 50,000 |

**Faults injected during the 50,000-payment run:**
- 5% of ledger writes crash just before commit, then the request is retried
- 5% have the response lost after commit; the client then sends **two retries at once** with the same key
- 3% are refunded, and a third of those refunds are requested twice
- 0.4% are abandoned by the service after the bank had already settled them (only recon can find these)
- the bank file drops, alters or duplicates about 0.7% of lines

## How it works

**Double entry.**
- Every movement is an entry with two or more postings that **sum to zero**. Money is moved, never created.
- A payment of ₹100 with a 1% fee is three postings: customer −10000 paise, merchant +9900, fee +100.
- Amounts are `int64` paise. Floating point is never used for money.

**Exactly once.** `Post` runs in one transaction:
1. It takes an advisory lock on the idempotency key, so two concurrent retries can't both get past step 2.
2. It checks whether the key has been seen. If so, it returns the stored outcome. If the key arrives with *different* content, it rejects the request (`ErrKeyReuse`). That catches a client bug that would otherwise silently drop a payment.
3. It locks the accounts **in sorted order**, which rules out deadlocks, and checks funds.
4. It writes the entry, its postings and the cached balances, then commits.

A crash anywhere before the commit leaves nothing behind. A retry after the commit finds the key and returns the same result.

Rejections such as insufficient funds are stored under their key too. If a retry arrives after the customer has topped up, it still gets the original "rejected". That prevents a late retry from paying for something the client has already told the user failed.

**The hot-account problem.** Every payment credits the platform's fee account, so with a single fee account every payment waits for the same row lock. Splitting it into 16 shards (chosen by payment) spreads that lock out, and the true fee total is the sum of the shards. This took throughput from 437 to 2,981 payments per second.

**Reconciliation** (`internal/recon`). Paisa matches the bank's settlement CSV against ledger payments by UPI reference and classifies every difference:

| Mismatch | Usual cause | Fix (idempotent, key `recon:<kind>:<ref>`) |
|---|---|---|
| `missing_in_ledger` | our request timed out after the bank succeeded | book the payment from `bank:clearing` |
| `missing_in_bank` | the bank failed after we booked, or settles late | move the amount to `suspense` for investigation |
| `amount_mismatch` | wrong amount on one side | post the difference between merchant and `suspense` |
| `duplicate_in_bank` | the bank settled the same reference twice | book the extra to `suspense`, to be returned |

Nothing is ever deleted. Every correction is a new entry, so the history explains itself.

**Audit.** The audit recomputes every invariant from raw postings:
- each entry sums to 0
- each cached balance equals the sum of its postings
- the sum of all balances is 0
- no forbidden negative balances
- no payment reference booked twice

It takes about 100 ms for 130,000 postings.

## Run it

```bash
cp .env.example .env && docker compose up -d && set -a && . ./.env && set +a
go run ./cmd/paisa run -payments 50000 -bank-file bank.csv
go run ./cmd/paisa run -payments 5000 -fee-shards 1      # see the hot account
go build -o paisa ./cmd/paisa && ./paisa killtest -rich -kills 20 -payments 50000
go test -race -tags integration -p 1 ./...
```

`-rich` gives every customer enough money that no payment is ever rejected. This makes the final balances independent of the order in which workers run, so a run that was killed can be compared with a clean run balance by balance. Without `-rich` the invariants still hold after every kill, but *which* payments get rejected for insufficient funds depends on timing.

## Limits

- **One database.** Real ledgers shard by account and need a protocol for transfers that cross shards. The fee-account sharding here is the single-node version of that idea.
- **Simulated bank file.** The CSV is generated with known planted errors. Real UPI and NPCI settlement formats differ.
- **No currency conversion and no interest.** It handles a single currency (INR, in paise).
- **No database-level kill test.** The kill test kills the application process. It doesn't kill Postgres itself, whose crash safety comes from its write-ahead log.

## Explain it in 2 minutes

"Payments fail in boring ways: timeouts, retries, crashes, and bank files that don't match. Paisa is a double-entry ledger, so every movement sums to zero, and the audit can prove the books balance from raw postings. Each request carries an idempotency key that is stored in the same transaction as its postings. So a request either fully happened, with its key recorded, or didn't happen at all. Retries get the original result, even a rejection. To prove it, I killed the process with kill -9 twenty times during 50,000 payments. The books balanced after every kill, and the final balances matched a clean run exactly. Then I reconciled against a bank file with 477 planted errors. All 477 were found and fixed, and running the fix again changed nothing. The one performance problem was the fee account every payment touches. Sharding it took throughput from 437 to about 3,000 payments a second."

## Likely interview questions

- **Why not just a `UNIQUE` constraint on the idempotency key?** That alone prevents a double write, but two concurrent retries would both do the work and one would fail with an error. The advisory lock makes the second retry wait, then return the stored result cleanly.
- **Why store rejections?** So the answer to a key never changes. Otherwise a retry after a top-up could pay for something the user was already told had failed.
- **Why not floats?** 0.1 + 0.2 ≠ 0.3 in binary floating point. Use integers of the smallest currency unit.
- **How do you scale writes?** Shard hot accounts, batch postings, and partition by account across databases, with a two-phase or saga protocol for transfers that cross partitions.
- **What does `kill -9` prove that the crash hook doesn't?** The hook aborts inside the process. `kill -9` ends the process with transactions in flight and no cleanup code running. Postgres rolls back anything uncommitted, and the idempotency keys make the restart redo exactly the missing work.
