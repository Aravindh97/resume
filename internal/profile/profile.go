// Package profile holds the resume content and eval board shared by the
// server, the static GitHub Pages build and the WebAssembly module.
package profile

const gh = "https://github.com/Aravindh97"

type Link struct {
	Label string `json:"label"`
	URL   string `json:"url"`
}

type Job struct {
	Title   string   `json:"title"`
	Dates   string   `json:"dates"`
	Bullets []string `json:"bullets"`
}

type ProfileData struct {
	Name     string   `json:"name"`
	Role     string   `json:"role"`
	Focus    string   `json:"focus"`
	Location string   `json:"location"`
	Email    string   `json:"email"`
	Links    []Link   `json:"links"`
	Projects []Repo   `json:"projects"`
	Summary  []string `json:"summary"`
	// Experience and Education match the PDF resume.
	Experience []Job  `json:"experience"`
	Education  string `json:"education"`
}

type Repo struct {
	Name  string `json:"name"`
	What  string `json:"what"`
	Stack string `json:"stack"`
	URL   string `json:"url"`
	Live  string `json:"live,omitempty"` // in-browser demo on GitHub Pages
}

// pages is where each repo's in-browser demo is published.
const pages = "https://aravindh97.github.io"

// Full-stack apps run on Render's free tier (with a free Neon Postgres).
func render(name string) string { return "https://" + name + ".onrender.com/" }

var Profile = ProfileData{
	Name:     "Aravindh Varadharaj",
	Role:     "Full-stack",
	Focus:    "Go · PostgreSQL · React · TypeScript · Payments · Offline sync · Real-time apps",
	Location: "Tiruppur, Tamil Nadu, India (open to relocation, on-site or remote)",
	Email:    "aravindhvaradha@gmail.com",
	Links: []Link{{"github.com/Aravindh97", gh}, {"LinkedIn", "https://www.linkedin.com/in/aravindh-varadharaj-253231201"},
		{"resume.pdf", "static/resume.pdf"}},
	Projects: []Repo{
		{"Kaasu", "Personal finance: a UPI-style wallet on a double-entry ledger, a fraud rule, and a statement analyser that can run on your device", "Go, PostgreSQL, WebAssembly, React, TypeScript", gh + "/kaasu", render("kaasu")},
		{"Arogya", "Hospital platform: OPD tokens, patient records with an access log, beds and a blood bank", "Go, PostgreSQL, Server-Sent Events, React, TypeScript", gh + "/arogya", render("arogya")},
		{"Thari", "Garment order tracker for a factory and its buyers, with live updates", "Go, PostgreSQL, Server-Sent Events, React, TypeScript", gh + "/thari", render("thari")},
		{"Kurippu", "Offline-first notes: a Go CRDT on the server and in the browser", "Go, WebAssembly, PostgreSQL, IndexedDB, React, TypeScript", gh + "/kurippu", render("kurippu")},
		{"Granth", "Document ingestion and hybrid retrieval for agents", "Go, Temporal, OpenSearch, Postgres, MCP, React, TypeScript", gh + "/granth", pages + "/granth/"},
		{"Smriti", "Scoped long-term memory for LLM agents", "Go, Postgres + pgvector, Temporal, MCP, React, TypeScript", gh + "/smriti", pages + "/smriti/"},
		{"Sutra", "Enterprise connectors, ontology and entity resolution", "Go, Postgres CDC, REST, MCP, React, TypeScript", gh + "/sutra", pages + "/sutra/"},
		{"fairdrop", "Ticketing for high-demand releases with a verifiable queue", "Go, PostgreSQL, drand, React, TypeScript", gh + "/fairdrop", pages + "/fairdrop/"},
		{"paisa", "Exactly-once double-entry ledger with settlement reconciliation", "Go, PostgreSQL, React, TypeScript", gh + "/paisa", pages + "/paisa/"},
		{"orbit", "Satellite conjunction screener for the public catalogue", "Go, SGP4, React, TypeScript", gh + "/orbit", pages + "/orbit/"},
		{"relief", "Offline-first sync for disaster relief teams", "Go, CRDTs, React, TypeScript", gh + "/relief", pages + "/relief/"},
		{"aqcal", "Low-cost air sensor calibration and fault detection", "Go streaming pipeline, React, TypeScript", gh + "/aqcal", pages + "/aqcal/"},
		{"foodwatch", "Food-stress early warning backtested against IPC", "Go, CHIRPS, WFP, React, TypeScript", gh + "/foodwatch", pages + "/foodwatch/"},
		{"Pallanguzhi", "The Tamil mancala game, online multiplayer over WebSockets, or against a search AI that explains its moves", "Go, WebSockets, WebAssembly, React, TypeScript", gh + "/pallanguzhi", pages + "/pallanguzhi/"},
		{"Resume", "This site: Raft mesh, Go WASM race simulator, git-native career", "Go, WebAssembly", gh + "/resume", ""},
	},
	Summary: []string{
		"Builder at Amazon since 2021 (digital marketing, then automating my team's workflows).",
		"Fourteen personal projects, built in my own time to learn: five full-stack apps (Go on PostgreSQL with React + TypeScript, tested in real browsers and load-tested) and nine Go systems projects, each with tests, CI, measured results and a live demo.",
	},
	Experience: []Job{{
		Title: "Sr. Digital Marketing Associate, Amazon",
		Dates: "2021 to present",
		Bullets: []string{
			"Ran digital marketing campaigns and worked with the AU Retail Marketing and Merchandising team to improve the online storefront and the customer journey.",
			"Built internal automation tools in Go that streamlined cross-functional workflows and removed manual bottlenecks for the team.",
			"Worked as the team's data engineer: built data extraction pipelines, analysed retail datasets and designed interactive Power BI dashboards that gave leadership insights for regional commercial decisions.",
			"Bridged marketing, merchandising and engineering, and built full-stack Go and React + TypeScript applications outside my core responsibilities.",
		},
	}},
	Education: "Bachelor of Engineering (B.E.) in Mechanical Engineering",
}

