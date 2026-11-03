// Command tsgen writes web/src/types.gen.ts from the Go types the web API
// returns. CI runs it and fails if the committed file is out of date.
package main

import (
	"log"
	"os"

	"github.com/Aravindh97/sutra/internal/eval"
	"github.com/Aravindh97/sutra/internal/resolve"
	"github.com/Aravindh97/sutra/internal/tsgen"
)

func main() {
	f, err := os.Create("web/src/types.gen.ts")
	if err != nil {
		log.Fatal(err)
	}
	defer f.Close()
	if err := tsgen.Write(f, "sutra", resolve.Lineage{}, resolve.Pair{}, eval.Metrics{}); err != nil {
		log.Fatal(err)
	}
}
