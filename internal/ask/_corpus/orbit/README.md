# orbit

**Live demo: [aravindh97.github.io/orbit](https://aravindh97.github.io/orbit/)**: pick any satellite in the 16,763-object public catalogue, screen it for close approaches, see them on its ground track, and check the screener against brute force in your tab. The Go code runs in your browser as WebAssembly; see [Live demo](#live-demo) for what differs from the full version.

A satellite conjunction screener in Go: it finds when two objects in orbit will pass within a few kilometres of each other. You can screen your own satellites against the whole public catalogue, or screen every pair.

Commercial collision-warning services are expensive. Student and startup satellite teams usually rely on free alerts, which are noisy and come late. This is an open, fast screener that runs on a laptop from CelesTrak's public data.

## Results

The test machine is an 8-core desktop. The catalogue is 16,763 objects from CelesTrak's public groups (active satellites plus the Fengyun-1C, Cosmos 2251, Iridium 33 and Cosmos 1408 debris clouds), fetched on 2026-11-02 at 04:45 UTC.

| Check | Result |
|---|---|
| SGP4 (the standard model that predicts a satellite's position from its published orbit data) against the reference implementation, `python-sgp4` 2.27 | 7,999 positions on 1,999 debris objects at 0–72 hours: **max difference 1 mm** |
| Screener against an exhaustive brute force (every pair, every 10 s, refined) | **190 of 190** close-approach pairs found, every distance within 10 m, while checking **0.066%** of the pairs brute force checks |
| Full catalogue, all pairs, **6 days** at 10 s steps, 5 km threshold | **3 min 49 s** · 986 million SGP4 propagations (4.3 million per second) · **3.8 million object-steps screened per second** · pairs checked: 0.0115% of brute force |
| "My satellites" mode: 170 satellites against the full catalogue, 6 days, 25 km threshold | 3 min 42 s · pairs checked: 0.0007% of brute force |

Full output: [`docs/`](docs/).

### Comparison with SOCRATES

[SOCRATES](https://celestrak.org/SOCRATES/) is CelesTrak's daily conjunction screen. I compared against its top 1,000 encounters by miss distance (all under 255 m) whose time of closest approach (TCA) falls in the same 6-day window. The headline recall is low, for two reasons:

1. **Different catalogue.** In 440 of the 534 encounters, at least one object isn't in CelesTrak's public groups, mostly general debris and rocket bodies. SOCRATES uses the full catalogue. The screener can read one from [Space-Track](https://www.space-track.org) (free account) with `-tle`.
2. **Newer orbit data.** My orbit data (element sets, CelesTrak's TLEs) is about 20 hours newer than the set SOCRATES ran on. For every one of the remaining 94 encounters, at least one object's element set had changed in between. Prediction error from these element sets grows by roughly a kilometre per day, and Starlink satellites manoeuvre. A predicted 100 m pass often becomes a 20 km pass, or moves to the next orbit about 47 minutes later.

On those 94 encounters:
- **17 (18%)** were found within ±2 minutes of SOCRATES' TCA. For these, the median difference in miss distance was 0.12 km and the median TCA difference was 0.2 s.
- Near SOCRATES' TCA, my newer data predicts the same two objects within 25 km in 34 of 94 cases (median 60 km). Most of the rest are the same pair crossing again 47 or 94 minutes away.

That's a disagreement in the *orbit data*, not in the screening. The brute-force check above is the test of the screener itself. [`scripts/socrates-aligned.sh`](scripts/socrates-aligned.sh) reruns the comparison on **the same element sets**: fetch the catalogue during the 2 hours after SOCRATES' 08:05 UTC data cut, then compare once SOCRATES publishes about 13 hours later. I haven't run that aligned comparison yet.

## How it works

Comparing every pair at every step means 140 million pairs per step for this catalogue. The screener avoids that in three stages:

1. **Altitude shells.** It computes each object's perigee and apogee from its mean motion and eccentricity. Two objects whose altitude bands never overlap, with margin, can never meet.
2. **Spatial hash.** At each step, every object goes into a 3D grid cell of side `threshold + 1 km + 16 km/s × step`. That's the furthest apart two objects can be at a sample time and still come within the threshold during that step. Only objects in the same or neighbouring cells are compared.
3. **Straight-line prefilter, then exact refinement.** For each candidate pair it extrapolates the relative position and velocity to the closest approach in a straight line. Only if that comes within the threshold inside the step does it refine on the true SGP4 distance, using a golden-section search to 1 ms.

The time window is split into slices that run in parallel, about four per core to balance load. Each worker has its own copy of every satellite, because the SGP4 code keeps state on the satellite struct. Detections of the same encounter from neighbouring steps are merged. A pair that stays close for more than 10 minutes, such as docked modules or formation flyers, is reported once and marked **co-orbiting**, not reported thousands of times.

**SGP4.** The code is vendored from [go-satellite](https://github.com/joshuaferrara/go-satellite) (BSD-2). I fixed one bug: upstream truncated each TLE's epoch to whole seconds. Measured on 2,000 debris objects, that put positions a median 3.6 km and up to 7.5 km off. After the fix it matches the reference to 1 mm. I also added an API that returns errors and never panics on a bad TLE. Building the ground-track map for the live demo turned up a second upstream bug: `LatLongDeg` mapped 190° to 170° instead of −170°, mirroring the eastern half of every track, and called `log.Fatal` on an out-of-range latitude. Both are fixed and tested.

## Live demo

The page at https://aravindh97.github.io/orbit/ is this repo's Go code compiled to WebAssembly (`cmd/web`), with a plain HTML/JS frontend in `web/`. `.github/workflows/pages.yml` builds and publishes it on every push. GitHub Pages only serves static files, so anything that needs a database or a server runs against in-memory versions in the browser; the numbers in this README come from the full version.

```bash
./web/build.sh dist && python3 -m http.server -d dist 8000   # http://127.0.0.1:8000
```

## Run it

```bash
go run ./cmd/orbit screen -start 2026-11-02T06:00:00Z -hours 24          # all pairs, testdata catalogue
go run ./cmd/orbit screen -mine 25544,48274 -hours 72 -threshold 10      # ISS and CSS against everything
go run ./cmd/orbit socrates -html saved-socrates-page.html -out soc.csv
go run ./cmd/orbit eval -events events.json -socrates testdata/socrates-minrange.csv
go test -race -short ./...
```

For current data, download the catalogue from CelesTrak (`gp.php?GROUP=…&FORMAT=tle`, which updates every 2 hours, so please fetch politely) or from Space-Track, and pass it with `-tle`.

## Limits

- **Miss distance only, no probability.** Collision probability needs covariance (how uncertain each orbit is), and TLEs don't include it. SOCRATES estimates it. That's the next step.
- **TLE accuracy.** Positions from TLEs are good to about a kilometre and get worse every day. A real operator screens with their own precise orbit and confirms with a provider before manoeuvring.
- **Public groups only.** The bundled catalogue is CelesTrak's public groups. Use a full Space-Track catalogue for complete coverage.
- **Not a manoeuvre-planning tool.** Use it for awareness and triage.

## Explain it in 2 minutes

"Small satellite teams can't afford collision alerts, so I built a screener for the public catalogue. The naive approach compares 140 million pairs every 10 seconds. I cut that in three stages. Altitude bands rule out pairs that can never meet. A spatial hash only compares neighbours, with the cell size chosen from the maximum relative speed so nothing can be missed. A straight-line closest-approach estimate decides which pairs to refine on the real model. It checks 0.01% of the pairs and matched an exhaustive brute force on all 190 encounters. Six days for 16,763 objects takes under four minutes on 8 cores, about 3.8 million object-steps a second. I validated the SGP4 port against the reference to a millimetre, after fixing an epoch bug that put satellites a median 3.6 km off. Against SOCRATES, recall is limited by data, not by the algorithm: my element sets were 20 hours newer, and many SOCRATES objects aren't in the public groups. The repo has a script to rerun it on the same data."

## Likely interview questions

- **Why can the grid miss nothing?** Two objects can't close faster than about 16 km/s. If they come within D at some moment in the step, then at the sample time they were within D + 16 km/s × step. The cells are that size, and neighbouring cells are checked too.
- **Why golden-section and not Newton's method?** Distance as a function of time is smooth and has a single minimum within one step. Golden-section needs no derivatives and always converges.
- **Why do you get thousands of approaches that SOCRATES doesn't list?** 96% are Starlink against Starlink in its dense shells. The SOCRATES list I compared against is cut at the top 1,000, and I haven't confirmed whether SOCRATES screens a constellation against itself.
- **How would you scale to 100,000 objects?** The grid's cost grows with the number of objects, not pairs. So: shard time slices across machines, cache propagated positions per slice, and use the GPU for SGP4.
- **What was the hardest bug?** The epoch truncation in the upstream SGP4 port: a median 3.6 km and up to 7.5 km of position error. I found it by reading how the epoch was computed, and confirmed the fix against the reference implementation.
