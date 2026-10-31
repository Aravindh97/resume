// Command career prints Aravindh Varadharaj's resume as a commit history.
//
//	go run github.com/Aravindh97/resume/cmd/career@latest log
package main

import (
	"fmt"
	"os"

	"github.com/Aravindh97/resume/internal/career"
)

func main() {
	args := os.Args[1:]
	if len(args) > 0 && args[0] == "init" {
		if len(args) < 2 {
			fmt.Fprintln(os.Stderr, "usage: career init <dir>")
			os.Exit(2)
		}
		if err := career.Init(args[1]); err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
			os.Exit(1)
		}
		fmt.Printf("wrote %d commits to %s\n  cd %s && git log --stat\n", len(career.Milestones), args[1], args[1])
		return
	}
	out, err := career.Run(args)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	fmt.Print(out)
}
