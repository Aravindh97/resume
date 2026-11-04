# Kurippu

**Live demo: [kurippu.onrender.com](https://kurippu.onrender.com/)**: make a notebook, open its share link in a second browser or on your phone, switch one to "work offline", edit the same note on both, and switch back. The free server sleeps when idle, so the first visit can take up to a minute.

[![A notebook with three notes](docs/notebook.png)](https://kurippu.onrender.com/)

*A personal project, built in my own time to learn. Not affiliated with or endorsed by my employer.*

Shared notes that work offline and merge without losing anyone's edits. Kurippu (குறிப்பு) is Tamil for note.

Full stack:
- **One Go CRDT library,** a text sequence (RGA) plus last-writer-wins titles. It runs on the server and, compiled to WebAssembly, in the browser, so there is exactly one implementation of how edits merge.
- **A Go sync server on Postgres:** an ordered log of operations per notebook, idempotent uploads, paged downloads, and live "something changed" notices through `LISTEN/NOTIFY` and Server-Sent Events.
- **React 19 + TypeScript,** with every change saved to IndexedDB before it is sent, a service worker so the app opens with no network, and the cursor kept in place when other people's edits arrive.

![On a phone with no network: the edit is saved on the device and waiting to sync](docs/phone-offline.png)

## How it works

**Every character has an id.** A note body is a list of characters, each with a unique id: a Lamport clock value and the device's id. Typing becomes an insert ("these characters go after the character with id X"); deleting becomes a set of ids marked removed. Concurrent inserts at the same spot are ordered by id, so every device puts them in the same order. Nothing is ever overwritten: two people typing in the same sentence at the same time both keep their words. Titles and "moved to trash" are last-writer-wins, ordered by the same ids. ([`internal/crdt`](internal/crdt/crdt.go))

**Ops wait for what they depend on.** An insert that refers to a character this device hasn't seen yet is held back until it arrives, and each device's ops apply in its own order. Ops can therefore arrive in any order, more than once, from the server or another tab.

**Saved on the device first.** An edit becomes ops in the WebAssembly engine, which are written to IndexedDB in the same transaction as the device's op counter. A closed tab, a crash or a dead battery loses nothing, and a reopened notebook never reuses an op number. Sync happens afterwards: pending ops go up in batches, and whatever is new comes back.

**The server's log has no gaps a reader can fall into.** Each notebook has its own counter. Storing ops takes that notebook's row lock, numbers them, and commits, so numbers become visible in order. A device that has seen everything up to n asks for "after n" and can't skip an op that commits late with a smaller number. A single global id column doesn't give that guarantee: two transactions can take 7 and 8 and commit 8 first. A retried upload, after a lost response, stores nothing twice.

**Sharing is by link.** A notebook's key is 128 random bits in the link's `#fragment`. Browsers never send the fragment to servers. The app sends the key in an `X-Space` header, never in a URL, so it stays out of server and proxy logs, and the database keeps only its SHA-256. Anyone with the link can edit, like a shared document link. There are no accounts.

**Offline app shell.** The service worker caches the page, the JavaScript and the WebAssembly engine, and never caches the API. With no network the app still opens and shows every note from IndexedDB.

## Tests

| What | How | Result |
|---|---|---|
| Merging | 1,500 random sessions: 3 to 5 devices edit, retitle, trash and create notes in rounds; each round every device gets a random half of the others' ops, shuffled, some twice; then everything | every device ends identical in every session, with nothing left waiting |
| Merging, specific cases | concurrent edits in one sentence; typing at the same spot; ops delivered backwards, twice; titles; cursor anchors; malformed and forged ops | pass |
| Server, against Postgres | three devices edit offline and sync in random order through the server; retried uploads; 20 writers while a reader follows its cursor; notebook isolation; validation and limits; live notices; paged sync of 2,300 ops | the reader saw all 301 ops exactly once; nothing typed was lost or duplicated |
| Sync loop | Vitest with a fake IndexedDB: offline edits kept and sent once, in order; a reopened notebook continues its numbering; a server error retried without losing the change | pass |
| Real browsers | Playwright against the real server: two browsers offline edit the same note and both edits survive; two people type at the same spot and both sentences survive; the app opens and edits with no network at all, then syncs; axe finds no serious accessibility problems | pass, 3 runs in a row |

Speed, from [`docs/bench.txt`](docs/bench.txt): a keystroke in the middle of a note takes 0.12 ms at 1,000 characters and 1.2 ms at 10,000. An edit in one browser appears in another in about 370 ms on loopback, most of which is the 300 ms the app waits to batch a typing burst into one request.

## Run it

```
docker run -d --name kurippu-pg -e POSTGRES_PASSWORD=dev -p 127.0.0.1:5432:5432 postgres:16-alpine
./web/build.sh                      # Go -> WebAssembly, then the React build, into dist/
DATABASE_URL=postgres://postgres:dev@127.0.0.1:5432/postgres?sslmode=disable STATIC_DIR=dist go run ./cmd/kurippu
# http://127.0.0.1:8080

KURIPPU_TEST_DATABASE_URL=... go test -race -p 1 ./...
cd web && npm test && KURIPPU_E2E_DATABASE_URL=... npm run e2e
```

## Deploy (free)

Create a free Postgres on [Neon](https://neon.com), then on [Render](https://render.com) choose New → Blueprint → this repo. [`render.yaml`](render.yaml) builds the [`Dockerfile`](Dockerfile); paste the Neon string as `DATABASE_URL`.

## Design decisions

- **Why a CRDT rather than "last save wins" or locking?** People edit offline for hours. Last save wins throws away someone's work. Locking can't work without a network. Operational transformation needs a central server to order every edit.
- **Why compile the same Go code to WebAssembly?** The merge rules are the part that must never differ between devices. With one implementation they can't drift, and the 1,500-session test covers the code the browser runs.
- **Why character ids rather than offsets?** An offset means something different after someone else's edit. An id always names the same character, which is also how the cursor stays put when remote edits arrive.
- **Why fetch for the event stream, not EventSource?** EventSource can't send headers, and the notebook key must not go in a URL.

## Limits

- Deleted characters stay as tombstones, so a note that is rewritten many times grows. Real use would compact history once every device has synced past it.
- The engine is 3.4 MB of WebAssembly (950 KB gzipped), loaded once and then cached. TinyGo would make it much smaller.
- Anyone with a notebook's link can read and edit it. There are no accounts, per-person permissions or end-to-end encryption.
- Plain text only, no rich text or images.
