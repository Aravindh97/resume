// Command build-static writes the GitHub Pages version of the site to a
// directory: the same page and assets as the server, with the API answered
// by the WebAssembly module in the visitor's browser.
//
//	go run ./cmd/build-static -out dist
package main

import (
	"flag"
	"fmt"
	"os"

	"github.com/Aravindh97/resume/internal/site"
)

func main() {
	out := flag.String("out", "dist", "output directory")
	flag.Parse()
	n, err := site.WriteStatic(*out)
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
	fmt.Printf("wrote %d files to %s\n", n, *out)
}