type Eval struct {
	Project  string `json:"project"`
	Metric   string `json:"metric"`
	Result   string `json:"result"`
	Baseline string `json:"baseline"`
	Source   string `json:"source"` // report that produced the number
}

var Evals = []Eval{
	{"Kaasu", "payments retried with the same key under load", "431 sent twice, 0 paid twice", "", gh + "/kaasu/blob/main/docs/loadtest.txt"},
	{"Kaasu", "ledger check after 10,306 requests in 20 s", "balanced", "", gh + "/kaasu/blob/main/docs/loadtest.txt"},
	{"Arogya", "load: 41,874 requests in 20 s, walk-in token p99", "61 ms, 0 errors", "", gh + "/arogya/blob/main/docs/loadtest.txt"},
	{"Arogya", "10 emergency requests racing for scarce O- units", "no unit reserved twice", "", gh + "/arogya/blob/main/internal/api/api_test.go"},
	{"Thari", "concurrent progress updates saved (lost-update bug found, fixed)", "40 / 40", "8 / 40 first version", gh + "/thari#how-the-important-parts-work"},
	{"Thari", "load: 50 users, about 3,700 requests/s, p99", "under 26 ms", "", gh + "/thari/blob/main/docs/loadtest.txt"},
	{"Kurippu", "random offline editing sessions that converge", "1,500 / 1,500", "", gh + "/kurippu#tests"},
	{"Kurippu", "keystroke at 10,000 characters", "1.2 ms", "", gh + "/kurippu/blob/main/docs/bench.txt"},
	{"Pallanguzhi", "load: 1,000 games, 4,000 WebSocket connections", "0 failed games", "", gh + "/pallanguzhi/blob/main/docs/loadtest.txt"},
	{"Granth", "retrieval hit@5, 44 questions, hybrid on OpenSearch", "0.977", "0.955 BM25", gh + "/granth/blob/main/docs/eval-opensearch.txt"},
	{"Granth", "MRR@10", "0.925", "0.891 BM25", gh + "/granth/blob/main/docs/eval-opensearch.txt"},
	{"Granth", "OpenSearch stopped mid-ingest", "recovered, 0 duplicates", "", gh + "/granth#failure-handling-tested-against-real-services"},
	{"Smriti", "correct current fact, 34 questions", "1.000", "0.676 raw history", gh + "/smriti/blob/main/docs/eval.txt"},
	{"Smriti", "stale answers", "0.000", "0.176 raw history", gh + "/smriti/blob/main/docs/eval.txt"},
	{"Sutra", "entity match F1, held-out seeds (300 companies)", "0.960–0.996", "0.720–0.768 GSTIN only", gh + "/sutra/blob/main/docs/eval.txt"},
	{"Sutra", "CDC kill -9 mid-stream, then restart", "source = warehouse (md5)", "", gh + "/sutra#connectors"},
	{"fairdrop", "seats sold twice, 50,000 simulated buyers (3 runs)", "0", "156,874 naive", gh + "/fairdrop/tree/main/docs"},
	{"paisa", "kill -9 ×20 during 50,000 payments", "balances = clean run", "", gh + "/paisa/blob/main/docs/kill-test.txt"},
	{"paisa", "planted reconciliation mismatches found", "477 / 477", "", gh + "/paisa/blob/main/docs/chaos-run.txt"},
	{"orbit", "encounters found vs exhaustive brute force", "190 / 190", "checks 0.07% of pairs", gh + "/orbit/blob/main/docs/validation.txt"},
	{"relief", "devices converged, 10,000 randomised partition runs", "10,000 / 10,000", "LWW lost data in all", gh + "/relief/blob/main/docs/sim-10000.txt"},
	{"aqcal", "hourly PM2.5 error on held-out days (MAE µg/m³)", "2.31", "4.81 raw", gh + "/aqcal/blob/main/docs/eval.txt"},
	{"foodwatch", "IPC crisis onsets flagged by rainfall signal", "20 / 26, 21.6 wk early", "10.7 by chance", gh + "/foodwatch/blob/main/docs/backtest-chirps.txt"},
	{"Resume", "Raft: 4 tests × 20 runs under -race", "pass", "", gh + "/resume/blob/main/internal/raft/raft_test.go"},
}
