// Command tsgen writes web/src/types.gen.ts from the Go types the web API
// returns. CI runs it and fails if the committed file is out of date.
package main

import (
	"log"
	"os"

	"github.com/Aravindh97/smriti/internal/eval"
	"github.com/Aravindh97/smriti/internal/store"
	"github.com/Aravindh97/smriti/internal/tsgen"
)

func main() {
	f, err := os.Create("web/src/types.gen.ts")
	if err != nil {
		log.Fatal(err)
	}
	defer f.Close()
	if err := tsgen.Write(f, "smriti", store.WriteResult{}, eval.Report{}); err != nil {
		log.Fatal(err)
	}
}
