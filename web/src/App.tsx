import { useCallback, useEffect, useRef, useState, type FormEvent } from "react";
import "./app.css";
import { api, currentMode } from "./lib/go";
import { Card, reportError, useLoad } from "./lib/ui";
import type { Eval, NodeStatus, Passage, ProfileData, Repo, Snapshot } from "./types.gen";

// Envelopes from the API (maps); the types inside are generated from Go.
interface AskResult { query: string; passages: Passage[] | null; low_confidence: boolean; took_ms: number; served_by: string }
interface Cluster { served_by: string; nodes: NodeStatus[] }

const shot = (repo: string) => `${import.meta.env.BASE_URL}shots/${repo}.jpg`;
const repoOf = (p: Repo) => p.url.split("/").pop()!;

function Hero({ p }: { p: ProfileData }) {
  const projects = p.projects ?? [];
  const live = projects.filter((x) => x.live).length;
  return (
    <section className="hero">
      <div>
        <h1>{p.name}</h1>
        <div className="role">{p.role} engineer</div>
        <div className="focus">{p.focus}</div>
        <div className="where">{p.location} · <a href={`mailto:${p.email}`}>{p.email}</a></div>
        <div className="links">
          {(p.links ?? []).map((l) => <a key={l.url} className="btn ghost" href={l.url}>{l.label}</a>)}
        </div>
        {(p.summary ?? []).map((s) => <p key={s}>{s}</p>)}
      </div>
      <div className="glance">
        <div><b>{projects.filter((x) => x.name !== "Resume").length}</b>personal projects</div>
        <div><b>{live}</b>with a live demo</div>
        <div><b>Go</b>backends, compiled to WebAssembly for the demos</div>
        <div><b>React + TS</b>frontends, typed against the Go structs</div>
      </div>
    </section>
  );
}

export function Projects({ p }: { p: ProfileData }) {
  return (
    <section className="block">
      <h2 className="section">Personal projects</h2>
      <p className="muted">Built in my own time to learn; not affiliated with my employer. Each demo runs the repo's own Go code in your browser. Where the full system uses Postgres, OpenSearch or Temporal, the demo uses an in-memory store with the same rules, and the README reports the numbers measured against the real services.</p>
      <div className="grid">
        {(p.projects ?? []).filter((x) => x.live).map((x) => (
          <article key={x.name} className="proj">
            <a href={x.live} aria-label={`${x.name} live demo`}><img src={shot(repoOf(x))} alt={`${x.name} running in the browser`} loading="lazy" /></a>
            <div className="body">
              <h3>{x.name}</h3>
              <p>{x.what}</p>
              <div className="chips">{x.stack.split(/,\s*/).map((s) => <span key={s} className="chip">{s}</span>)}</div>
              <div className="actions"><a className="btn" href={x.live}>Live demo</a><a className="btn ghost" href={x.url}>Code</a></div>
            </div>
          </article>
        ))}
      </div>
    </section>
  );
}

function Results() {
  const [evals] = useLoad(() => api<Eval[]>("GET", "api/v1/evals"), []);
  return (
    <Card wide title="Measured, not claimed">
      <table>
        <thead><tr><th>Project</th><th>Metric</th><th>Result</th><th>Baseline</th></tr></thead>
        <tbody>
          {evals?.map((e, i) => (
            <tr key={i}><td><a href={e.source}>{e.project}</a></td><td>{e.metric}</td><td className="mono">{e.result}</td><td className="mono muted">{e.baseline}</td></tr>
          ))}
        </tbody>
      </table>
      <div className="muted">Each project links to the report that produced the number.</div>
    </Card>
  );
}

const examples = [
  "How does Granth avoid duplicate chunks on retry?",
  "How does Smriti stop two writers creating two current facts?",
  "How does Sutra validate a GSTIN?",
  "How does fairdrop stop a seat being sold twice?",
  "How does paisa make payments exactly once?",
  "How does the Pallanguzhi AI prune its search?",
];

export function Ask() {
  const [q, setQ] = useState("");
  const [r, setR] = useState<AskResult | null>(null);
  const ask = useCallback(async (query: string) => {
    setQ(query);
    setR(await api<AskResult>("GET", "api/v1/ask?q=" + encodeURIComponent(query)));
  }, []);
  const submit = (e: FormEvent) => {
    e.preventDefault();
    ask(q).catch(reportError);
  };
  return (
    <Card wide n="01" title="Ask my work: search over the source of my project repos, every answer cited">
      <form className="row" onSubmit={submit}>
        <input value={q} onChange={(e) => setQ(e.target.value)} maxLength={300} style={{ flex: 1 }} aria-label="question" placeholder="Ask about any of the projects…" />
        <button type="submit">Ask</button>
      </form>
      <div className="chips">{examples.map((x) => <button key={x} className="ghost small" onClick={() => ask(x).catch(reportError)}>{x}</button>)}</div>
      {r && (
        <>
          {r.low_confidence && <div className="warn">No strong match in the repos; these are the closest passages.</div>}
          {(r.passages ?? []).map((p) => (
            <div key={p.url} className="passage">
              <div className="mono meta"><a href={p.url} target="_blank" rel="noopener">{p.repo}/{p.file}:{p.start_line}-{p.end_line}</a> · {p.title} · score {p.score}</div>
              <pre>{p.text}</pre>
            </div>
          ))}
          <div className="muted">BM25 over the repo snapshot · {r.took_ms} ms · served by {r.served_by}</div>
        </>
      )}
      <div className="muted">Extractive: you see the actual passages and lines, nothing generated.</div>
    </Card>
  );
}

