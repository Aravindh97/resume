package normalize

import "testing"

func TestGSTIN(t *testing.T) {
	// 27AAPFU0939F1ZV is the sample GSTIN used in GST portal documentation.
	if g, ok := GSTIN("27 aapfu0939f1zv"); !ok || g != "27AAPFU0939F1ZV" {
		t.Fatalf("valid GSTIN rejected: %s %v", g, ok)
	}
	if _, ok := GSTIN("27AAPFU0939F1Z0"); ok {
		t.Fatal("bad checksum accepted")
	}
	if p := PANFromGSTIN("27AAPFU0939F1ZV"); p != "AAPFU0939F" {
		t.Fatalf("pan = %s", p)
	}
}

func TestPhone(t *testing.T) {
	for in, want := range map[string]string{"+91 98765 43210": "+919876543210", "098765-43210": "+919876543210",
		"9876543210": "+919876543210", "0421 2345678": "04212345678", "123": ""} {
		if got := Phone(in); got != want {
			t.Errorf("%q -> %q, want %q", in, got, want)
		}
	}
}

func TestNameSimilarity(t *testing.T) {
	same := [][2]string{
		{"Shree Lakshmi Textiles Pvt Ltd", "Sri Laxmi Textiles"},
		{"Thirumalai Knit Fabrics", "TIRUMALAI KNIT FABRICS PRIVATE LIMITED"},
		{"Vinayaka Packaging", "Vinayaka Packaging Private Limitted"},
		{"Kongu Logistics", "Kongu Logistics PvtL td"},
	}
	for _, p := range same {
		if s := NameSimilarity(p[0], p[1]); s < 0.94 {
			t.Errorf("%q vs %q = %.3f, want >= 0.94", p[0], p[1], s)
		}
	}
	diff := [][2]string{
		{"Lakshmi Knit Fabrics", "Selvam Knit Fabrics LLP"},
		{"Amman Processors", "Amman Printers"},
	}
	for _, p := range diff {
		if s := NameSimilarity(p[0], p[1]); s >= 0.94 {
			t.Errorf("%q vs %q = %.3f, want < 0.94", p[0], p[1], s)
		}
	}
	if EmailDomain("a@gmail.com") != "" || EmailDomain("ap@kaveri.in") != "kaveri.in" {
		t.Error("email domain")
	}
}
