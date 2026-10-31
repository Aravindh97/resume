//go:build js && wasm

// Command racesim-wasm exposes the race simulator to the browser:
//
//	racesimStep(mode, writers) -> JSON snapshot
//	racesimReset()
package main

import (
	"encoding/json"
	"math/rand/v2"
	"syscall/js"

	"github.com/Aravindh97/resume/internal/racesim"
)

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
	js.Global().Set("racesimReady", true)
	select {}
}
