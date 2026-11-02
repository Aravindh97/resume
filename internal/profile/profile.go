// Package profile holds the resume content and eval board shared by the
// server, the static GitHub Pages build and the WebAssembly module.
package profile

const gh = "https://github.com/Aravindh97"

type Link struct{ Label, URL string }

type ProfileData struct {
	Name     string   `json:"name"`
	Role     string   `json:"role"`
	Focus    string   `json:"focus"`
	Location string   `json:"location"`
	Email    string   `json:"email"`
	Links    []Link   `json:"links"`
	Projects []Repo   `json:"projects"`
	Summary  []string `json:"summary"`
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
	Focus:    "Ingestion pipelines · Retrieval with citations · Agent memory · Entity resolution",
	Location: "Tiruppur, Tamil Nadu (open to Bengaluru, on-site)",
	Email:    "aravindhvaradha@gmail.com",
	Links: []Link{{"github.com/Aravindh97", gh}, {"LinkedIn", "https://www.linkedin.com/in/aravindh-varadharaj-253231201"},
		{"resume.pdf", "static/resume.pdf"}},
	Projects: []Repo{
		{"Granth", "Document ingestion and hybrid retrieval for agents", "Go, Temporal, OpenSearch, Postgres, MCP", gh + "/granth", pages + "/granth/"},
		{"Smriti", "Scoped long-term memory for LLM agents", "Go, Postgres + pgvector, Temporal, MCP", gh + "/smriti", pages + "/smriti/"},
		{"Sutra", "Enterprise connectors, ontology and entity resolution", "Go, Postgres CDC, REST, MCP", gh + "/sutra", pages + "/sutra/"},
		{"fairdrop", "Ticketing for high-demand releases with a verifiable queue", "Go, PostgreSQL, drand", gh + "/fairdrop", pages + "/fairdrop/"},
		{"paisa", "Exactly-once double-entry ledger with settlement reconciliation", "Go, PostgreSQL", gh + "/paisa", pages + "/paisa/"},
		{"orbit", "Satellite conjunction screener for the public catalogue", "Go, SGP4", gh + "/orbit", pages + "/orbit/"},
		{"relief", "Offline-first sync for disaster relief teams", "Go, CRDTs", gh + "/relief", pages + "/relief/"},
		{"aqcal", "Low-cost air sensor calibration and fault detection", "Go streaming pipeline", gh + "/aqcal", pages + "/aqcal/"},
		{"foodwatch", "Food-stress early warning backtested against IPC", "Go, CHIRPS, WFP", gh + "/foodwatch", pages + "/foodwatch/"},
		{"Resume", "This site: Raft mesh, Go WASM race simulator, git-native career", "Go, WebAssembly", gh + "/resume", ""},
	},
	Summary: []string{
		"Builder at Amazon since 2021 (digital marketing, then automating my team's workflows).",
		"Three open-source Go systems for agent knowledge and memory, each tested against real Postgres, OpenSearch and Temporal and measured with its own eval.",
	},
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
