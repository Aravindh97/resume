# aqcal

**Live demo: [aravindh97.github.io/aqcal](https://aravindh97.github.io/aqcal/)**: pick any of the 20 Berlin sensors to see raw, calibrated and reference PM2.5, then break it (stuck, saturated, offline, spike, drift) and watch the streaming detector catch it. The Go code runs in your browser as WebAssembly; see [Live demo](#live-demo) for what differs from the full version.

[![aqcal running in the browser](docs/screenshot.png)](https://aravindh97.github.io/aqcal/)

Calibration and fault detection for low-cost air-quality sensors, written as a streaming pipeline in Go.

Community-run air-quality sensors cost about €30 and cover whole cities, but they drift, get stuck, saturate, and disagree with official stations. So their readings tend to get ignored. `aqcal` does three things:
1. Calibrates each sensor against nearby official reference stations.
2. Detects broken sensors automatically.
3. Streams calibrated values and alerts, publishing a value only when the sensor can be trusted.

All data is real and public:
- **Low-cost sensors:** 280,000 readings from **20 SDS011 sensors in Berlin** (Sensor.Community), October 2026.
- **Reference:** hourly PM2.5 from **6 stations** of the German Environment Agency (UBA), each within 1.5 km of one of the sensors.
- **Humidity:** Open-Meteo city humidity.

Sources and licences are in [`testdata/README.md`](testdata/README.md).

## Results

Output of `aqcal eval -seeds 20` ([`docs/eval.txt`](docs/eval.txt)). Calibration is fitted on 1–18 October and scored on **19–28 October, which it never saw**.

**Health check, using training days only.** 5 of the 20 sensors were unusable and were excluded automatically:
- 3 don't track the reference at all (r between −0.07 and 0.02)
- 1 saturates at 999.9
- 1 has too little data

**Calibration** (hourly PM2.5 against the nearest reference station, 15 sensors, 3,516 held-out hours):

| method | MAE (µg/m³) | RMSE | R² |
|---|---|---|---|
| raw sensor | 4.81 | 5.98 | −0.11 |
| one linear fit for all sensors | 3.04 | 4.05 | 0.49 |
| **per-sensor linear** | **2.31** | **3.15** | **0.69** |
| per-sensor + κ-Köhler humidity correction | 2.37 | 3.25 | 0.67 |

- **Per-sensor calibration cuts the error by 52%** compared with raw readings, and by 24% compared with one fit for all sensors. Each unit has its own gain: fitted slopes range from 0.9 to 2.1 for 14 of the sensors. Sensor 95422 reads so low that its slope is about 17.
- **The humidity correction didn't help this month.** The fitted κ came out at 0.02, which means almost no humidity effect. On humid hours (RH ≥ 85%) it was level with the plain per-sensor fit (2.82 vs 2.85 MAE). The code keeps it as an option for wetter seasons; it isn't presented as an improvement.

**Fault detection.** Over 20 seeds, faults were injected into real held-out data: one per healthy sensor per seed, 300 in total.

| fault | detected | median delay |
|---|---|---|
| stuck (frozen value) | **60 / 60** | 2 h |
| saturated (999.9) | **60 / 60** | 1 h |
| offline (6 h gap) | **59 / 60** | 4 h |
| spike (8× plus 60 µg/m³) | **59 / 60** | 1 h |
| drift (gain ramps to 2.5× over 3 days) | **60 / 60** | 115 h |
| **overall** | **298 / 300 (99.3%)** | |

Misses:
- One offline fault overlapped a real gap in that sensor's data.
- One spike was on sensor 95422, which reads close to zero and was already marked untrusted.

**Alerts on clean held-out data**, meaning the same sensors with nothing injected:
- **5 offline alerts.** Each is a real gap of 4–13 hours in the archived data.
- **1 drift alert**, on sensor 95422, which reads about 0.6 µg/m³ when the reference reads about 9. I couldn't verify this one.

**Throughput.** 5.6 million readings (the month replayed for 400 sensors) took **0.81 s with 8 aggregator shards**. That's **6.9 million readings per second**, against 4.7 million per second with 1 shard.

## How it works

```
source ─► aggregators (sharded by sensor id) ─► detector + calibrator ─► sink (latest values, alerts, API)
   └──────── watermarks: "nothing older than T will arrive" ────────┘
```

**Streaming design**
- Each stage is a goroutine, connected by **bounded channels**, so a slow stage slows the source down instead of using up memory.
- Readings from one sensor always go to the same shard, which keeps them in order.
- The source sends a **watermark** every 5 simulated minutes.
- The detector closes an hour only after **every** shard has passed the watermark for that hour's end plus a 15-minute grace period. So late readings within the grace period are counted, and no hour is evaluated twice. `TestEveryHourEvaluatedOnce` checks this.

**Calibration**
- Model: `reference ≈ a + b · raw / g(RH)`, with κ-Köhler growth `g = 1 + κ·aw/(1−aw)`, where aw = RH/100, capped at 95%.
- **κ** is fitted once for the city, by grid search.
- **a** and **b** are fitted per sensor by least squares. Outliers beyond 4 MAD (median absolute deviation) are removed and the fit is repeated.

**Detectors**, evaluated per sensor-hour:

| detector | rule | why it's built that way |
|---|---|---|
| saturated | more than half the readings in the hour are 999.9 | |
| stuck | identical median for 4+ hours, or zero spread with many readings | Below 2 µg/m³ the SDS011 reports a few quantised values. The first version flagged clean air as "stuck", so those hours are skipped. |
| offline | no readings for 3 hours | |
| spike | calibrated value above 4× the median of the healthy sensors and more than 30 µg/m³ above it | |
| drift | the sensor's 72-hour gain against its reference has moved more than 1.5× from its training gain, relative to the typical change across all sensors, for 24 hours in a row | Real gains wander with weather, and a city-wide change isn't one sensor drifting. The first version, without the "relative to others" and "24 hours" parts, raised 7 drift alerts on clean data; this version raises 1. |

**Timezones.** UBA timestamps are CET all year, without daylight saving. This was checked by correlation rather than assumed. The median correlation between sensors and reference peaks at shift 0 h (r = 0.886) after converting CET to UTC, and at +1 h without the conversion.

## Live demo

The page at https://aravindh97.github.io/aqcal/ is this repo's Go code compiled to WebAssembly (`cmd/web`), with a plain HTML/JS frontend in `web/`. `.github/workflows/pages.yml` builds and publishes it on every push. GitHub Pages only serves static files, so anything that needs a database or a server runs against in-memory versions in the browser; the numbers in this README come from the full version.

```bash
./web/build.sh dist && python3 -m http.server -d dist 8000   # http://127.0.0.1:8000
```

## Run it

```bash
go run ./cmd/aqcal eval -seeds 20
go run ./cmd/aqcal replay -speed 3600 -addr 127.0.0.1:8090   # one hour of data per second
curl 127.0.0.1:8090/sensors ; curl 127.0.0.1:8090/alerts
go test -race ./...
```

## Limits

- **Small sample:** one city, one month, 20 sensors and 6 reference stations, some of them 1.5 km away. The R² of 0.69 includes real differences between the sensor's location and the station's.
- **Injected faults:** the 300 faults in the detection test are injected, so the test shows what the detectors catch, not how common each fault is in practice. Alerts on clean data are reported separately for that reason.
- **Slow drift detection:** it takes about 5 days. That's a deliberate trade for fewer false alarms.
- **Live API:** loopback only, no authentication. It's a demo of the stream, not a public service.

## Design decisions

- **Why watermarks and not just timestamps?** Shards run independently. Without a watermark, the detector can't know whether another shard still holds readings for an hour it is about to close.
- **Why bounded channels?** Backpressure. An unbounded queue hides a slow stage until memory runs out.
- **Why compare drift with other sensors?** Gains change with the aerosol mix and humidity across the whole city. Only one sensor changing *on its own* is drift.
- **Why didn't the humidity model help?** October in Berlin rarely had the very high humidity where hygroscopic growth matters, and the cap at 95% RH limits the correction. A winter or monsoon month would test it properly.
- **How do you avoid fooling yourself?** Calibration is scored on days it never saw. Broken sensors are excluded using training data only. Detectors are tested on injected faults, and their alerts on clean data are reported and checked against the real data.
