# Sutra

Enterprise connectors, an ontology and entity resolution in Go and PostgreSQL, so agents can work with structured records from several systems as one consistent model.

Sutra streams an ERP's vendor table through Postgres logical replication (CDC) and syncs CRM accounts incrementally over a REST API. It maps both onto a YAML ontology and works out which records describe the same company, using Indian business identifiers (GSTIN with checksum, PAN, +91 phones) and transliteration-aware name matching. Every value in the resulting golden record can be traced to the source record it came from. Agents query it through read-only MCP tools.

```
 ERP Postgres ──logical replication (pgoutput)──┐
                                                 ├─► source_records ─► resolve ─► entities + lineage + review queue ─► MCP tools
 CRM REST API ──keyset incremental sync─────────┘        (Postgres, with connector checkpoints)
```

## Connectors

**Postgres CDC** (`internal/connector/pgcdc`)

The first run:
1. Creates a publication for the table.
2. Creates a replication slot with an **exported snapshot**.
3. Copies the table inside that exact snapshot. Changes after it arrive through the slot, so nothing is missed or counted twice.

Streaming:
- Changes are buffered per transaction. At COMMIT they are applied to the warehouse together with the commit LSN (WAL position) as the checkpoint, in **one** transaction.
- The LSN is acknowledged to the source only after that commit, so the source keeps WAL until Sutra has durably applied it.
- After a crash, transactions at or below the checkpoint are skipped.
- Every stored record carries the sequence number of the position that delivered it. An upsert never replaces a newer version with an older one.

**REST incremental sync** (`internal/connector/rest`)
- Keyset pagination on `(updated_at, id)` instead of offset pages. A plain "updated since" filter loses records that share a timestamp, and offsets skip or repeat records when data changes mid-sync. `TestRESTSyncRetriesAndResumes` uses records with shared timestamps.
- The checkpoint is written in the same transaction as its page.
- 429 responses honour `Retry-After`. 5xx responses and network errors back off with jitter. 401/403 stop immediately.
- `sutra fake-crm` is a demo API. It requires a bearer token, refuses to listen on anything but loopback, and can inject 429/503 responses.

**Tested against real services** (local Docker: Postgres 16 with `wal_level=logical`)
- **Snapshot and streaming.** 280 vendor rows were snapshotted. An update, a delete and an insert then streamed through and appeared in the warehouse; the delete was stored as a tombstone.
- **Crash recovery.**
  1. Ran 40 source transactions, each with an update, an insert and a delete.
  2. Killed the connector with `kill -9` after transaction 15.
  3. Restarted it. It applied the remaining 25 transactions.
  4. The md5 of every `(id, name, city)` in the source equals the md5 of the live warehouse rows.
- **CRM sync.** 223 accounts synced with 429/503 responses injected. A wrong token fails with `authentication rejected by source: HTTP 401`.

## Ontology

[`config/ontology.yaml`](config/ontology.yaml) declares:
- entity types and their attributes, each with a kind (`name`, `gstin`, `pan`, `phone`, `email`, `city`, `string`)
- each source's kind (`postgres_cdc` or `rest`)
- how each source's fields map onto attributes
- survivorship priority between sources

The file is validated when it loads. A mapping to an unknown attribute fails.

## Entity resolution

1. **Normalise.**
   - GSTIN is validated with its mod-36 checksum, and a value with a bad checksum is never used as a key. A wrongly keyed GSTIN would otherwise merge unrelated companies.
   - The PAN is derived from characters 3–12 of the GSTIN. One legal entity has one PAN but a separate GSTIN in each state, so PAN links branches.
   - Phones become +91 numbers.
   - Free-mail domains are ignored, because they say nothing about the company.
   - Names lose legal suffixes, including misspelt ones ("Privat eLimited", "Limitted"). Transliteration variants are folded: Shree/Sri, Lakshmi/Laxmi, Thirumalai/Tirumalai.
2. **Block.** Only records sharing a key are compared: GSTIN, PAN, phone, email domain, first name token plus city, or name prefix. For 544 records that was 5,205 pair comparisons instead of 147,696.
3. **Score** with explainable rules, from strongest to weakest:
   - same GSTIN
   - same PAN
   - same phone plus similar name
   - same company email domain plus similar name
   - near-identical name plus same city
   
   Name similarity is weighted Monge-Elkan over phonetic tokens, with generic words ("Textiles", "Knit", "Fabrics") weighted at half. That stops "Lakshmi Knit Fabrics" and "Selvam Knit Fabrics" from matching.
