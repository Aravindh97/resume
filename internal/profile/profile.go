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
}

var Profile = ProfileData{
	Name:     "Aravindh Varadharaj",
	Role:     "Go backend",
	Focus:    "Ingestion pipelines · Retrieval with citations · Agent memory · Entity resolution",
	Location: "Tiruppur, Tamil Nadu (open to Bengaluru, on-site)",
	Email:    "aravindhvaradha@gmail.com",
	Links: []Link{{"github.com/Aravindh97", gh}, {"LinkedIn", "https://www.linkedin.com/in/aravindh-varadharaj-253231201"},
		{"resume.pdf", "static/resume.pdf"}},
	Projects: []Repo{
		{"Granth", "Document ingestion and hybrid retrieval for agents", "Go, Temporal, OpenSearch, Postgres, MCP", gh + "/granth"},
		{"Smriti", "Scoped long-term memory for LLM agents", "Go, Postgres + pgvector, Temporal, MCP", gh + "/smriti"},
		{"Sutra", "Enterprise connectors, ontology and entity resolution", "Go, Postgres CDC, REST, MCP", gh + "/sutra"},
		{"Resume", "This site: Raft mesh, Go WASM race simulator, git-native career", "Go, WebAssembly", gh + "/resume"},
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
	{"Resume", "Raft: 4 tests × 20 runs under -race", "pass", "", gh + "/resume/blob/main/internal/raft/raft_test.go"},
}
