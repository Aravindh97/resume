// Command tsgen writes web/src/types.gen.ts from the Go types the API
// returns. CI runs it and fails if the committed file is out of date.
package main

import (
	"log"
	"os"

	"github.com/Aravindh97/kaasu/internal/analyse"
	"github.com/Aravindh97/kaasu/internal/auth"
	"github.com/Aravindh97/kaasu/internal/store"
	"github.com/Aravindh97/kaasu/internal/tsgen"
)

func main() {
	f, err := os.Create("web/src/types.gen.ts")
	if err != nil {
		log.Fatal(err)
	}
	defer f.Close()
	if err := tsgen.Write(f, "Kaasu", auth.User{}, store.Problem{}, store.Wallet{}, store.Moved{}, store.Activity{}, store.Payee{}, store.Send{}, store.TopUp{},
		store.MoneyRequest{}, store.Requests{}, store.NewRequest{}, store.NewSplit{}, store.Split{}, store.Budgets{}, store.SetBudget{}, store.Goal{}, store.NewGoal{},
		store.Import{}, store.DeviceImport{}, store.Teach{}, store.Report{}, store.PausedAccount{}, store.AuditEntry{}, store.LedgerCheck{}, store.DemoUser{},
		analyse.Summary{}); err != nil {
		log.Fatal(err)
	}
}
