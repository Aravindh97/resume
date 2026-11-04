package analyse

import (
	"strings"
	"testing"
	"time"
)

// The sample's rows repeated 12 times: about 1,500 rows, a busy year. The
// browser timing in the README uses the same file.
func BenchmarkLargeStatement(b *testing.B) {
	var head string
	var rows []string
	for _, l := range strings.Split(SampleCSV(7, time.Date(2026, 11, 4, 0, 0, 0, 0, time.UTC)), "\n") {
		switch {
		case strings.HasPrefix(l, "Date,"):
			head = l
		case len(l) > 2 && l[2] == '/':
			rows = append(rows, l)
		}
	}
	csv := head + "\n" + strings.Repeat(strings.Join(rows, "\n")+"\n", 12)
	p, err := Parse(strings.NewReader(csv))
	if err != nil {
		b.Fatal(err)
	}
	b.ReportMetric(float64(len(p.Txns)), "rows")
	for b.Loop() {
		p, _ := Parse(strings.NewReader(csv))
		Analyse(p.Txns, nil)
	}
}
