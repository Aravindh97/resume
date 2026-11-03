# Granth

**Live demo: [aravindh97.github.io/granth](https://aravindh97.github.io/granth/)**: ask the test corpus questions and see each answer's file, page, section and rank trace, ingest your own Markdown, HTML, CSV or PDF, watch broken files get classified, run the 44-question eval, and call the MCP search tool. The Go code runs in your browser as WebAssembly; see [Live demo](#live-demo) for what differs from the full version.

[![granth running in the browser](docs/screenshot.png)](https://aravindh97.github.io/granth/)

*A personal project, built in my own time to learn. Not affiliated with or endorsed by my employer.*

Document ingestion and hybrid retrieval for AI agents, in Go, built on Temporal, OpenSearch and PostgreSQL.

Granth takes a folder of messy enterprise files (Markdown, HTML, text, CSV, text PDFs and scanned PDFs), runs each one through a retry-safe Temporal workflow, and indexes layout-aware chunks into OpenSearch. Agents call it through MCP tools that return passages with citations: source file, page, section path and block range.

```
            ┌──────────── Temporal workflow: IngestDocument (one per file) ────────────┐
 file/URL → │ Fetch → AlreadyIndexed? → Parse (+OCR) → Chunk+Embed → Index → Ledger      │
            └───────┬──────────────────────┬──────────────────┬────────────┬──────────────┘
            blob store (by sha256)   PermanentError → dead letter   OpenSearch   Postgres ledger
                                                                 │
 agent ── MCP stdio ── search / get_chunk / open_source ── hybrid search (BM25 + k-NN, RRF)
```

## What it does

**Ingestion that is safe to retry**
- One Temporal workflow per document. `IngestBatch` runs them as child workflows with a concurrency limit. The child workflow id comes from the document id, so two batches can't ingest the same file at the same time.
- Every activity is idempotent:
  - Activities pass sha256 blob references instead of file bodies, which keeps Temporal history small.
  - Chunk ids are `sha256(doc id, content hash, ordinal)`, so a replay upserts the same ids.
  - `ReplaceDoc` upserts the new chunks and then deletes chunks from older versions.
  - Re-ingesting unchanged content is skipped. `TestIngestDocumentHappyPathAndReplaySkips` covers this.
- Errors are classified:
  - Unsupported format, corrupt PDF, invalid UTF-8, files over 100 MB and HTTP 404/403 are `PermanentError`. They are not retried and go straight to the dead-letter state in the Postgres ledger, with the failing step and error. Use `granth failed` to see them and `granth retry` to re-run them.
  - Everything else is retried with exponential backoff (1 s up to 1 min) for up to an hour per step.
- Embedding results are cached by `hash(model, batch texts)`. When the embed step is retried, or a document is re-ingested, unchanged batches don't call the embedding API again. `TestEmbeddingCacheAvoidsRepeatCalls` covers this.
- `Reindex` (for example after changing the embedding model) pages through the ledger and uses continue-as-new after each page, so it resumes from its cursor after a crash.
- External calls to the embedding API and OCR have timeouts, jittered retries and a circuit breaker.

**Parsing**
- Markdown, HTML and text keep headings, paragraphs and tables. HTML drops nav, footer and script content.
- Tables become row/column data. The indexed text is `header: value` pairs, so "hotel limit in Coimbatore" matches the right row.
- PDFs are read page by page. A page with no text layer goes to OCR (`pdftoppm` + `tesseract`, using `eng+hin+tam` in the worker image).
- A panic in the PDF library becomes `ErrCorrupt`, so one bad file can't crash the worker.

**Chunking.** A chunk never crosses a heading or a page boundary. Each chunk's text starts with its heading path. A long table is split by rows, and every piece repeats the header row.

**Retrieval**
- Search runs BM25 and k-NN in parallel and fuses them with reciprocal rank fusion (k=60).
- OpenSearch uses a multi-field mapping: standard, English and Hindi analyzers, plus a Lucene HNSW vector with filtered k-NN.
- Filters: document ids, metadata and tables only.
- If the embedder is down, hybrid search falls back to BM25 instead of failing.
- Every result carries a trace (`lexical_rank`, `vector_rank`, `fused_score`), so you can tell whether a missing answer was never retrieved or was retrieved and outranked.
- A token budget (`max_tokens`) caps the size of results, so agents can call search often without filling their context.

**MCP tools**
| Tool | What it returns |
|---|---|
| `search` | top passages + citations + rank trace |
| `get_chunk` | one passage by id |
| `open_source` | neighbouring passages, addressed directly through deterministic chunk ids (no extra search) |

## Evaluation

`granth eval` indexes `testdata/corpus` and runs 44 labelled questions in each mode. A question counts as correct at rank *r* when the chunk at *r* comes from the expected file **and** contains the evidence text. Labels are tied to text, not chunk ids, so they stay valid when chunking changes.

The corpus is 9 documents for a fictional textile company, in Markdown, HTML, CSV, text, PDF, Hindi and Tamil, plus 3 deliberately broken files. Results from this repo with the offline hashing embedder:

| backend | mode | hit@1 | hit@5 | MRR@10 | citation@1 | p50 |
|---|---|---|---|---|---|---|
| in-memory | lexical | 0.909 | 0.977 | 0.943 | 0.955 | 56 µs |
| in-memory | vector | 0.886 | 0.955 | 0.917 | 0.909 | 28 µs |
| in-memory | **hybrid** | **0.909** | 0.977 | **0.942** | 0.955 | 69 µs |
| OpenSearch 2.17 | lexical | 0.841 | 0.955 | 0.891 | 0.864 | 5.6 ms |
| OpenSearch 2.17 | vector | 0.886 | 0.955 | 0.917 | 0.909 | 5.7 ms |
| OpenSearch 2.17 | **hybrid** | **0.886** | **0.977** | **0.925** | **0.909** | 5.4 ms |

Full output with a trace for every miss: [`docs/eval-memory.txt`](docs/eval-memory.txt), [`docs/eval-opensearch.txt`](docs/eval-opensearch.txt).

**Where it goes wrong, and why:**
- **q44** ("what should dyeing unit workers wear") misses in every mode. The answer is only in the Tamil document. BM25 can't match across languages, and the hashing embedder isn't semantic. Fixing it needs a multilingual embedding model (set `EMBED_URL` to one, such as multilingual-e5).
- **q16** ("rotate my password every 90 days?") misses in OpenSearch lexical mode, because the answer says "not on a fixed schedule" and shares almost no words with the question. Hybrid recovers it.
- **q17** ("Which systems need MFA?") misses in vector mode, because "MFA" never appears in the text. BM25 finds it through "authentication". Hybrid recovers it.

**A bug the live demo found:** when two results have the same fused score (rank 1 + 2 and rank 2 + 1 fuse to the same value), the original code broke the tie by chunk id. Chunk ids are hashes that include the file path, so the in-memory hybrid hit@1 depended on which directory the repo was checked out in (0.932 here, 0.886 in the browser). Ties now go to the better single-retriever rank, then source, page and position, with a test that two different paths give the same order. The OpenSearch numbers didn't change; the in-memory hybrid row above is the corrected one.

**How to read these numbers:** the corpus is small, so absolute scores are high. The harness is for comparing changes on the same question set: chunking, analyzers, fusion constants or a real embedding model. Add questions to `testdata/eval/questions.jsonl` as you find failures.

## Failure handling, tested against real services

These came from a run against the local Docker stack:

- **Bad files.** Ingesting `testdata/corpus` and `testdata/bad` together indexed all 9 good documents. The 3 bad files went to the dead-letter state at the `parse` step after a single attempt each.
- **Re-ingest.** Running the same batch again returned `skipped` for every document.
- **OpenSearch outage.**
  1. Stopped OpenSearch with `docker stop`, then started an ingest.
  2. The `IndexChunks` activity kept retrying with backoff. It had reached attempt 7 after 60 s.
  3. After `docker start`, the workflow finished with `indexed` and the document became searchable.
  4. No duplicate chunks were created.

## Live demo

The page at https://aravindh97.github.io/granth/ is this repo's Go code compiled to WebAssembly (`cmd/web`), with a plain HTML/JS frontend in `web/`. `.github/workflows/pages.yml` builds and publishes it on every push. GitHub Pages only serves static files, so anything that needs a database or a server runs against in-memory versions in the browser; the numbers in this README come from the full version.

```bash
./web/build.sh dist && python3 -m http.server -d dist 8000   # http://127.0.0.1:8000
```

## Run it

Requires Go 1.26 and Docker.

```bash
cp .env.example .env            # set POSTGRES_PASSWORD and OPENSEARCH_PASSWORD
docker compose up -d            # Postgres, OpenSearch (security plugin on), Temporal dev server
set -a; . ./.env; set +a

go run ./cmd/granth worker &                     # Temporal worker
go run ./cmd/granth ingest testdata/corpus testdata/bad
go run ./cmd/granth failed                        # dead-letter list
go run ./cmd/granth search "hotel limit in Coimbatore"
go run ./cmd/granth eval -v                       # retrieval eval with miss traces
```

To run without any services: `OCR=none go run ./cmd/granth eval` uses in-memory backends.

**Use from an MCP client** (Claude Desktop, Kiro, etc.):

```json
{ "mcpServers": { "granth": { "command": "granth", "args": ["mcp"],
  "env": { "GRANTH_OPENSEARCH_URL": "https://127.0.0.1:9200", "OPENSEARCH_USER": "admin",
           "OPENSEARCH_PASSWORD": "…", "OPENSEARCH_CA_CERT": "/path/root-ca.pem" } } } }
```

## Tests

```bash
go test -race ./...                                   # unit + Temporal workflow tests (no services)
go test -tags integration ./internal/index ./internal/store   # against the compose stack
```

The workflow tests use Temporal's test environment and cover:
- the happy path, and a replay that skips
- transient embedding failures that are retried until they succeed
- permanent failures that go to the dead-letter state without retries
- a batch that keeps going past bad files
- a new version of a document replacing the old chunks
- the embedding cache

## Layout

```
cmd/granth            CLI: worker, ingest, search, mcp, failed, retry, reindex, eval
internal/pipeline     Temporal activities + workflows, in-process runner
internal/parse        Markdown/HTML/text/CSV/PDF parsers, OCR fallback
internal/chunk        layout-aware chunker
internal/index        Index interface; OpenSearch and in-memory BM25/cosine
internal/search       hybrid retrieval, RRF, traces, token budget
internal/embed        hashing embedder, OpenAI-compatible HTTP embedder
internal/resilience   retry with jitter, circuit breaker
internal/store        Postgres document ledger / dead letter
internal/mcp          MCP server (JSON-RPC over stdio)
internal/eval         retrieval eval harness
```

## Known limits

- The default embedder is a feature-hashing model, so `granth` runs offline. It isn't semantic. For real use, set `EMBED_URL` to a multilingual embedding model.
- PDF citations go down to page and block, not bounding box. The text-layer reader doesn't keep coordinates.
- There's no reranker yet. A cross-encoder pass over the top fused results would be the next quality step.
