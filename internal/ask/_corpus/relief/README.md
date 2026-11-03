# relief

**Live demo: [aravindh97.github.io/relief](https://aravindh97.github.io/relief/)**: cut the network between three devices, change stock and needs on both sides, reconnect and watch them merge; run the randomised partition test in your tab. The Go code runs in your browser as WebAssembly; see [Live demo](#live-demo) for what differs from the full version.

[![relief running in the browser](docs/screenshot.png)](https://aravindh97.github.io/relief/)

*A personal project, built in my own time to learn. Not affiliated with or endorsed by my employer.*

Offline-first data sync for disaster relief teams, in Go with no dependencies.

After a cyclone or flood, camps and supply trucks can go days without a network. Teams keep recording what came in, what was handed out, who needs what, and each camp's status. When devices meet again, over a local hotspot, a USB stick, or a phone carried between camps, their records have to merge **without losing anyone's changes**. The usual approach, "the newest copy of the sheet wins", silently drops the other side's work.

`relief` uses a CRDT (conflict-free replicated data type): a data structure designed so that any two copies can be merged and every device ends up with the same result, with no central server.

## Results

From `relief sim -runs 10000`, output in [`docs/sim-10000.txt`](docs/sim-10000.txt). Each randomised run has:
- 3–8 devices and 100–500 steps
- the network splitting into random groups and rejoining
- devices relaying other devices' changes
- 10% of batches delivered twice
- phone clocks off by up to ±5 minutes

| | relief (CRDT) | "newest sheet wins" baseline |
|---|---|---|
| all devices identical after the network heals | **10,000 / 10,000 runs** | n/a |
| broken rules (lost stock change, wrong open needs, wrong latest field) | **0** | data lost in **10,000 / 10,000** runs, **3.88 million** stock units wrong in total |
| data per sync | **10.7 ops**, using version vectors | 326.9 ops if each sync sent everything (relief moves **3.3%** of the bytes) |

The three rules are checked against an oracle built from every change ever made in the run, not by comparing devices with each other.

[`scripts/demo.sh`](scripts/demo.sh) is an end-to-end demo with three devices: [`docs/demo.txt`](docs/demo.txt).
1. Camp 2 and a truck sync, then lose the network.
2. Both hand out rice, and one closes an insulin request while the other opens a new one.
3. The truck carries a USB bundle to camp 4. Importing it twice is harmless, and a damaged copy is rejected.
4. When everyone has synced, camp 2 and the truck show 90 kg of rice, the new insulin request still open, and the later status.

## How it works

**Every change is an operation with a unique stamp `(device, seq)`.** A device's knowledge is a *version vector*: for each device, the highest `seq` it holds. Syncing works like this:

> "Here's my version vector." → "Here are the ops you don't have."

- Nothing is sent twice.
- A device can pass on ops it got from another device, so a truck carries changes between camps that never meet.
- Receiving a batch twice changes nothing.
- A gap in the sequence is refused, so a device never ends up with a hole in another device's history.

**The visible state comes from the set of ops using rules where order doesn't matter.** Any two devices with the same ops therefore show the same thing:

| Data | Rule | Why |
|---|---|---|
| Stock (`rice_kg`, `tarpaulin`, …) | **counter**: sum of all + and − changes | Camp A hands out 30 and camp B hands out 50 while both are offline; after the merge the total is down by 80, not by whichever was saved last |
| Needs ("insulin camp-2") | **add-wins set**: a need is open if any request exists that no "done" had seen | Closing an old request offline doesn't erase a new one someone made meanwhile |
| Fields (camp status, contact) | **last writer wins by hybrid logical clock (HLC)** | The latest edit wins even if a phone's clock is 5 minutes fast, because a device that has seen a timestamp always writes later ones |

**What a CRDT can't do:** stop two offline camps from handing out the same 10 tarpaulins. It can make sure nobody misses it: `relief show` prints `CHECK tarpaulin oversubscribed by 6`. Preventing this would need coordination, meaning a device that is allowed to hand out stock. That isn't possible while the network is down.

**On the device:**
- The replica is one JSON file. It's written to a temp file and then renamed, so a crash leaves either the old file or the new one, never half of each.
- A lock file stops two processes from writing at once.
- Bundles carry a sha256 checksum, so a truncated or damaged file is refused.

## Live demo

The page at https://aravindh97.github.io/relief/ is this repo's Go code compiled to WebAssembly (`cmd/web`), with a plain HTML/JS frontend in `web/`. `.github/workflows/pages.yml` builds and publishes it on every push. GitHub Pages only serves static files, so anything that needs a database or a server runs against in-memory versions in the browser; the numbers in this README come from the full version.

```bash
./web/build.sh dist && python3 -m http.server -d dist 8000   # http://127.0.0.1:8000
```

## Run it

```bash
go run ./cmd/relief sim -runs 10000
./scripts/demo.sh
go test -race ./...
```

```bash
relief init   -dir ./camp2 -device camp2-phone
relief stock  -dir ./camp2 rice_kg +200
relief need   -dir ./camp2 open "insulin camp-2"
relief set    -dir ./camp2 camp-2.status flooded
relief show   -dir ./camp2
relief vv     -dir ./camp4 > camp4.vv                  # what camp 4 already has
relief export -dir ./camp2 -for camp4.vv -o usb.bundle  # only what camp 4 lacks
relief import -dir ./camp4 usb.bundle
relief serve  -dir ./camp2 -addr 127.0.0.1:7700   ;   relief sync -dir ./truck http://127.0.0.1:7700
```

## Limits

- **Network sync is loopback-only and has no authentication.** Between devices, use bundles or an SSH/WireGuard tunnel. Live sync over a field network would need mutual TLS with a team certificate.
- **Bundles are checked for damage, not for forgery.** Signing every op with the device's key is the next step.
- **History is never compacted.** Every op is kept. That's fine for the tens of thousands of changes a relief operation makes, but very long deployments would need compaction once every device has seen an op.
- **Simulated network.** The 10,000 runs use an in-process network. The demo uses real processes and files.

## Design decisions

- **Why not last-writer-wins for everything?** Two camps change the same stock count offline and one change disappears. Counters add both changes instead.
- **Add-wins or remove-wins for needs?** Add-wins: a request for insulin must never vanish because someone closed an older one. The cost is that a need is sometimes closed twice.
- **Why a hybrid logical clock instead of wall time?** Phone clocks are wrong. With an HLC, once a device has seen a timestamp, its next timestamp is later, so an edit made after reading someone else's always wins.
- **How do you know it's correct?** 10,000 randomised runs, checked against an oracle built from every op ever made, plus targeted tests for each rule: concurrent close and add, a fast clock, relays, duplicates, gaps.
- **How is this different from Git?** Git merges text and asks a human about conflicts. A CRDT merges by rules chosen per field, so nobody is asked unless an invariant like non-negative stock breaks.
