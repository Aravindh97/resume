package store

import (
	"fmt"
	"sort"
)

// SeatPos is a seat's place in the hall.
type SeatPos struct {
	ID  string
	Row string
	Num int
}

// RowLabel is A, B, ... Z, AA, AB, ...
func RowLabel(i int) string { return rowLabel(i) }

// SeatID is the id used for row r (0-based) and seat number n (1-based).
func SeatID(r, n int) string { return fmt.Sprintf("%s%d", rowLabel(r), n) }

// Candidates takes the free seats sorted by row (front first) and number,
// and returns blocks of n adjacent seats in one row, best first (nearest the
// front, then nearest the centre of the run of free seats), at most one per
// run, plus every free seat in order. Both stores use it, so they choose the
// same seats.
func Candidates(all []SeatPos, n int) ([][]string, []string) {
	var singles []string
	type cand struct {
		ids   []string
		rowIx int
		off   float64
	}
	var cands []cand
	rowIx := -1
	prevRow := ""
	for i := 0; i < len(all); {
		if all[i].Row != prevRow {
			rowIx++
			prevRow = all[i].Row
		}
		j := i
		for j+1 < len(all) && all[j+1].Row == all[i].Row && all[j+1].Num == all[j].Num+1 {
			j++
		}
		run := all[i : j+1]
		for _, x := range run {
			singles = append(singles, x.ID)
		}
		if len(run) >= n {
			best, bestOff := 0, 1e9
			centre := float64(run[0].Num+run[len(run)-1].Num) / 2
			for k := 0; k+n <= len(run); k++ {
				mid := float64(run[k].Num+run[k+n-1].Num) / 2
				if off := abs(mid - centre); off < bestOff {
					best, bestOff = k, off
				}
			}
			ids := make([]string, n)
			for k := 0; k < n; k++ {
				ids[k] = run[best+k].ID
			}
			cands = append(cands, cand{ids, rowIx, bestOff})
		}
		i = j + 1
	}
	sort.SliceStable(cands, func(a, b int) bool {
		if cands[a].rowIx != cands[b].rowIx {
			return cands[a].rowIx < cands[b].rowIx
		}
		return cands[a].off < cands[b].off
	})
	blocks := make([][]string, len(cands))
	for i, c := range cands {
		blocks[i] = c.ids
	}
	return blocks, singles
}
