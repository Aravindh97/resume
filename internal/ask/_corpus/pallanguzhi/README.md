# pallanguzhi

**Live demo: [aravindh97.github.io/pallanguzhi](https://aravindh97.github.io/pallanguzhi/)**: play against the Go AI (it runs in your tab as WebAssembly), or create a room and play a friend online through the Go game server.

[![pallanguzhi running in the browser](docs/screenshot.png)](https://aravindh97.github.io/pallanguzhi/)

*A personal project, built in my own time to learn. Not affiliated with or endorsed by my employer.*

Pallanguzhi (பல்லாங்குழி), the fourteen-pit Tamil mancala game, full stack:
- a Go rules engine and search AI (no dependencies);
- a Go game server for online play over WebSockets: rooms, spectators, reconnecting, rematches;
- a React + TypeScript frontend, tested with Vitest and with Playwright driving real browsers against the real server.

## Play it

```
go run ./cmd/pallanguzhi play -level hard      # in a terminal
go run ./cmd/pallanguzhi bench -games 200      # the AI benchmark below
./web/build.sh dist && STATIC_DIR=dist go run ./cmd/server   # online play at http://127.0.0.1:8080
go run ./cmd/loadtest -rooms 1000              # the load test below
```

## Online play

![Ravi's view of an online game: his pits along the bottom](docs/online.png)

Create a room, send the link, and play on two devices. Anyone else who opens the link watches.

**The server owns the board** ([`internal/room`](internal/room/room.go)). A browser only asks to lift a pit, with the version of the position it was looking at. The room refuses the move unless:
- it is that player's turn and the pit is legal;
- the version matches the current position. A double-click, a second tab, or a move made from a stale view is refused instead of being played twice.

**Seats survive disconnects.** Joining gives the browser a random seat token, kept in `localStorage`. Reloading the page or losing the network reconnects with backoff (0.5 s doubling to 8 s) and takes the same seat back. The other player sees "disconnected; their seat is kept for them". A guessed token gets a spectator view, not the seat.

**Slow connections never stall a room.** Each connection holds only the newest state it hasn't sent yet. A client that falls behind skips the positions it missed and gets the current one; nothing queues up, and nobody is disconnected for being slow. (The first version queued 16 states per client and disconnected anyone who fell behind. The load test caught it: about 1 game in 200 lost a player at 800 connections.)

**Limits,** because it is public and anonymous: WebSockets and API calls must come from an allowed origin; rooms created per IP per minute, connections per IP, messages per second and message size are capped; rooms are deleted 15 minutes after everyone leaves, and after 6 hours in any case. No accounts and nothing about players is stored.

### Tests

| What | How | Result |
|---|---|---|
| Rules of a room | unit tests: turns, spectators, double-submit, reconnect, rematch, expiry, slow clients | pass under `-race` |
| Concurrency | 8 tabs per player sending moves (many stale or illegal) while spectators join and leave, for 3 s; then every applied move is replayed on a fresh board | the replay matches the room's board in every run; every move was legal and in turn |
| Over real WebSockets | two clients play a whole game (moves chosen by the AI) while a spectator watches; plus origin checks, rate limits, oversized messages, unknown rooms | pass |
| In real browsers | Playwright: two players and a spectator on separate browser contexts, six moves, all three views agree with all 148 seeds; a reload takes the seat back | pass, also against the Docker image |
| Load | `cmd/loadtest`: N games at once, each with 2 players and 2 spectators, random legal moves | below |

From [`docs/loadtest.txt`](docs/loadtest.txt), server and every client on one 8-core machine over loopback (so the clients compete with the server for CPU):

| Games at once | Connections | Moves | Failed games | Move round trip p50 / p99 |
|---|---|---|---|---|
| 100 | 400 | 1,801 | 0 | 0.8 ms / 65 ms |
| 200 | 800 | 3,476 | 0 | 2.6 ms / 96 ms |
| 1,000 | 4,000 | 17,792 | 0 | 14 ms / 722 ms |

The round trip is from a player sending a move to receiving the new position. The free hosting plan has a fraction of one CPU, so expect fewer games there; the numbers show the design holds up, not hosted capacity.

### Deploy (free)

The [`Dockerfile`](Dockerfile) builds the server and the React page into one 12.5 MB image. [`render.yaml`](render.yaml) deploys it on Render's free plan: Dashboard → New → Blueprint → this repo. Then set the repository variable `SERVER_URL` (Settings → Secrets and variables → Actions → Variables) to the Render URL and rerun the `pages` workflow, so the GitHub Pages copy of the game can connect too.

The free plan sleeps after 15 minutes without traffic, so the first visit can take up to a minute (the page says so). Rooms live in memory, so a restart or a sleep ends any open games.

## Rules

Following the [Wikipedia description](https://en.wikipedia.org/wiki/Pallanguzhi); village rules vary.

- **Board:** 2 rows of 7 pits. Each pit starts with 12 seeds, except the middle pit of each row, which starts with 2 (148 seeds in all).
- **Sowing:** lift every seed from one of your pits and sow them one per pit, counter-clockwise.
- **Relay:** after the last seed, if the next pit has seeds, lift them and keep sowing.
- **Capture:** if the next pit is empty, capture the pit after it, and your turn ends.
- **Pasu:** any pit you bring to exactly 6 seeds is captured at once.
- **End:** the round ends when the player to move has no seeds; each side keeps the seeds on its row.

This plays a single round. The traditional multi-round game with "rubbish" pits is not implemented.

## Results

From `pallanguzhi bench -games 200` ([`docs/bench.txt`](docs/bench.txt)): 2,800 games, run in parallel on 8 cores in 8 minutes. Each game has its own seed, so the numbers are reproducible.

| A vs B | A wins as first player | A wins as second player |
|---|---|---|
| random vs random | 26.0% | 70.5% |
| medium vs random | 62.5% | **100%** |
| hard vs random | 65.0% | **100%** |
| hard vs easy | 24.5% | **100%** |
| hard vs medium | 44.5% | 60.5% |

**What these results show:**
- **The second player has a large advantage.** A depth-12 search of the start position says every first move loses by about 24 seeds if both sides play the moves the search sees. I didn't expect this; it is why every comparison is reported by seat.
- **As second player, search wins every game** against random and easy players.
- **As first player, the AI is playing a lost position.** It wins only when the opponent makes mistakes. Easy plays well enough most of the time (it looks one move ahead and blunders 35% of the time), so the AI beats it in only 24.5% of games from that seat.
- **Hard is only a little stronger than medium:** 52.5% of games over both seats, average margin +7.6 seeds. Searching deeper helps less here than I expected, because one relay can move most of the board.

Other checks:
- `TestPruningMatchesPlainSearch`: alpha-beta pruning and move ordering give the same value as a plain search in 1,200 positions, while searching fewer nodes.
- `TestRandomGames`: 5,000 random games. All of them end, the board always holds exactly 148 seeds, and no turn reaches the safety limit of 2,000 relays (the longest relay seen was 32).
- Hand-worked positions test each rule: capture, pasu, relay, two empty pits, and wrapping around the board.

## Frontend

The page is **React 19 + TypeScript**, built with Vite, in [`web/`](web/). It talks to the Go code in [`cmd/web`](cmd/web/), compiled to WebAssembly, through the same JSON API a server would offer: `api("POST", "path", body)` in [`web/src/lib/go.ts`](web/src/lib/go.ts) is answered by the Go handler registered for that route.

Types for the domain structs the page uses are **generated from the Go code** (`go run ./cmd/tsgen` writes `web/src/types.gen.ts`), and CI fails if they drift. The small response wrappers built in `cmd/web` are typed by hand.

```
./web/build.sh dist        # Go to WebAssembly, then the React build, into dist/
cd web && npm run dev      # dev server with hot reload (run build.sh once first for app.wasm)
cd web && npm test         # component tests (Vitest + Testing Library, Go replaced by a fake)
```

## How the AI works

- **Search:** negamax with alpha-beta pruning ([`internal/ai`](internal/ai/ai.go)). Moves that capture the most right away are tried first, so pruning cuts more of the tree.
- **Scoring:** the score a position is given is the final margin if the round ended now (store plus seeds on your row). It is exact at the end of the game. I also tried a store-only score and two mixed scores at depth 4. None was clearly better over 40 games each, so I kept the simplest.
- **Levels:**
  - **easy:** looks 1 move ahead and plays a random move 35% of the time.
  - **medium:** looks 4 moves ahead.
  - **hard:** deepens one move at a time until it has searched 300,000 positions, usually 14 to 30 moves deep. It counts positions instead of time, so it plays the same move on every machine. That takes about 60 ms per move natively and about 0.4 s in the browser.
- **Explanations:** the web page shows every move the AI considered, what each captures immediately, and the margin it expects.

## Design decisions

- **Why WebSockets rather than polling?** Moves need to reach the other player within a fraction of a second, and both sides send. Server-Sent Events would cover server-to-browser, but moves would then need a second channel.
- **Why a version number on every move?** It turns "is this click about the board I think it is?" into one integer comparison under the room's lock. That rejects double-clicks, second tabs and stale views without any per-client bookkeeping.
- **Why send the newest state instead of every state?** The client only needs the current position plus the last move to animate. Queueing every update per client lets one slow phone use unbounded memory or forces you to disconnect it.
- *Why count positions instead of time for the hard level?* So play is the same on every machine and in tests. A time limit would make the AI weaker on a slow phone and the results irreproducible.
- *How do you know alpha-beta is correct?* It is compared with the unpruned search in 1,200 random positions at depths 1 to 4.
- *How is the parallel benchmark deterministic?* Each game gets its own PCG random source seeded by (pairing, game number). Workers only add results together, and addition doesn't depend on order.
- *What would make the AI stronger?* A transposition table (relays often reach the same position by different moves), a learned scoring function, or Monte Carlo tree search. Each would need to beat the current hard level by seat in the benchmark before I'd adopt it.

## Limits

- Online rooms are in memory on one server: a restart ends open games, and it would need shared state (for example Redis) to run more than one instance.

- Single round only. The multi-round game with rubbish pits isn't implemented.
- The rules follow one published description; families play variants (for example, 5 seeds per pit, or different capture rules).
- "Expected margin" is only as good as the search depth. It isn't a solved value.
