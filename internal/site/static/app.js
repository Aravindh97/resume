"use strict";
const $ = (id) => document.getElementById(id);
const esc = (s) => String(s).replace(/[&<>"']/g, (c) => ({"&":"&amp;","<":"&lt;",">":"&gt;",'"':"&quot;","'":"&#39;"}[c]));
async function api(path, opts) {
  const r = await fetch(path, opts);
  const j = await r.json().catch(() => ({}));
  if (!r.ok) throw new Error(j.error || ("HTTP " + r.status));
  return j;
}

// ---------- 01 ask ----------
const examples = [
  "How does Granth avoid duplicate chunks on retry?",
  "What happens when OpenSearch goes down?",
  "How does Smriti stop two writers creating two current facts?",
  "How does Sutra validate a GSTIN?",
  "What does the CDC connector do after a crash?",
];
for (const q of examples) {
  const b = document.createElement("button"); b.className = "ghost"; b.textContent = q;
  b.onclick = () => { $("q").value = q; ask(q); }; $("chips").appendChild(b);
}
$("askf").onsubmit = (e) => { e.preventDefault(); ask($("q").value); };
async function ask(q) {
  $("ans").innerHTML = '<div class="muted">searching…</div>';
  try {
    const r = await api("/api/v1/ask?q=" + encodeURIComponent(q));
    let h = r.low_confidence ? '<div class="warn">No strong match in the repos; these are the closest passages.</div>' : "";
    for (const p of r.passages) {
      h += `<div class="passage"><div class="meta mono"><a href="${esc(p.url)}" target="_blank" rel="noopener">${esc(p.repo)}/${esc(p.file)}:${p.start_line}-${p.end_line}</a> · ${esc(p.title)} · score ${p.score}</div><pre>${esc(p.text)}</pre></div>`;
    }
    h += `<div class="muted">BM25 over ${"repo"} passages · ${r.took_ms} ms · served by ${esc(r.served_by)}</div>`;
    $("ans").innerHTML = h;
  } catch (e) { $("ans").innerHTML = `<div class="warn">${esc(e.message)}</div>`; }
}

// ---------- 02 mesh ----------
let ok = 0, failed = 0, lastLeader = null, lastTerm = null, lastVer = null, nodes = [];
function mlog(s) { const t = new Date().toLocaleTimeString(); $("mlog").insertAdjacentHTML("beforeend", `<div>[${t}] ${s}</div>`); $("mlog").scrollTop = 1e9; }
async function poll() {
  try {
    const r = await api("/api/v1/cluster"); ok++;
    nodes = r.nodes;
    const leader = nodes.find((n) => n.role === "leader" && !n.isolated);
    const term = Math.max(...nodes.map((n) => n.term || 0));
    const ver = Math.max(...nodes.map((n) => n.version || 0));
    if (leader && leader.id !== lastLeader) mlog(`leader is <b>${esc(leader.id)}</b> (${esc(leader.region)}), term ${leader.term}`);
    if (!leader && lastLeader) mlog("no leader: election in progress");
    if (lastTerm !== null && term > lastTerm) mlog(`term ${lastTerm} → ${term}`);
    if (lastVer !== null && ver > lastVer) mlog(`resume v${ver} committed on a majority`);
    lastLeader = leader ? leader.id : null; lastTerm = term; lastVer = ver;
    $("term").textContent = term; $("leader").textContent = leader ? leader.id : "electing…"; $("ver").textContent = "v" + ver;
    $("ncount").textContent = nodes.length;
    $("nodes").innerHTML = nodes.map((n) => {
      const down = n.isolated || !n.up;
      const cls = "node" + (down ? " down" : "") + (n.role === "leader" && !down ? " leader" : "");
      const state = !n.up ? "unreachable" : n.isolated ? "isolated (partition)" : `${n.role} · v${n.version} · commit ${n.commit_index}`;
      return `<div class="${cls}"><div class="tag">${n.role === "leader" && !down ? "LEADER" : ""}</div><span class="dot"></span><div><b>${esc(n.region || n.id)}</b></div><div class="muted mono">${esc(n.id)}</div><div class="muted">${esc(state)}</div></div>`;
    }).join("");
  } catch (e) { failed++; }
  $("okc").textContent = ok; $("failc").textContent = failed;
}
setInterval(poll, 700); poll();
async function chaos(target) {
  try { const r = await api("/api/v1/cluster/chaos", {method: "POST", headers: {"Content-Type": "application/json"}, body: JSON.stringify({target})});
    mlog(`<span style="color:#ff8a8a">✖ isolated ${esc(r.isolated)} for ${r.for_s}s</span>`);
  } catch (e) { mlog(`<span style="color:#f5c26b">${esc(e.message)}</span>`); }
}
document.querySelector("[data-chaos=leader]").onclick = () => chaos("leader");
$("followerBtn").onclick = () => {
  const f = nodes.filter((n) => n.role === "follower" && !n.isolated && n.up);
  if (!f.length) return mlog("no healthy follower to isolate");
  chaos(f[Math.floor(Math.random() * f.length)].id);
};
$("writeBtn").onclick = async () => {
  try { const r = await api("/api/v1/cluster/write", {method: "POST"}); mlog(`update proposed to leader ${esc(r.committed_by)} and committed`); }
  catch (e) { mlog(`<span style="color:#f5c26b">${esc(e.message)}</span>`); }
};

// ---------- 03 race simulator (Go WASM) ----------
let mode = "none", want = 0, running = false;
$("traffic").oninput = () => { $("tv").textContent = $("traffic").value; };
document.querySelectorAll("[data-mode]").forEach((b) => b.onclick = () => {
  document.querySelectorAll("[data-mode]").forEach((x) => x.classList.remove("on")); b.classList.add("on"); mode = b.dataset.mode;
  if (window.racesimReset) { racesimReset(); want = 0; }
});
$("resetRace").onclick = () => { if (window.racesimReset) { racesimReset(); want = 0; } };
function render(s) {
  let torn = 0;
  $("race").innerHTML = s.fields.map((f) => {
    if (f.torn) torn++;
    let out = "";
    for (let i = 0; i < Math.max(f.value.length, f.want.length); i++) {
      const c = f.value[i] || ""; out += c !== f.want[i] ? `<span class="x">${esc(c || "·")}</span>` : esc(c);
    }
    return f.name.padEnd(6) + ": " + out;
  }).join("\n");
  $("want").textContent = want; $("views").textContent = s.views; $("lost").textContent = want - s.views; $("torn").textContent = torn;
}
(async () => {
  try {
    const go = new Go();
    const res = await WebAssembly.instantiateStreaming(fetch("/static/racesim.wasm"), go.importObject);
    go.run(res.instance);
    setInterval(() => {
      if (!window.racesimStep || running) return;
      running = true;
      const w = +$("traffic").value; want += w;
      render(JSON.parse(racesimStep(mode, w)));
      running = false;
    }, 300);
  } catch (e) { $("race").textContent = "could not load WebAssembly: " + e.message; }
})();

// ---------- 04 career ----------
async function git(cmd) {
  $("gitcmd").value = cmd; $("term1").textContent = "$ career " + cmd + "\n";
  try { const r = await api("/api/v1/career?cmd=" + encodeURIComponent(cmd)); $("term1").textContent += r.output; }
  catch (e) { $("term1").textContent += e.message; }
}
$("gitf").onsubmit = (e) => { e.preventDefault(); git($("gitcmd").value); };
document.querySelectorAll("[data-git]").forEach((b) => b.onclick = () => git(b.dataset.git));
git("log");
