package data

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestReferenceIsConvertedFromCET(t *testing.T) {
	p := filepath.Join(t.TempDir(), "ref.csv")
	os.WriteFile(p, []byte("station_id,hour_start_local,pm25,pm10\n1,2026-10-01 01:00:00,7,12\n1,2026-10-01 02:00:00,,\n"), 0o600)
	ref, err := LoadReference(p)
	if err != nil {
		t.Fatal(err)
	}
	// 01:00 CET is 00:00 UTC; the empty hour is skipped.
	want := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	if r, ok := ref["1"][want]; !ok || r.PM25 != 7 || len(ref["1"]) != 1 {
		t.Fatalf("%v", ref)
	}
}

func TestLoadRealData(t *testing.T) {
	rs, bad, err := LoadReadings("../../testdata/lowcost-2026-10.csv.gz")
	if err != nil {
		t.Skip(err)
	}
	if len(rs) < 100000 || bad != 0 {
		t.Fatalf("%d readings, %d bad", len(rs), bad)
	}
	for i := 1; i < len(rs); i++ {
		if rs[i].T.Before(rs[i-1].T) {
			t.Fatal("not sorted")
		}
	}
}
