package recon

import (
	"strings"
	"testing"
)

func TestReadCSV(t *testing.T) {
	ls, err := ReadCSV(strings.NewReader("reference,amount_paise,merchant\nUPI1,9900,merchant:001\nUPI2,500,merchant:002\n"))
	if err != nil || len(ls) != 2 || ls[0].Amount != 9900 || ls[1].Merchant != "merchant:002" {
		t.Fatalf("%+v %v", ls, err)
	}
	if _, err := ReadCSV(strings.NewReader("h,h,h\nUPI1,ten,m\n")); err == nil {
		t.Fatal("bad amount accepted")
	}
}
