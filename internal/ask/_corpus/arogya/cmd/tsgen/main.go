// Command tsgen writes web/src/types.gen.ts from the Go types the API
// returns. CI runs it and fails if the committed file is out of date.
package main

import (
	"log"
	"os"

	"github.com/Aravindh97/arogya/internal/auth"
	"github.com/Aravindh97/arogya/internal/store"
	"github.com/Aravindh97/arogya/internal/tsgen"
)

func main() {
	f, err := os.Create("web/src/types.gen.ts")
	if err != nil {
		log.Fatal(err)
	}
	defer f.Close()
	if err := tsgen.Write(f, "Arogya", auth.User{}, store.Problem{}, store.Patient{}, store.NewPatient{}, store.Doctor{}, store.Visit{},
		store.Booking{}, store.Queue{}, store.BoardRow{}, store.Chart{}, store.NewEncounter{}, store.AccessEntry{}, store.Consents{},
		store.Ward{}, store.AdmitRequest{}, store.BloodBank{}, store.NewBloodRequest{}, store.DonorMatch{}, store.Message{}); err != nil {
		log.Fatal(err)
	}
}