function Mesh() {
  const [c, setC] = useState<Cluster | null>(null);
  const [log, setLog] = useState<string[]>([]);
  const [okFail, setOkFail] = useState([0, 0]);
  const last = useRef<{ leader: string | null; term: number | null; ver: number | null }>({ leader: null, term: null, ver: null });
  const add = (s: string) => setLog((l) => [...l.slice(-30), `[${new Date().toLocaleTimeString()}] ${s}`]);
  useEffect(() => {
    let live = true;
    const poll = async () => {
      try {
        const r = await api<Cluster>("GET", "api/v1/cluster");
        if (!live) return;
        setOkFail(([o, f]) => [o + 1, f]);
        const leader = r.nodes.find((n) => n.role === "leader" && !n.isolated);
        const term = Math.max(...r.nodes.map((n) => n.term));
        const ver = Math.max(...r.nodes.map((n) => n.version));
        const L = last.current;
        if (leader && leader.id !== L.leader) add(`leader is ${leader.id} (${leader.region}), term ${leader.term}`);
        if (!leader && L.leader) add("no leader: election in progress");
        if (L.term !== null && term > L.term) add(`term ${L.term} → ${term}`);
        if (L.ver !== null && ver > L.ver) add(`resume v${ver} committed on a majority`);
        last.current = { leader: leader?.id ?? null, term, ver };
        setC(r);
      } catch {
        if (live) setOkFail(([o, f]) => [o, f + 1]);
      }
    };
    void poll();
    const t = setInterval(poll, 700);
    return () => {
      live = false;
      clearInterval(t);
    };
  }, []);
  const chaos = async (target: string) => {
    try {
      const r = await api<{ isolated: string; for_s: number }>("POST", "api/v1/cluster/chaos", { target });
      add(`✖ isolated ${r.isolated} for ${r.for_s}s`);
    } catch (e) {
      add((e as Error).message);
    }
  };
  const nodes = c?.nodes ?? [];
  const leader = nodes.find((n) => n.role === "leader" && !n.isolated);
  return (
    <Card n="02" title={`Resume mesh: Raft across ${nodes.length || 4} nodes${currentMode() === "static" ? ", running in this tab" : ""}`}>
      <div className="stat">
        <span>term <b>{nodes.length ? Math.max(...nodes.map((n) => n.term)) : "–"}</b></span>
        <span>leader <b>{leader ? leader.id : "electing…"}</b></span>
        <span>resume version <b>v{nodes.length ? Math.max(...nodes.map((n) => n.version)) : 0}</b></span>
        <span>requests ok <b>{okFail[0]}</b> / failed <b>{okFail[1]}</b></span>
      </div>
      <div className="nodes">
        {nodes.map((n) => {
          const down = n.isolated || !n.up;
          return (
            <div key={n.id} className={"node" + (down ? " down" : "") + (n.role === "leader" && !down ? " leader" : "")}>
              <div className="tag">{n.role === "leader" && !down ? "LEADER" : ""}</div>
              <b>{n.region || n.id}</b>
              <div className="muted mono">{n.id}</div>
              <div className="muted">{!n.up ? "unreachable" : n.isolated ? "isolated (partition)" : `${n.role} · v${n.version} · commit ${n.commit_index}`}</div>
            </div>
          );
        })}
      </div>
      <div className="row">
        <button className="bad" onClick={() => chaos("leader")}>Isolate the leader</button>
        <button className="ghost" onClick={() => {
          const f = nodes.filter((n) => n.role === "follower" && !n.isolated && n.up);
          if (!f.length) return add("no healthy follower to isolate");
          void chaos(f[Math.floor(Math.random() * f.length)].id);
        }}>Isolate a follower</button>
        <button className="ghost" onClick={async () => {
          try {
            const r = await api<{ committed_by: string }>("POST", "api/v1/cluster/write");
            add(`update proposed to leader ${r.committed_by} and committed`);
          } catch (e) {
            add((e as Error).message);
          }
        }}>Replicate an update</button>
      </div>
      <div className="log mono">{log.map((l, i) => <div key={i}>{l}</div>)}</div>
      <div className="muted">Four real <a href="https://github.com/Aravindh97/resume/blob/main/internal/raft/raft.go">internal/raft</a> nodes. Isolation is a network partition for a few seconds; at most one node may be out, so a majority always remains.</div>
    </Card>
  );
}

type RaceMode = "none" | "mutex" | "chan";

