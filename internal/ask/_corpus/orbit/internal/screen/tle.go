package screen

import (
	"bufio"
	"io"
	"strconv"
	"strings"

	"github.com/Aravindh97/orbit/internal/sgp4"
)

// LoadTLE reads 3-line element sets (name, line 1, line 2). Records that do
// not parse or initialise are skipped and counted. Duplicate catalogue
// numbers keep the first occurrence.
func LoadTLE(r io.Reader) (objs []Object, skipped int, err error) {
	sc := bufio.NewScanner(r)
	var lines []string
	for sc.Scan() {
		if l := strings.TrimRight(sc.Text(), "\r "); l != "" {
			lines = append(lines, l)
		}
	}
	if err := sc.Err(); err != nil {
		return nil, 0, err
	}
	seen := map[int]bool{}
	for i := 0; i+2 < len(lines)+0; {
		name, l1, l2 := lines[i], lines[i+1], lines[i+2]
		if !strings.HasPrefix(l1, "1 ") || !strings.HasPrefix(l2, "2 ") {
			i++ // resynchronise on malformed input
			skipped++
			continue
		}
		i += 3
		s, err := sgp4.New(l1, l2)
		if err != nil {
			skipped++
			continue
		}
		if seen[s.NoradID()] {
			continue
		}
		seen[s.NoradID()] = true
		mm, _ := strconv.ParseFloat(strings.TrimSpace(l2[52:63]), 64)
		ecc, _ := strconv.ParseFloat("0."+strings.TrimSpace(l2[26:33]), 64)
		per, apo := Shell(mm, ecc)
		objs = append(objs, Object{ID: s.NoradID(), Name: strings.TrimSpace(strings.TrimPrefix(name, "0 ")), Sat: *s, Perigee: per, Apogee: apo})
	}
	return objs, skipped, nil
}
