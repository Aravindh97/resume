package resolve

import (
	"testing"

	"github.com/Aravindh97/sutra/internal/normalize"
	"github.com/Aravindh97/sutra/internal/ontology"
)

func gst(state, pan string) string {
	s := state + pan + "1Z"
	return s + string(normalize.GSTINChecksum(s))
}

func rec(src, id string, attrs map[string]string) ontology.Record {
	return ontology.Record{Source: src, ID: id, Entity: "Company", Attrs: attrs}
}

func clusterOf(res Result) map[string]string {
	m := map[string]string{}
	for _, e := range res.Entities {
		for _, k := range e.Members {
			m[k] = e.ID
		}
	}
	return m
}

func TestResolve(t *testing.T) {
	recs := []ontology.Record{
		// Same company, two state registrations: linked by PAN.
		rec("erp", "1", map[string]string{"name": "Kaveri Spinning Mills Pvt Ltd", "gstin": gst("33", "AAACK1234K"), "city": "Tiruppur"}),
		rec("crm", "A", map[string]string{"name": "KAVERI SPINNING MILLS", "gstin": gst("29", "AAACK1234K"), "city": "Bengaluru"}),
		// Transliteration variant + same phone, no GSTIN on the CRM side.
		rec("erp", "2", map[string]string{"name": "Shree Lakshmi Textiles", "gstin": gst("33", "BBBCL5678L"), "phone": "98765 43210", "city": "Erode"}),
		rec("crm", "B", map[string]string{"name": "Sri Laxmi Textiles Private Limited", "phone": "+91-9876543210", "city": "Erode"}),
		// Same trading name, same city, different legal entities.
		rec("erp", "3", map[string]string{"name": "Murugan Textiles", "gstin": gst("33", "CCCFM1111M"), "city": "Salem"}),
		rec("erp", "4", map[string]string{"name": "Murugan Textiles", "gstin": gst("33", "DDDFM2222M"), "city": "Salem"}),
		// Name-only record that could belong to either: must go to review, not be merged.
		rec("crm", "C", map[string]string{"name": "Murugan Textiles & Co", "city": "Salem"}),
	}
	res := Resolve(recs, map[string]int{"erp": 1, "crm": 2}, DefaultConfig())
	c := clusterOf(res)
	if c["erp:1"] != c["crm:A"] {
		t.Error("PAN-linked branches not merged")
	}
	if c["erp:2"] != c["crm:B"] {
		t.Error("transliteration + phone match not merged")
	}
	if c["erp:3"] == c["erp:4"] {
		t.Error("different PANs merged")
	}
	if c["crm:C"] == c["erp:3"] || c["crm:C"] == c["erp:4"] {
		t.Error("ambiguous name-only record was merged")
	}
	if len(res.Review) == 0 {
		t.Error("ambiguous pair should be in review")
	}
	for _, e := range res.Entities {
		if c["erp:1"] == e.ID {
			if e.Golden["name"] != "Kaveri Spinning Mills Pvt Ltd" || e.Golden["pan"] != "AAACK1234K" {
				t.Errorf("golden = %v", e.Golden)
			}
			for _, l := range e.Lineage {
				if l.Attribute == "name" && (l.Source != "erp" || l.RecordID != "1") {
					t.Errorf("name lineage = %+v", l)
				}
			}
		}
	}
}

func TestInvalidGSTINIsNotAKey(t *testing.T) {
	bad := gst("33", "EEECX9999X")
	bad = bad[:14] + "0"
	if _, ok := normalize.GSTIN(bad); ok {
		t.Skip("random checksum happened to be 0")
	}
	a := rec("erp", "1", map[string]string{"name": "Alpha Exports", "gstin": bad})
	b := rec("crm", "1", map[string]string{"name": "Zeta Garments", "gstin": bad})
	if p := Score(a, b); p.Score >= DefaultConfig().Match {
		t.Fatalf("invalid GSTIN used as match key: %+v", p)
	}
}

func TestEntityIDStableWhenRecordsArrive(t *testing.T) {
	base := []ontology.Record{rec("erp", "1", map[string]string{"name": "Kongu Logistics", "gstin": gst("33", "FFFCK3333K")})}
	r1 := Resolve(base, nil, DefaultConfig())
	more := append(base, rec("crm", "Z", map[string]string{"name": "Kongu Logistics Pvt Ltd", "gstin": gst("33", "FFFCK3333K")}))
	cfg := DefaultConfig()
	cfg.Previous = map[string]string{"erp:1": r1.Entities[0].ID}
	r2 := Resolve(more, nil, cfg)
	if len(r2.Entities) != 1 || r1.Entities[0].ID != r2.Entities[0].ID {
		t.Fatalf("id changed: %s -> %+v", r1.Entities[0].ID, r2.Entities)
	}
}
