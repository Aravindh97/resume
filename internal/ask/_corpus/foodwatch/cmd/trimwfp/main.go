// Command trimwfp keeps the WFP rows foodwatch's signals use (retail cereals
// and goats), parsed as real CSV: category names contain commas.
//
//	go run ./cmd/trimwfp < testdata/wfp_food_prices_som.csv > cmd/web/embed/wfp.csv
package main

import (
	"encoding/csv"
	"fmt"
	"io"
	"os"
)

var keep = map[string]bool{"Sorghum (red)": true, "Maize (white)": true, "Sorghum (white)": true, "Livestock (Goat)": true}

func main() {
	r, w := csv.NewReader(os.Stdin), csv.NewWriter(os.Stdout)
	hdr, err := r.Read()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	col := map[string]int{}
	for i, h := range hdr {
		col[h] = i
	}
	w.Write(hdr)
	for {
		rec, err := r.Read()
		if err == io.EOF {
			break
		}
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		if rec[col["pricetype"]] == "Retail" && keep[rec[col["commodity"]]] {
			w.Write(rec)
		}
	}
	w.Flush()
	if err := w.Error(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
