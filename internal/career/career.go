// Package career is the resume as a commit history.
//
// The same data drives the CLI (`career log`, `career show`, `career
// cherry-pick skills=go,temporal`), the terminal panel on the website, and
// `career init DIR`, which writes a real git repository whose commits are
// the milestones, so `git log` works on it.
//
// Dates that are not recorded are shown as "date not recorded" instead of
// being made up. Edit milestones below to add them.
package career

import (
	"crypto/sha1"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

type Milestone struct {
	ID      string   // stable short id used by `career show`
	Date    string   // YYYY-MM-DD, YYYY, or "" if not recorded
	Subject string   // conventional-commit style subject
	Body    []string // bullet lines
	Skills  []string
	Link    string
}

const GitHub = "https://github.com/Aravindh97"

var Milestones = []Milestone{
	{ID: "education", Subject: "init: B.E. Mechanical Engineering",
		Body: []string{"Bachelor of Engineering in Mechanical Engineering"}, Skills: []string{"engineering"}},
	{ID: "amazon", Date: "2021", Subject: "feat(amazon): join Amazon as Digital Marketing Associate",
		Body:   []string{"Now Sr. Digital Marketing Associate"},
		Skills: []string{"stakeholders"}},
	{ID: "automation", Subject: "feat(amazon): automate team workflows",
		Body: []string{"Built and automated internal tools and workflows that replaced repetitive manual tasks",
			"Found process gaps with stakeholders, then designed, built and supported the automation"},
		Skills: []string{"automation", "python"}},
	{ID: "platforms", Subject: "feat: six full-stack platforms in Python and Go",
		Body: []string{"OpenSchool Enterprise, FamineLens, Kindred (Python/FastAPI)",
			"TerraZero, Nuclear Legacy & Civil Safety, EcoSignal (Go net/http)",
			"TerraZero: live USGS feed with bounded fetch, cache and last-good fallback"},
		Skills: []string{"go", "python", "fastapi", "react", "nextjs"}, Link: GitHub},
	{ID: "granth", Date: "2026-10-31", Subject: "feat(granth): ingestion pipeline and hybrid retrieval",
		Body: []string{"One Temporal workflow per document; idempotent activities, dead-letter ledger in Postgres",
			"BM25 + filtered k-NN fused with RRF in OpenSearch; citations to file, page and section",
			"Survived a live OpenSearch outage with no duplicate chunks",
			"Eval: hit@5 0.977, MRR 0.925 (hybrid) vs 0.955 / 0.891 (BM25)"},
		Skills: []string{"go", "temporal", "opensearch", "postgres", "mcp", "retrieval", "evals"}, Link: GitHub + "/granth"},
	{ID: "smriti", Date: "2026-10-31", Subject: "feat(smriti): scoped long-term memory for agents",
		Body: []string{"User, team and org memories in Postgres + pgvector, one access rule in every query",
			"Add / confirm / supersede under an advisory lock; bitemporal history with sources",
			"Eval: 1.000 correct, 0 stale vs 0.676 / 0.176 for raw chat history"},
		Skills: []string{"go", "postgres", "pgvector", "temporal", "mcp", "memory", "evals"}, Link: GitHub + "/smriti"},
	{ID: "sutra", Date: "2026-10-31", Subject: "feat(sutra): enterprise connectors and entity resolution",
		Body: []string{"Postgres logical-replication CDC; source == warehouse after kill -9 and restart",
			"Keyset incremental CRM sync with Retry-After handling",
			"GSTIN checksum, PAN, transliteration-aware names; lineage for every golden value",
			"Eval: held-out F1 0.96-0.996 vs 0.72-0.77 GSTIN-only"},
		Skills: []string{"go", "postgres", "cdc", "entity-resolution", "mcp", "evals"}, Link: GitHub + "/sutra"},
	{ID: "resume", Date: "2026-10-31", Subject: "feat(resume): this resume as a Raft mesh, a race simulator and a git log",
		Body: []string{"Raft leader election and log replication across nodes, with a chaos button",
			"Race simulator compiled to WebAssembly: no sync vs sync.Mutex vs channels",
			"Ask my work: extractive search over the three repos with file and line citations"},
		Skills: []string{"go", "raft", "wasm", "concurrency"}, Link: GitHub + "/resume"},
}

// Hash is a stable short id for display (not a git object id).
func (m Milestone) Hash() string {
	s := sha1.Sum([]byte(m.ID + m.Subject))
	return hex.EncodeToString(s[:])[:7]
}

func (m Milestone) DateLabel() string {
	if m.Date == "" {
		return "date not recorded"
	}
	return m.Date
}

func Log() string {
	var b strings.Builder
	for i := len(Milestones) - 1; i >= 0; i-- {
		m := Milestones[i]
		head := ""
		if i == len(Milestones)-1 {
			head = " (HEAD -> main)"
		}
		fmt.Fprintf(&b, "%s%s %s  [%s]\n", m.Hash(), head, m.Subject, m.DateLabel())
	}
	return b.String()
}

func Find(id string) (Milestone, bool) {
	for _, m := range Milestones {
		if m.ID == id || m.Hash() == id {
			return m, true
		}
	}
	return Milestone{}, false
}

func Show(id string) (string, error) {
	m, ok := Find(id)
	if !ok {
		return "", fmt.Errorf("unknown milestone %q (try: %s)", id, ids())
	}
	var b strings.Builder
	fmt.Fprintf(&b, "commit %s\nDate:   %s\n\n    %s\n\n", m.Hash(), m.DateLabel(), m.Subject)
	for _, l := range m.Body {
		fmt.Fprintf(&b, "    + %s\n", l)
	}
	fmt.Fprintf(&b, "\n    skills: %s\n", strings.Join(m.Skills, ", "))
	if m.Link != "" {
		fmt.Fprintf(&b, "    -> %s\n", m.Link)
	}
	return b.String(), nil
}

func ids() string {
	var s []string
	for _, m := range Milestones {
		s = append(s, m.ID)
	}
	return strings.Join(s, ", ")
}

// CherryPick returns the milestones that use every requested skill, as a
// short markdown resume for that stack.
func CherryPick(skills []string) string {
	want := map[string]bool{}
	for _, s := range skills {
		if s = strings.ToLower(strings.TrimSpace(s)); s != "" {
			want[s] = true
		}
	}
	var b strings.Builder
	keys := make([]string, 0, len(want))
	for k := range want {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	fmt.Fprintf(&b, "picking commits that use %s\n\n", strings.Join(keys, " AND "))
	picked := 0
	for i := len(Milestones) - 1; i >= 0; i-- {
		m := Milestones[i]
		have := map[string]bool{}
		for _, s := range m.Skills {
			have[s] = true
		}
		ok := len(want) > 0
		for k := range want {
			if !have[k] {
				ok = false
			}
		}
		if !ok {
			continue
		}
		picked++
		fmt.Fprintf(&b, "  picked %s %s\n", m.Hash(), m.Subject)
		for _, l := range m.Body {
			fmt.Fprintf(&b, "         - %s\n", l)
		}
	}
	if picked == 0 {
		b.WriteString("  nothing matches all of those skills\n")
	}
	return b.String()
}

// Init writes a real git repository at dir with one commit per milestone.
// Milestones without a recorded date are committed one day after the
// previous one and say so in the message.
func Init(dir string) error {
	if _, err := exec.LookPath("git"); err != nil {
		return errors.New("git is not installed")
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	if entries, _ := os.ReadDir(dir); len(entries) > 0 {
		return fmt.Errorf("%s is not empty", dir)
	}
	run := func(env []string, args ...string) error {
		c := exec.Command("git", args...)
		c.Dir = dir
		c.Env = append(os.Environ(), env...)
		if out, err := c.CombinedOutput(); err != nil {
			return fmt.Errorf("git %s: %v: %s", args[0], err, out)
		}
		return nil
	}
	if err := run(nil, "init", "-q", "-b", "main"); err != nil {
		return err
	}
	prev := time.Date(2015, 1, 1, 9, 0, 0, 0, time.UTC)
	for _, m := range Milestones {
		at, note := prev.AddDate(0, 0, 1), "\n\n[date not recorded]"
		if m.Date != "" {
			layout := "2006-01-02"
			if len(m.Date) == 4 {
				layout = "2006"
			}
			if t, err := time.Parse(layout, m.Date); err == nil {
				at, note = t.Add(9*time.Hour), ""
			}
		}
		if at.Before(prev) {
			at = prev.Add(time.Minute)
		}
		prev = at
		path := filepath.Join(dir, m.ID+".md")
		show, _ := Show(m.ID)
		if err := os.WriteFile(path, []byte(show), 0o644); err != nil {
			return err
		}
		body := m.Subject + "\n"
		for _, l := range m.Body {
			body += "\n- " + l
		}
		date := at.Format(time.RFC3339)
		env := []string{"GIT_AUTHOR_NAME=Aravindh Varadharaj", "GIT_AUTHOR_EMAIL=aravindhvaradha@gmail.com",
			"GIT_COMMITTER_NAME=Aravindh Varadharaj", "GIT_COMMITTER_EMAIL=aravindhvaradha@gmail.com",
			"GIT_AUTHOR_DATE=" + date, "GIT_COMMITTER_DATE=" + date}
		if err := run(env, "add", m.ID+".md"); err != nil {
			return err
		}
		if err := run(env, "commit", "-q", "--no-verify", "-m", body+note); err != nil {
			return err
		}
	}
	return nil
}

// Run executes one CLI command line (shared by the binary and the website).
func Run(args []string) (string, error) {
	if len(args) == 0 {
		return Help, nil
	}
	switch args[0] {
	case "log":
		return Log(), nil
	case "show":
		if len(args) < 2 {
			return "", errors.New("usage: career show <id>")
		}
		return Show(args[1])
	case "cherry-pick":
		var skills []string
		for _, a := range args[1:] {
			skills = append(skills, strings.Split(strings.TrimPrefix(a, "skills="), ",")...)
		}
		return CherryPick(skills), nil
	case "help", "-h", "--help":
		return Help, nil
	}
	return "", fmt.Errorf("unknown command %q\n\n%s", args[0], Help)
}

const Help = `career: Aravindh Varadharaj's resume as a commit history

  career log                         milestones, newest first
  career show <id>                   one milestone (granth, smriti, sutra, ...)
  career cherry-pick skills=go,temporal
                                     only the work that used all of those skills
  career init <dir>                  write a real git repo, then: cd <dir> && git log
`
