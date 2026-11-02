//go:build js && wasm

package main

import (
	"encoding/json"
	"net/url"
	"strings"
	"syscall/js"
)

// resp is what goapi returns to JavaScript.
type resp struct {
	Status int `json:"status"`
	Body   any `json:"body"`
}

func ok(v any) resp                  { return resp{200, v} }
func fail(code int, msg string) resp { return resp{code, map[string]string{"error": msg}} }

type handler func(q url.Values, body []byte) resp

// serve exposes goapi(method, path, body, callback) to JavaScript. Each call
// runs in its own goroutine so long computations never block the page's
// event loop, and a panic becomes an error response instead of killing the
// module.
func serve(routes map[string]handler) {
	js.Global().Set("goapi", js.FuncOf(func(_ js.Value, a []js.Value) any {
		method, path, body, cb := a[0].String(), a[1].String(), a[2].String(), a[3]
		go func() {
			var r resp
			defer func() {
				if p := recover(); p != nil {
					r = fail(500, "internal error")
				}
				b, err := json.Marshal(r)
				if err != nil {
					b, _ = json.Marshal(fail(500, err.Error()))
				}
				cb.Invoke(string(b))
			}()
			u, err := url.Parse(path)
			if err != nil {
				r = fail(400, "bad path")
				return
			}
			h, found := routes[method+" "+strings.TrimPrefix(u.Path, "/")]
			if !found {
				r = fail(404, "not found")
				return
			}
			r = h(u.Query(), []byte(body))
		}()
		return nil
	}))
	select {}
}
