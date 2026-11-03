// Command tsgen writes web/src/types.gen.ts from the Go types the web API
// returns. CI runs it and fails if the committed file is out of date.
package main

import (
	"log"
	"os"

	"github.com/Aravindh97/fairdrop/internal/lottery"
	"github.com/Aravindh97/fairdrop/internal/sim"
	"github.com/Aravindh97/fairdrop/internal/store"
	"github.com/Aravindh97/fairdrop/internal/tsgen"
)

func main() {
	f, err := os.Create("web/src/types.gen.ts")
	if err != nil {
		log.Fatal(err)
	}
	defer f.Close()
	if err := tsgen.Write(f, "fairdrop", sim.Report{}, store.SeatState{}, lottery.Commitment{}, lottery.Entry{}); err != nil {
		log.Fatal(err)
	}
}
