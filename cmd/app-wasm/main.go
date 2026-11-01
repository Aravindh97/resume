//go:build js && wasm

// Command app-wasm is the browser half of the resume.
//
// It always exposes the race simulator (racesimStep, racesimReset). For the
// static GitHub Pages build it also exposes goapi(method, path, body), which
// answers the same /api/v1/* routes the server does, entirely in the tab:
// search over the embedded repo snapshot, the career CLI, and a 4-node Raft
// mesh over an in-memory network.
package main

import (
	"context"
	"encoding/json"
	"math/rand/v2"
	"net/url"
	"strings"
	"syscall/js"
	"time"

	"github.com/Aravindh97/resume/internal/ask"
	"github.com/Aravindh97/resume/internal/browsermesh"
	"github.com/Aravindh97/resume/internal/career"
	"github.com/Aravindh97/resume/internal/profile"
	"github.com/Aravindh97/resume/internal/racesim"
)

type resp struct {
	Status int `json:"status"`
	Body   any `json:"body"`
}

func errBody(code int, msg string) resp { return resp{code, map[string]string{"error": msg}} }

func main() {
	rec := racesim.NewRecord(racesim.DefaultFields)
	js.Global().Set("racesimStep", js.FuncOf(func(_ js.Value, args []js.Value) any {
		mode := racesim.Mode(args[0].String())
		writers := min(max(args[1].Int(), 1), 256)
		rec.Step(mode, writers, func(int) (int, int) {
			return rand.IntN(len(racesim.DefaultFields)), rand.IntN(2)
		})
		b, _ := json.Marshal(rec.Snapshot())
		return string(b)
	}))
	js.Global().Set("racesimReset", js.FuncOf(func(js.Value, []js.Value) any { rec.Reset(); return nil }))

	if js.Global().Get("STATIC_SITE").Truthy() {
		ctx := context.Background()
		ix, err := ask.Load()
		mesh := browsermesh.New(ctx, 15*time.Second)
		var lastChaos time.Time
		route := func(method, path, body string) resp {
			u, _ := url.Parse(path)
			q := u.Query()
			var in struct {
				Target string `json:"target"`
			}
			if body != "" {
				json.Unmarshal([]byte(body), &in)
			}
			switch method + " " + strings.TrimPrefix(u.Path, "/") {
			case "GET api/v1/ask":
				if err != nil {
					return errBody(500, err.Error())
				}
				s := q.Get("q")
				if len(s) < 3 || len(s) > 300 {
					return errBody(400, "q must be 3-300 characters")
				}
				t := time.Now()
				res := ix.Search(s, 3)
				return resp{200, map[string]any{"query": s, "passages": res, "low_confidence": len(res) == 0 || res[0].Score < 6,
					"took_ms": float64(time.Since(t).Microseconds()) / 1000, "served_by": "your browser (WebAssembly)"}}
			case "GET api/v1/career":
				args := strings.Fields(q.Get("cmd"))
				if len(args) > 0 && args[0] == "init" {
					return errBody(400, "run `career init` locally: go run github.com/Aravindh97/resume/cmd/career@latest init ./aravindh")
				}
				out, err := career.Run(args)
				if err != nil {
					return errBody(400, err.Error())
				}
				return resp{200, map[string]string{"output": out}}
			case "GET api/v1/cluster":
				return resp{200, map[string]any{"served_by": "browser", "nodes": mesh.Status()}}
			case "POST api/v1/cluster/chaos":
				if since := time.Since(lastChaos); since < 5*time.Second {
					return errBody(429, "chaos cooling down for a few seconds")
				}
				target := "leader"
				if in.Target != "" {
					target = in.Target
				}
				id, err := mesh.Isolate(target)
				if err != nil {
					return errBody(409, err.Error())
				}
				lastChaos = time.Now()
				return resp{200, map[string]any{"isolated": id, "for_s": 15}}
			case "POST api/v1/cluster/write":
				id, err := mesh.Write(ctx)
				if err != nil {
					return errBody(503, err.Error())
				}
				return resp{200, map[string]string{"committed_by": id}}
			case "GET api/v1/profile":
				return resp{200, profile.Profile}
			case "GET api/v1/evals":
				return resp{200, profile.Evals}
			}
			return errBody(404, "not found")
		}
		// goapi(method, path, body, resolve) runs off the JS event loop so blocking
		// calls (Raft proposals) do not freeze the page.
		js.Global().Set("goapi", js.FuncOf(func(_ js.Value, a []js.Value) any {
			method, path, body, cb := a[0].String(), a[1].String(), a[2].String(), a[3]
			go func() {
				b, _ := json.Marshal(route(method, path, body))
				cb.Invoke(string(b))
			}()
			return nil
		}))
	}
	js.Global().Set("goReady", true)
	select {}
}
