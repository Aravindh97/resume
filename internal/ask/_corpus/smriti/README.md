# Smriti

**Live demo: [aravindh97.github.io/smriti](https://aravindh97.github.io/smriti/)**: chat as a user and watch facts get added, replaced or denied; ask what the agent would recall now or on a past date; try to read another team's memories; run the eval against raw chat history. The Go code runs in your browser as WebAssembly; see [Live demo](#live-demo) for what differs from the full version.

Long-term memory for LLM agents, in Go, on PostgreSQL + pgvector and Temporal.

Smriti remembers what users say about themselves, their team and their organisation across sessions. When a fact changes, the new value replaces the old one instead of piling up next to it. The old value is kept as history, with its source message, so an agent can answer two kinds of question: "what is true now" and "what did we believe in February".

## Design

**Scopes and access**

| Scope | Owner | Who can read | Who can write |
|---|---|---|---|
| `user` | a user | that user | that user |
| `team` | a team | team members | team members |
| `org` | the organisation | everyone in the org | principals with `OrgWriter` only |

- Every read runs through one SQL access predicate (`store.visible`).
- The principal (org, user, teams) is set when the process starts, by the application that launches it. MCP tools never take a user or team id. A model that passes `user_id` gets an error (`DisallowUnknownFields`).
- Org writes need explicit permission. Without it, one user, or a prompt injected into their chat, could change what every agent in the company believes.

**Write path: extract, reconcile, store**
1. An extractor proposes candidate facts: scope, subject key, content, confidence and the source message. `extract.LLM` calls any OpenAI-compatible chat API. Its output is treated as untrusted:
   - unknown scopes are dropped
   - facts citing assistant messages are dropped
   - empty subjects and content over 500 characters are dropped
   - confidence and TTL are clamped
   
   `extract.Rules` is a deterministic extractor for tests and the eval.
2. `memory.Policy.Decide` reconciles each candidate against the owner's current memories:
   - **confirm**: same subject and same value. The confirmation count goes up.
   - **supersede**: same subject, new value. The old row is closed (`valid_to`, `superseded_by`) and a new row is inserted.
   - **ignore**: low confidence, or a statement *older* than the current value (out-of-order replay).
   - **add**: anything else. A different subject with near-identical content counts as a confirm.
   
   Whether a value changed is decided by token containment, not embedding similarity. "Prefers Tamil for voice calls" and "Prefers English for voice calls" embed almost identically but are different values. "Manager is Arjun Mehta now" is the same value as "Manager is Arjun Mehta". `TestDecide` covers both.
3. Each candidate is written in one transaction:
   - An advisory lock on (org, scope, owner) serialises writers for the same owner.
   - A partial unique index guarantees at most one current value per subject. `TestConcurrentWritersLeaveOneCurrentValue` runs 20 concurrent writers on the same subject.
   - A `memory_writes` table keyed on (source message, subject) makes replaying a conversation a no-op.

**Bitemporal rows.** `valid_from` / `valid_to` record when the fact was true, taken from the message timestamp. `recorded_at` records when Smriti learned it. `Recall(..., AsOf: t)` answers as of any past time.

**Recall.** pgvector HNSW finds candidates within the visible scopes. They are re-ranked by `0.75·similarity + 0.15·recency + 0.10·confirmations`, where recency has a 30-day half-life since the memory was last confirmed and confirmations saturate at 10.

**Temporal**
- `RememberConversation` runs extract, then write. LLM timeouts are retried, and writes are idempotent.
- `Maintain` expires TTL memories (for example "I'm travelling until Friday") and merges near-duplicates stored under different subjects. `smriti schedule` registers it as a nightly Temporal schedule.

**Forget.** `forget` hard-deletes every version of one of the user's own memories.

## Evaluation

`smriti eval` replays [`testdata/eval/scenario.json`](testdata/eval/scenario.json) into a fresh org namespace. The scenario has 6 users, 4 teams and 17 sessions between January and June 2026. Facts change across the sessions: managers, regions, team tools, review days, the payroll date. There is also a user who tries to overwrite an org fact without permission.

The eval then asks 34 questions:
- 29 about current values, including facts shared through a team or the org
- 2 leakage probes: a user asking about another team's tools, or another user's manager
- 3 "as of" questions about past values

The baseline is what an agent without a memory layer does: retrieve the most similar message from the user's own chat history, using the same embedder.

```
writes: add:25 confirm:1 denied:1 supersede:9   replay of all sessions changed 0 rows

system                   correct   stale  leaks   tokens/query
smriti (top-1 memory)      1.000   0.000      0           30.5
raw history (top-1)        0.676   0.176      -           65.9
```

Per-question output: [`docs/eval.txt`](docs/eval.txt).

**What this shows.** The baseline's errors are mostly stale answers: it returns "My manager is Priya" after the manager changed. It also misses team and org facts, because they were said by someone else. Smriti returns the current value, keeps team and org knowledge shared, rejects the unauthorised org write, leaks nothing across users or teams, and replaying every session changes nothing.

**What it does not show.** The eval uses the rule-based extractor on a scenario written in the phrasing those rules recognise. It tests reconciliation, temporal validity, access control and ranking. It does not test extraction quality. Measuring extraction needs a run with `SMRITI_LLM_URL` set and a larger, more varied conversation set, which is the next step. The baseline is also deliberately simple.

## Live demo

The page at https://aravindh97.github.io/smriti/ is this repo's Go code compiled to WebAssembly (`cmd/web`), with a plain HTML/JS frontend in `web/`. `.github/workflows/pages.yml` builds and publishes it on every push. GitHub Pages only serves static files, so anything that needs a database or a server runs against in-memory versions in the browser; the numbers in this README come from the full version.

```bash
./web/build.sh dist && python3 -m http.server -d dist 8000   # http://127.0.0.1:8000
```

## Run it

```bash
cp .env.example .env && docker compose up -d     # pgvector + Temporal dev server
set -a; . ./.env; set +a
go run ./cmd/smriti worker &
go run ./cmd/smriti remember --team design conversation.json
go run ./cmd/smriti recall who is my manager
go run ./cmd/smriti recall --as-of 2026-02-15T00:00:00Z who is my manager
go run ./cmd/smriti history manager
go run ./cmd/smriti schedule                      # nightly Maintain
go run ./cmd/smriti eval -v
```

`conversation.json`: `{"id":"c1","messages":[{"id":"0","role":"user","at":"2026-06-01T10:00:00Z","text":"My manager is Arjun."}]}`

**MCP** (`smriti mcp`) exposes `remember`, `recall`, `history` and `forget`, for the principal configured in the environment.

## Tests

```bash
go test -race ./...                                     # reconcile, extract, Temporal workflows
go test -race -tags integration ./internal/store        # needs DATABASE_URL (pgvector)
```

The integration tests cover:
- concurrent writers on one subject
- scope isolation (user, team, another org) and denied writes
- supersede, as-of recall and history
- ignoring late older statements and replay idempotency
- forget, TTL expiry and consolidation
- the MCP server, including rejection of a `user_id` argument

## Limits

- The bundled embedder is feature hashing, used so tests and evals run offline. For real use, plug a multilingual embedding model into the `embed.Embedder` interface.
- Team membership is taken from the principal. Smriti does not sync it from an identity provider.
- Consolidation compares pairs within one owner. That is fine for thousands of memories per owner. Beyond that it needs batching by subject cluster.
