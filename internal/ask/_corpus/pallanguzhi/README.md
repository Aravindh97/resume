# pallanguzhi

**Live demo: [aravindh97.github.io/pallanguzhi](https://aravindh97.github.io/pallanguzhi/)**: play Pallanguzhi against the Go AI and see every move it considered. The rules and AI run in your browser as Go WebAssembly, the same code as the terminal version.

[![pallanguzhi running in the browser](docs/screenshot.png)](https://aravindh97.github.io/pallanguzhi/)

*A personal project, built in my own time to learn. Not affiliated with or endorsed by my employer.*

Pallanguzhi (பல்லாங்குழி), the fourteen-pit Tamil mancala game, with a rules engine and a search-based AI opponent in Go. No dependencies.

## Play it

```
go run ./cmd/pallanguzhi play -level hard      # in a terminal
go run ./cmd/pallanguzhi bench -games 200      # the benchmark below
```

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

- *Why count positions instead of time for the hard level?* So play is the same on every machine and in tests. A time limit would make the AI weaker on a slow phone and the results irreproducible.
- *How do you know alpha-beta is correct?* It is compared with the unpruned search in 1,200 random positions at depths 1 to 4.
- *How is the parallel benchmark deterministic?* Each game gets its own PCG random source seeded by (pairing, game number). Workers only add results together, and addition doesn't depend on order.
- *What would make the AI stronger?* A transposition table (relays often reach the same position by different moves), a learned scoring function, or Monte Carlo tree search. Each would need to beat the current hard level by seat in the benchmark before I'd adopt it.

## Limits

- Single round only. The multi-round game with rubbish pits isn't implemented.
- The rules follow one published description; families play variants (for example, 5 seeds per pit, or different capture rules).
- "Expected margin" is only as good as the search depth. It isn't a solved value.