4. **Cluster.** The strongest pairs merge first, under a cannot-link rule: **a cluster can never hold two different PANs**.
5. **Review instead of guessing.**
   - Pairs scoring 0.65–0.85 go to `review_queue`.
   - When two different PANs trade under the same name in the same city, name-only matches for that name are capped at the review band.
6. **Golden record with lineage.** For each attribute, Sutra prefers a valid identifier, then the higher-priority source, then the latest update. `attribute_lineage` stores the source and record id behind every value. `match_evidence` stores why the members were linked.
7. **Stable ids.** A cluster reuses the entity id most of its members had in the last run, and the larger half keeps the id when a cluster splits. Ids that agents have stored keep working. `TestEntityIDStableWhenRecordsArrive` caught the first version of this getting it wrong.

## Evaluation

`sutra eval --heldout` runs on labelled synthetic ERP and CRM data from [`internal/synth`](internal/synth/synth.go). The generator plants the hard cases:
- transliteration variants, typos, and changes in letter case and legal suffix
- companies registered for GST in two states
- different companies with the same trading name
- duplicate vendor rows inside the ERP
- keying errors in GSTINs
- missing identifiers on the CRM side

The metric is pairwise precision, recall and F1. Full output: [`docs/eval.txt`](docs/eval.txt).

**Dev set** (seed 7, 503 records, 288 companies; the rules were tuned on this):

| method | precision | recall | F1 |
|---|---|---|---|
| exact name | 0.468 | 0.121 | 0.193 |
| GSTIN only | 1.000 | 0.523 | 0.687 |
| **sutra** (match ≥ 0.85) | **1.000** | **0.992** | **0.996** |

**Held-out seeds** (101–505, never used for tuning):

| companies | sutra F1 | GSTIN-only F1 | review pairs |
|---|---|---|---|
| 300 | 0.960 – 0.996 | 0.720 – 0.768 | 61 – 82 |
| 2,000 | 0.923 – 0.937 | 0.710 – 0.736 | 4,674 – 5,474 |

**What these numbers show.** At 2,000 companies, the generator's 240 possible names spread over 7 cities, so many unrelated companies share a name and city. Precision drops to about 0.89, and the review queue grows to thousands of pairs. Records with no identifiers can't be separated in that setting. The fix is more signals (addresses, bank account hashes) and cheaper review, not lower thresholds. The data is synthetic. Real ERP and CRM exports will have failure modes the generator doesn't model.

## Run it

```bash
cp .env.example .env && docker compose up -d   # warehouse Postgres + source Postgres (wal_level=logical)
set -a; . ./.env; set +a
go run ./cmd/sutra seed-source --reset          # demo ERP vendors table
go run ./cmd/sutra fake-crm --fail-every 3 &    # demo CRM API on 127.0.0.1:8089
go run ./cmd/sutra sync erp_vendors --once      # snapshot + stream until idle
go run ./cmd/sutra sync crm_accounts --once
go run ./cmd/sutra resolve
go run ./cmd/sutra find gstin 33AAACK1234K1Z… ; go run ./cmd/sutra get com_…
go run ./cmd/sutra eval -v --heldout            # no database needed
```

**MCP tools** (`sutra mcp`, read-only):

| Tool | What it does |
|---|---|
| `describe_ontology` | lists entity types, attributes and sources |
| `find_entities` | normalises identifiers before matching |
| `get_entity` | golden record, lineage, member records, match evidence |

## Tests

```bash
go test -race ./...                                       # normalisers, resolver
go test -race -tags integration ./internal/warehouse      # needs DATABASE_URL
```

## Limits

- One entity type per resolve run, and resolution is a batch job. Incremental resolution of only the changed blocks is the next step.
- Updates that leave a TOASTed column unchanged arrive without that column's value. Sutra keeps the other columns but doesn't refetch that one.
- The review queue has no UI. Decisions are stored in `review_queue.status` but don't feed back into scoring yet.
