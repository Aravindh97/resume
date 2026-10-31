package ask

import (
	"strings"
	"testing"
)

func TestSearchFindsTheRightFiles(t *testing.T) {
	ix, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	cases := map[string]string{
		"how does granth avoid duplicate chunks on retry":                  "granth/",
		"what happens when opensearch goes down mid ingest":                "granth/",
		"how does smriti stop two concurrent writers creating two current": "smriti/",
		"how does sutra validate GSTIN checksum":                           "sutra/",
		"logical replication snapshot crash restart checkpoint":            "sutra/",
	}
	for q, repo := range cases {
		res := ix.Search(q, 3)
		if len(res) == 0 || !strings.HasPrefix(res[0].Repo+"/", repo) {
			t.Errorf("%q: top = %+v", q, res)
			continue
		}
		if !strings.Contains(res[0].URL, "#L") || res[0].StartLine < 1 || res[0].EndLine < res[0].StartLine {
			t.Errorf("%q: bad citation %s", q, res[0].URL)
		}
	}
}
