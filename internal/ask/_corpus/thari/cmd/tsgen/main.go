// Command tsgen writes web/src/types.gen.ts from the Go types the API
// returns. CI runs it and fails if the committed file is out of date.
package main

import (
	"log"
	"os"

	"github.com/Aravindh97/thari/internal/auth"
	"github.com/Aravindh97/thari/internal/domain"
	"github.com/Aravindh97/thari/internal/store"
	"github.com/Aravindh97/thari/internal/tsgen"
)

func main() {
	f, err := os.Create("web/src/types.gen.ts")
	if err != nil {
		log.Fatal(err)
	}
	defer f.Close()
	if err := tsgen.Write(f, "Thari", store.Order{}, store.OrderDetail{}, store.Dashboard{}, store.Buyer{}, store.Event{},
		store.OrderInput{}, store.OrderPatch{}, store.InvalidError{}, auth.User{}, auth.NewUser{}, domain.AQLPlan{}); err != nil {
		log.Fatal(err)
	}
}
