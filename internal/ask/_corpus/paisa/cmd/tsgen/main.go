// Command tsgen writes web/src/types.gen.ts from the Go types the web API
// returns. CI runs it and fails if the committed file is out of date.
package main

import (
	"log"
	"os"

	"github.com/Aravindh97/paisa/internal/ledger"
	"github.com/Aravindh97/paisa/internal/recon"
	"github.com/Aravindh97/paisa/internal/tsgen"
)

func main() {
	f, err := os.Create("web/src/types.gen.ts")
	if err != nil {
		log.Fatal(err)
	}
	defer f.Close()
	if err := tsgen.Write(f, "paisa", ledger.Audit{}, ledger.Posting{}, recon.Break{}); err != nil {
		log.Fatal(err)
	}
}
