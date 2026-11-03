// Command tsgen writes web/src/types.gen.ts from the Go types the site's
// API returns. CI runs it and fails if the committed file is out of date.
package main

import (
	"log"
	"os"

	"github.com/Aravindh97/resume/internal/ask"
	"github.com/Aravindh97/resume/internal/browsermesh"
	"github.com/Aravindh97/resume/internal/profile"
	"github.com/Aravindh97/resume/internal/racesim"
	"github.com/Aravindh97/resume/internal/tsgen"
)

func main() {
	f, err := os.Create("web/src/types.gen.ts")
	if err != nil {
		log.Fatal(err)
	}
	defer f.Close()
	if err := tsgen.Write(f, "the resume site", profile.ProfileData{}, profile.Eval{}, ask.Passage{},
		browsermesh.NodeStatus{}, racesim.Snapshot{}); err != nil {
		log.Fatal(err)
	}
}