export function Race() {
  const [mode, setMode] = useState<RaceMode>("none");
  const [writers, setWriters] = useState(16);
  const [snap, setSnap] = useState<Snapshot | null>(null);
  const [want, setWant] = useState(0);
  const cfg = useRef({ mode, writers });
  cfg.current = { mode, writers };
  useEffect(() => {
    const t = setInterval(() => {
      if (!window.racesimStep) return;
      const { mode: m, writers: w } = cfg.current;
      setSnap(JSON.parse(window.racesimStep(m, w)));
      setWant((x) => x + w);
    }, 300);
    return () => clearInterval(t);
  }, []);
  const reset = (m: RaceMode) => {
    setMode(m);
    window.racesimReset?.();
    setWant(0);
  };
  const torn = (snap?.fields ?? []).filter((f) => f.torn).length;
  return (
    <Card n="03" title="Race simulator: real goroutines in Go WebAssembly">
      <div className="row">
        <span className="muted">writers per tick</span>
        <input type="range" min={1} max={128} value={writers} onChange={(e) => setWriters(+e.target.value)} aria-label="writers" /><b>{writers}</b>
        <div className="switch">
          {(["none", "mutex", "chan"] as RaceMode[]).map((m) => (
            <button key={m} className={mode === m ? "on" : "ghost"} onClick={() => reset(m)}>{m === "none" ? "no sync" : m === "mutex" ? "sync.Mutex" : "channels"}</button>
          ))}
        </div>
        <button className="ghost" onClick={() => reset(mode)}>reset</button>
      </div>
      <div className="stat">
        <span>increments <b>{want}</b></span><span>counted <b>{snap?.views ?? 0}</b></span>
        <span>lost <b className={want - (snap?.views ?? 0) ? "bad" : ""}>{want - (snap?.views ?? 0)}</b></span><span>torn fields <b className={torn ? "bad" : ""}>{torn}</b></span>
      </div>
      <pre className="race mono">
        {snap?.fields?.map((f) => (
          <div key={f.name}>
            {f.name.padEnd(6)}: {Array.from({ length: Math.max(f.value.length, f.want.length) }, (_, i) => {
              const ch = f.value[i] ?? "";
              return ch !== f.want[i] ? <span key={i} className="x">{ch || "·"}</span> : ch;
            })}
          </div>
        )) ?? "loading…"}
      </pre>
      <div className="muted">Two writers updating one record without a lock lose updates and leave half-written fields: the bug class Smriti and paisa prevent in Postgres. <a href="https://github.com/Aravindh97/resume/blob/main/internal/racesim/racesim.go">source</a></div>
    </Card>
  );
}

function Career() {
  const [cmd, setCmd] = useState("log");
  const [out, setOut] = useState("");
  const run = useCallback(async (c: string) => {
    setCmd(c);
    try {
      const r = await api<{ output: string }>("GET", "api/v1/career?cmd=" + encodeURIComponent(c));
      setOut(`$ career ${c}\n${r.output}`);
    } catch (e) {
      setOut(`$ career ${c}\n${(e as Error).message}`);
    }
  }, []);
  useEffect(() => {
    void run("log");
  }, [run]);
  return (
    <Card n="04" title="Career as a git log">
      <form className="row" onSubmit={(e) => { e.preventDefault(); void run(cmd); }}>
        <span className="mono prompt">$ career</span>
        <input className="mono" value={cmd} onChange={(e) => setCmd(e.target.value)} maxLength={80} style={{ flex: 1 }} aria-label="career command" />
        <button type="submit">run</button>
      </form>
      <div className="chips">{["log", "show granth", "cherry-pick skills=go,temporal"].map((c) => <button key={c} className="ghost small mono" onClick={() => run(c)}>{c}</button>)}</div>
      <pre className="term">{out}</pre>
      <div className="muted">Locally: <code>go run github.com/Aravindh97/resume/cmd/career@latest init ./aravindh &amp;&amp; cd aravindh &amp;&amp; git log</code></div>
    </Card>
  );
}

function Experience({ p }: { p: ProfileData }) {
  return (
    <Card wide title="Experience and education">
      {(p.experience ?? []).map((j) => (
        <div key={j.title}>
          <div className="job"><b>{j.title}</b><span className="muted">{j.dates}</span></div>
          <ul>{(j.bullets ?? []).map((b) => <li key={b}>{b}</li>)}</ul>
        </div>
      ))}
      <div>{p.education}</div>
    </Card>
  );
}

export default function App() {
  const [p] = useLoad(() => api<ProfileData>("GET", "api/v1/profile"), []);
  if (!p) return null;
  return (
    <>
      <Hero p={p} />
      <Projects p={p} />
      <h2 className="section wrap">Try the engineering</h2>
      <main>
        <Ask />
        <Mesh />
        <Race />
        <Career />
        <Results />
        <Experience p={p} />
      </main>
      <footer>
        {currentMode() === "static" ? "Everything above is Go compiled to WebAssembly, running in your browser: no server, no tracking." : "Served by the Go server in cmd/site."}
        {" "}React + TypeScript frontend. Personal projects, not affiliated with my employer.
      </footer>
    </>
  );
}
