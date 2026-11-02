# foodwatch

**Live demo: [aravindh97.github.io/foodwatch](https://aravindh97.github.io/foodwatch/)**: pick a region to see its rainfall, prices and IPC history, see which regions are on WATCH or WARNING now, and rerun the backtest with either rainfall source. The Go code runs in your browser as WebAssembly; see [Live demo](#live-demo) for what differs from the full version.

Food-stress early warning in Go. It combines rainfall, staple cereal prices and livestock-to-cereal terms of trade per region, then backtests the alerts against what actually happened.

Rain shortfalls and market shocks appear in public data months before a food crisis is officially classified. `foodwatch` computes simple, explainable signals for each region every month. It then checks them against **IPC** classifications, the official, consensus-based scale used by governments and UN agencies, where **Phase 3** means "Crisis".

**Where:** Somalia, the only place where all the inputs are public at region level:
- IPC analyses: 14 rounds, January 2021 to January 2026, for 18 regions
- WFP market prices since 1995
- CHIRPS satellite rainfall since 1991

I started with India in mind, but India doesn't use IPC, so there was no comparable independent ground truth to test against.

## Results

Output of `foodwatch backtest` ([`docs/backtest-chirps.txt`](docs/backtest-chirps.txt)).

- **Event definition:** a region's population share in IPC Phase 3+ reaches 30% *and* has risen by at least 10 points since the previous analysis. That gives 26 events in 234 region-analyses.
- **No hindsight:** each analysis is scored using only the data available the month before it.
- **Chance baseline:** the same number of alerts placed at random, repeated 10,000 times.

| signal | events flagged | by chance | p | median lead time | precision (chance 0.24) |
|---|---|---|---|---|---|
| **rain deficit** (CHIRPS) | **20 / 26** | 10.7 | < 0.001 | **21.6 weeks** | 0.34 |
| any signal (WATCH) | 21 / 26 | 11.9 | < 0.001 | 21.6 weeks | 0.32 |
| rain deficit + market signal (WARNING) | 5 / 26 | 2.0 | 0.039 | 21.6 weeks | 0.33 |
| cereal price spike only | 5 / 26 | 3.0 | 0.165 | 26.1 weeks | 0.22 |
| terms of trade only | 1 / 26 | 0.4 | 0.376 | 26.3 weeks | 0.25 |

**By episode:**
- **2020–23 Horn of Africa drought** (five failed rainy seasons): 16 of 20 regional crisis onsets were flagged, a median **21.6 weeks** before the IPC analysis that recorded them.
- **2025 Deyr rains failure:** 4 of 6 were flagged, a median 4.4 weeks before the January 2026 analysis.

**What the numbers mean**

- **Rainfall carries the signal.** A failed rainy season flags most crisis onsets four to five months ahead, far better than chance.
- **Price signals add little.** Market signals alone don't beat chance here. Two reasons:
  - Somali cereal prices are partly set by imports.
  - WFP's series has gaps exactly when they matter. For example, Bay has no cereal prices from November 2020 to January 2022.
  
  So the combined WARNING is more selective, but catches only 5 events.
- **Precision is modest:** 0.34 against 0.24 by chance. Many rain-deficit alerts happen in regions that were already in crisis and didn't rise a further 10 points, so the event definition counts them as misses. Others happen where drought didn't turn into crisis.
- **Missed by the rain signal:** Togdheer (×2), Lower Juba, Sanaag (×2) and Galgaduud in January 2026. Rainfall at the market centroid looked normal in each case, and a 1° box is coarse for these regions. Galgaduud was still caught by WATCH: one goat bought only 53% of its usual cereal. That's the one onset where the market signals added something the rain signal missed.

**Rainfall data matters.**
- NASA POWER, the first source I used, put Bay's Deyr 2021 rains at **438 mm**. CHIRPS and ERA5 both put them at **92–93 mm**, and the failure of that season is well documented.
- The backtest therefore uses CHIRPS, the FEWS NET standard. ERA5 is kept as a second source ([`docs/backtest-era5.txt`](docs/backtest-era5.txt)): with ERA5 the rain signal flags 16 of 26 events (p = 0.004).
- During the ERA5 download, one region's request failed on rate limiting, and the first script silently reused the previous region's data. The fetch script now refuses to continue instead.

**Current signals** ([`docs/now.txt`](docs/now.txt)): as of September 2026, the latest the data allows, **Bay and Mudug are at WARNING**. Their Gu rains were at percentile 13 and 7, and cereal prices are 67–75% above normal. Most northern regions are at WATCH after a poor Gu. This is a signal, not a forecast.

## How it works

Each signal is computed per region per month **m**. Data for **m − 1** is the latest assumed to be in hand: CHIRPS final rainfall and WFP prices both arrive weeks after the month ends. Each signal is available at the end of month m.

| signal | rule |
|---|---|
| rain deficit | Rainfall in the current Gu (Apr–Jun) or Deyr (Oct–Dec) season so far, or in the last completed season, is in the driest 30% of 1991–2020 for the same months and region. |
| cereal price spike | The median across red sorghum, white maize and white sorghum of (price ÷ previous 24-month median) is 1.4 or more. Each region's prices are in a single currency every month; I checked that none mixes Somali and Somaliland shillings. |
| terms of trade drop | The kilograms of cereal one goat buys have fallen to 70% or less of the previous 24-month median. |
| level | **WARNING** = rain deficit plus a market signal. **WATCH** = any one signal. Every alert lists the reasons that triggered it. |

**Lead time** is measured from when the alert first became available to the start of the IPC analysis, counting back through consecutive alert months (up to 12).

**What I changed after seeing results** (so you can discount it):
- **Switched rainfall source**, from NASA POWER to CHIRPS, after POWER contradicted a documented drought.
- **Added a one-month data lag.** This makes the test stricter.
- **Scored seasons while they are still running.** Before this change, a failing season wasn't flagged until it ended.

The thresholds (30th percentile, 1.4×, 0.7×) were set once, before the first run, and never tuned.

## Live demo

The page at https://aravindh97.github.io/foodwatch/ is this repo's Go code compiled to WebAssembly (`cmd/web`), with a plain HTML/JS frontend in `web/`. `.github/workflows/pages.yml` builds and publishes it on every push. GitHub Pages only serves static files, so anything that needs a database or a server runs against in-memory versions in the browser; the numbers in this README come from the full version.

```bash
./web/build.sh dist && python3 -m http.server -d dist 8000   # http://127.0.0.1:8000
```

## Run it

```bash
go run ./cmd/foodwatch backtest              # CHIRPS rainfall
go run ./cmd/foodwatch backtest -rain era5
go run ./cmd/foodwatch timeline -region Bay  # month by month, with reasons
go run ./cmd/foodwatch now
go test -race ./...
./scripts/fetch.sh                           # rebuild testdata/ from the sources
```

## Limits

- **Small sample.** There are 26 events from 14 IPC rounds in one country, and most of them come from one drought. The p-values say it's better than chance, not how well it generalises.
- **Coarse rainfall.** It is averaged over a 1° box at each region's market centroid, not over cropland or pasture.
- **Patchy market data.** WFP price series have gaps, and these are the main reason the market signals are weak.
- **No conflict signal.** IPC outcomes in Somalia also depend on conflict and humanitarian access, which this model doesn't include.
- **Not a forecast.** It is a set of signals for analysts. It doesn't replace FEWS NET or IPC.

## Explain it in 2 minutes

"I wanted to see whether public data warns of food crises early enough to matter. Ground truth had to be independent, so I used IPC classifications. That meant Somalia, not India, which doesn't use IPC. Each month, for each region, I compute three signals using only data that would really have been available: rainfall percentile against 1991–2020, cereal prices against their own two-year median, and how much grain a goat buys. Then I backtest against 26 crisis onsets with a random-placement baseline. Rainfall alone flagged 20 of 26 onsets, about 22 weeks before the IPC analysis, against 11 by chance. The price signals didn't beat chance, mainly because of data gaps. My first rainfall source said the 2021 drought season was wet, which taught me to cross-check sources against known events before trusting a backtest."

## Likely interview questions

- **How do you avoid lookahead?** Each signal for month m uses data up to m − 1, and an analysis is scored with the signal from the month before it starts. The climatology (1991–2020) ends before the first analysis.
- **Why a random baseline?** Droughts are common in Somalia. Without a baseline, "flagged 20 of 26" says nothing about whether the signal is better than raising alerts at random.
- **Why is precision low?** Rainfall deficits are necessary but not sufficient. Many hit regions already in crisis or with other buffers. A real system would combine this with livelihoods, conflict and humanitarian data.
- **What would you do next?** Average rainfall over pasture and cropland masks instead of a box, add vegetation (NDVI) and conflict event data, and test on Ethiopia and Kenya IPC data.
