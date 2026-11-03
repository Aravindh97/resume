// Command pallanguzhi plays the game in a terminal or benchmarks the AI.
//
//	pallanguzhi play [-level hard] [-first]
//	pallanguzhi bench [-games 200]
package main

import (
	"bufio"
	"flag"
	"fmt"
	"math/rand/v2"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/Aravindh97/pallanguzhi/internal/ai"
	"github.com/Aravindh97/pallanguzhi/internal/bench"
	"github.com/Aravindh97/pallanguzhi/internal/game"
)

var levels = map[string]ai.Level{"random": ai.Random, "easy": ai.Easy, "medium": ai.Medium, "hard": ai.Hard}

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: pallanguzhi play|bench [flags]")
		os.Exit(2)
	}
	switch os.Args[1] {
	case "play":
		play(os.Args[2:])
	case "bench":
		runBench(os.Args[2:])
	default:
		fmt.Fprintln(os.Stderr, "unknown command", os.Args[1])
		os.Exit(2)
	}
}

func runBench(args []string) {
	fs := flag.NewFlagSet("bench", flag.ExitOnError)
	n := fs.Int("games", 200, "games per pairing and seat")
	fs.Parse(args)
	t := time.Now()
	var rs []bench.Result
	pairs := [][2]ai.Level{{ai.Random, ai.Random}, {ai.Easy, ai.Random}, {ai.Medium, ai.Random}, {ai.Hard, ai.Random},
		{ai.Medium, ai.Easy}, {ai.Hard, ai.Easy}, {ai.Hard, ai.Medium}}
	for i, p := range pairs {
		for seat := 0; seat < 2; seat++ {
			rs = append(rs, bench.Play(p[0], p[1], seat, *n, uint64(100*i+seat)))
		}
	}
	bench.Print(os.Stdout, rs)
	fmt.Printf("\n%d games in %s. Games between medium and hard start with %d random plies; all others from the start position.\n",
		len(rs)**n, time.Since(t).Round(time.Second), bench.Opening)
	b := game.New()
	sc, st := ai.Analyse(b, 12)
	fmt.Printf("start position searched to depth 12 (%d nodes): South's best move is pit %d, value %+d seeds for South\n", st.Nodes, sc[0].Pit+1, sc[0].Score)
}

func show(b game.Board) {
	fmt.Print("\n        ")
	for i := 13; i >= 7; i-- {
		fmt.Printf("%4d", b.Pits[i])
	}
	fmt.Printf("    North store %d\n        ", b.Store[1])
	for i := 0; i < 7; i++ {
		fmt.Printf("%4d", b.Pits[i])
	}
	fmt.Printf("    South store %d\n  pits:  ", b.Store[0])
	for i := 1; i <= 7; i++ {
		fmt.Printf("%4d", i)
	}
	fmt.Println()
}

func play(args []string) {
	fs := flag.NewFlagSet("play", flag.ExitOnError)
	lv := fs.String("level", "medium", "random|easy|medium|hard")
	first := fs.Bool("first", false, "you move first (the first player is at a disadvantage)")
	fs.Parse(args)
	level, ok := levels[*lv]
	if !ok {
		fmt.Fprintln(os.Stderr, "unknown level", *lv)
		os.Exit(2)
	}
	r := rand.New(rand.NewPCG(uint64(time.Now().UnixNano()), 1))
	b := game.New()
	you := game.North
	if *first {
		you = game.South
	}
	in := bufio.NewScanner(os.Stdin)
	for !b.Over() {
		show(b)
		var m int
		if b.Turn == you {
			fmt.Print("your pit (1-7): ")
			if !in.Scan() {
				return
			}
			k, err := strconv.Atoi(strings.TrimSpace(in.Text()))
			m = k - 1 + you*game.PerSide
			if err != nil || b.Play(m, nil) != nil {
				fmt.Println("not a legal pit")
				continue
			}
			continue
		}
		m, _, _ = ai.Choose(b, level, r)
		fmt.Printf("AI lifts pit %d\n", m-(1-you)*game.PerSide+1)
		b.Play(m, nil)
	}
	show(b)
	f := b.Final()
	fmt.Printf("final: you %d, AI %d\n", f[you], f[1-you])
}
