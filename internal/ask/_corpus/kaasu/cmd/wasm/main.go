//go:build js && wasm

// Command wasm is the statement analyser compiled to WebAssembly, for
// privacy mode: the browser parses and analyses the file itself, and only
// the summary is sent to the server. It is the same Go code the server runs.
//
//	GOOS=js GOARCH=wasm go build -o web/public/analyse.wasm ./cmd/wasm
package main

import (
	"encoding/json"
	"strings"
	"syscall/js"

	"github.com/Aravindh97/kaasu/internal/analyse"
)

type result struct {
	Error   string          `json:"error,omitempty"`
	Txns    []analyse.Txn   `json:"txns"`
	Skipped []analyse.Skip  `json:"skipped"`
	Columns string          `json:"columns"`
	Summary analyse.Summary `json:"summary"`
}

// kaasuAnalyse(csvText, rulesJSON) returns a JSON string.
func run(this js.Value, args []js.Value) any {
	var out result
	if len(args) != 2 {
		out.Error = "kaasuAnalyse needs the file text and the rules"
	} else if p, err := analyse.Parse(strings.NewReader(args[0].String())); err != nil {
		out.Error = err.Error()
	} else {
		rules := map[string]string{}
		json.Unmarshal([]byte(args[1].String()), &rules)
		out.Summary = analyse.Analyse(p.Txns, rules)
		out.Txns, out.Skipped, out.Columns = p.Txns, p.Skipped, p.Columns
	}
	b, _ := json.Marshal(out)
	return string(b)
}

func main() {
	js.Global().Set("kaasuAnalyse", js.FuncOf(run))
	js.Global().Call("dispatchEvent", js.Global().Get("Event").New("kaasu-analyser-ready"))
	select {}
}
