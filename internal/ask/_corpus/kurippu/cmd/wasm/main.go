//go:build js && wasm

// Command wasm exposes the notes CRDT to the browser as window.kurippu, so
// the browser merges edits with exactly the same code as the server.
// Values cross the boundary as JSON strings.
package main

import (
	"encoding/json"
	"syscall/js"

	"github.com/Aravindh97/kurippu/internal/crdt"
)

var doc = crdt.New("unset")

func j(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}

// fn wraps a function so a Go error becomes {"error": "..."} instead of a crash.
func fn(f func(a []js.Value) (any, error)) js.Func {
	return js.FuncOf(func(_ js.Value, a []js.Value) (out any) {
		defer func() {
			if p := recover(); p != nil {
				out = j(map[string]any{"error": "internal error"})
			}
		}()
		v, err := f(a)
		if err != nil {
			return j(map[string]any{"error": err.Error()})
		}
		return j(v)
	})
}

func main() {
	api := map[string]any{
		"init": fn(func(a []js.Value) (any, error) {
			doc = crdt.New(a[0].String())
			doc.SetSeq(uint64(a[1].Int()))
			return true, nil
		}),
		// apply(opsJSON) -> number of ops that changed something
		"apply": fn(func(a []js.Value) (any, error) {
			var ops []crdt.Op
			if err := json.Unmarshal([]byte(a[0].String()), &ops); err != nil {
				return nil, err
			}
			n := 0
			for _, o := range ops {
				changed, err := doc.Apply(o)
				if err != nil {
					return nil, err
				}
				if changed {
					n++
				}
			}
			return n, nil
		}),
		"create": fn(func(a []js.Value) (any, error) { return doc.CreateNote(a[0].String()) }),
		"title":  fn(func(a []js.Value) (any, error) { return doc.SetTitle(a[0].String(), a[1].String()) }),
		"trash":  fn(func(a []js.Value) (any, error) { return doc.Trash(a[0].String(), a[1].Bool()) }),
		"edit":   fn(func(a []js.Value) (any, error) { return doc.Edit(a[0].String(), a[1].String()) }),
		"text":   fn(func(a []js.Value) (any, error) { return doc.Text(a[0].String()), nil }),
		"notes":  fn(func(a []js.Value) (any, error) { return doc.Notes(), nil }),
		"anchor": fn(func(a []js.Value) (any, error) { return doc.Anchor(a[0].String(), a[1].Int()), nil }),
		"offset": fn(func(a []js.Value) (any, error) {
			var id crdt.ID
			json.Unmarshal([]byte(a[1].String()), &id)
			return doc.Offset(a[0].String(), id), nil
		}),
		"waiting": fn(func(a []js.Value) (any, error) { return doc.Waiting(), nil }),
	}
	obj := js.Global().Get("Object").New()
	for k, v := range api {
		obj.Set(k, v)
	}
	js.Global().Set("kurippu", obj)
	select {}
}
