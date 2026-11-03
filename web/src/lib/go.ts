// The site runs in two modes with one frontend:
//   server: the Go server (cmd/site) answers /api/v1/* over HTTP.
//   static: on GitHub Pages there is no server, so the same routes are
//           answered by cmd/app-wasm, the Go code compiled to WebAssembly.
// Both modes load the WebAssembly module, which also runs the race simulator.

type GoAPI = (method: string, path: string, body: string, done: (raw: string) => void) => void;
declare global {
  interface Window {
    goapi?: GoAPI;
    goReady?: boolean;
    STATIC_SITE?: boolean;
    racesimStep?: (mode: string, writers: number) => string;
    racesimReset?: () => void;
    Go: new () => { importObject: WebAssembly.Imports; run(i: WebAssembly.Instance): Promise<void> };
  }
}

export type Mode = "server" | "static";
export class ApiError extends Error {
  status: number;
  constructor(status: number, message: string) {
    super(message);
    this.status = status;
  }
}
export type Transport = (method: string, path: string, body: string) => Promise<{ status: number; body: unknown }>;

let started: Promise<Mode> | null = null;
let mode: Mode = "static";

const sleep = (ms: number) => new Promise((r) => setTimeout(r, ms));

async function isServer() {
  try {
    const r = await fetch("api/v1/profile", { headers: { accept: "application/json" } });
    return r.ok && (r.headers.get("content-type") ?? "").includes("json");
  } catch {
    return false;
  }
}

/** Detects the mode and starts the WebAssembly module once. */
export function loadGo(): Promise<Mode> {
  started ??= (async () => {
    mode = (await isServer()) ? "server" : "static";
    if (mode === "static") window.STATIC_SITE = true; // cmd/app-wasm registers goapi only then
    const go = new window.Go();
    const url = "static/app.wasm";
    let inst: WebAssembly.Instance;
    try {
      inst = (await WebAssembly.instantiateStreaming(fetch(url), go.importObject)).instance;
    } catch {
      inst = (await WebAssembly.instantiate(await (await fetch(url)).arrayBuffer(), go.importObject)).instance;
    }
    void go.run(inst);
    while (!window.goReady) await sleep(10);
    return mode;
  })();
  return started;
}

export const currentMode = () => mode;

let transport: Transport = async (method, path, body) => {
  await loadGo();
  if (mode === "static") {
    const raw = await new Promise<string>((res) => window.goapi!(method, path, body, res));
    return JSON.parse(raw);
  }
  const r = await fetch(path, { method, body: method === "GET" ? undefined : body, headers: { "Content-Type": "application/json" } });
  return { status: r.status, body: await r.json().catch(() => ({})) };
};

export function setTransport(t: Transport, m: Mode = "static") {
  transport = t;
  mode = m;
  started = Promise.resolve(m);
}

export async function api<T>(method: "GET" | "POST", path: string, body?: unknown): Promise<T> {
  const r = await transport(method, path, body === undefined ? "" : JSON.stringify(body));
  if (r.status >= 300) throw new ApiError(r.status, (r.body as { error?: string } | null)?.error ?? `HTTP ${r.status}`);
  return r.body as T;
}
