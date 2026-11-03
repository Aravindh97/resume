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

var Profile = ProfileData{
	Name:     "Aravindh Varadharaj",
	Role:     "Go backend",
	Focus:    "Concurrency · Payments · Distributed systems · Data pipelines · Search and retrieval",
	Location: "Tiruppur, Tamil Nadu, India (open to relocation, on-site or remote)",
	Email:    "aravindhvaradha@gmail.com",
	Links: []Link{{"github.com/Aravindh97", gh}, {"LinkedIn", "https://www.linkedin.com/in/aravindh-varadharaj-253231201"},
		{"resume.pdf", "static/resume.pdf"}},
	Projects: []Repo{
		{"Granth", "Document ingestion and hybrid retrieval for agents", "Go, Temporal, OpenSearch, Postgres, MCP, React, TypeScript", gh + "/granth", pages + "/granth/"},
		{"Smriti", "Scoped long-term memory for LLM agents", "Go, Postgres + pgvector, Temporal, MCP, React, TypeScript", gh + "/smriti", pages + "/smriti/"},
		{"Sutra", "Enterprise connectors, ontology and entity resolution", "Go, Postgres CDC, REST, MCP, React, TypeScript", gh + "/sutra", pages + "/sutra/"},
		{"fairdrop", "Ticketing for high-demand releases with a verifiable queue", "Go, PostgreSQL, drand, React, TypeScript", gh + "/fairdrop", pages + "/fairdrop/"},
		{"paisa", "Exactly-once double-entry ledger with settlement reconciliation", "Go, PostgreSQL, React, TypeScript", gh + "/paisa", pages + "/paisa/"},
		{"orbit", "Satellite conjunction screener for the public catalogue", "Go, SGP4, React, TypeScript", gh + "/orbit", pages + "/orbit/"},
		{"relief", "Offline-first sync for disaster relief teams", "Go, CRDTs, React, TypeScript", gh + "/relief", pages + "/relief/"},
		{"aqcal", "Low-cost air sensor calibration and fault detection", "Go streaming pipeline, React, TypeScript", gh + "/aqcal", pages + "/aqcal/"},
		{"foodwatch", "Food-stress early warning backtested against IPC", "Go, CHIRPS, WFP, React, TypeScript", gh + "/foodwatch", pages + "/foodwatch/"},
		{"Pallanguzhi", "The Tamil mancala game against a search AI that explains its moves", "Go, WebAssembly, React, TypeScript", gh + "/pallanguzhi", pages + "/pallanguzhi/"},
		{"Resume", "This site: Raft mesh, Go WASM race simulator, git-native career", "Go, WebAssembly", gh + "/resume", ""},
	},
	Summary: []string{
		"Builder at Amazon since 2021 (digital marketing, then automating my team's workflows).",
		"Ten personal Go projects, built in my own time to learn, each with tests, CI, results measured against a baseline, and a live demo in the browser.",
	},
	Experience: []Job{{
		Title: "Sr. Digital Marketing Associate, Amazon",
		Dates: "2021 to present",
		Bullets: []string{
			"Built and automated internal tools and workflows for my team, replacing repetitive manual tasks with reliable, reusable solutions.",
			"Worked with stakeholders to find process gaps, then designed, built and supported automation that the team adopted in daily work.",
			"Grew from a marketing role into hands-on development, and built Go and Python backend systems outside my core responsibilities.",
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
