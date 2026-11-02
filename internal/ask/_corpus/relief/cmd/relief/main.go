// Command relief runs one relief team's device.
//
//	relief init   -dir ./camp2 -device camp2-phone
//	relief stock  -dir ./camp2 rice_kg +50        (or -20)
//	relief need   -dir ./camp2 open "insulin camp-2" | done "insulin camp-2"
//	relief set    -dir ./camp2 camp-2.status flooded
//	relief show   -dir ./camp2
//	relief vv     -dir ./camp2 > camp2.vv.json    (what this device has)
//	relief export -dir ./camp2 -for other.vv.json -o to-other.bundle
//	relief import -dir ./camp4 to-other.bundle
//	relief serve  -dir ./camp2 -addr 127.0.0.1:7700
//	relief sync   -dir ./camp4 http://127.0.0.1:7700
//	relief sim    -runs 10000
package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"sort"
	"strconv"
	"time"

	"github.com/Aravindh97/relief/internal/crdt"
	"github.com/Aravindh97/relief/internal/sim"
	"github.com/Aravindh97/relief/internal/store"
)

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: relief init|stock|need|set|show|vv|export|import|serve|sync|sim")
		os.Exit(2)
	}
	if err := run(os.Args[1], os.Args[2:]); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

// args parses flags anywhere in the argument list. Signed numbers such as
// -60 are quantities, not flags.
func args(fs *flag.FlagSet, in []string) []string {
	var pos, rest []string
	for _, a := range in {
		if len(a) > 1 && (a[0] == '-' || a[0] == '+') {
			if _, err := strconv.ParseInt(a, 10, 64); err == nil {
				pos = append(pos, a)
				continue
			}
		}
		rest = append(rest, a)
	}
	in = rest
	var flagged []string
	for {
		fs.Parse(in)
		if fs.NArg() == 0 {
			break
		}
		flagged = append(flagged, fs.Arg(0))
		in = fs.Args()[1:]
	}
	// keep the original order: quantities come last in every command
	return append(flagged, pos...)
}

func run(cmd string, in []string) error {
	fs := flag.NewFlagSet(cmd, flag.ExitOnError)
	dir := fs.String("dir", ".", "replica directory")
	switch cmd {
	case "init":
		device := fs.String("device", "", "device name, unique per device")
		args(fs, in)
		if *device == "" {
			return errors.New("-device is required")
		}
		return store.Init(*dir, *device)

	case "stock":
		p := args(fs, in)
		if len(p) != 2 {
			return errors.New("usage: relief stock ITEM +N|-N")
		}
		n, err := strconv.ParseInt(p[1], 10, 64)
		if err != nil || n == 0 {
			return fmt.Errorf("bad quantity %q", p[1])
		}
		return store.Store{Dir: *dir}.With(func(d *crdt.Doc) error { d.AddStock(p[0], n); return nil })

	case "need":
		p := args(fs, in)
		if len(p) != 2 || (p[0] != "open" && p[0] != "done") {
			return errors.New(`usage: relief need open|done "NEED"`)
		}
		return store.Store{Dir: *dir}.With(func(d *crdt.Doc) error {
			if p[0] == "open" {
				d.OpenNeed(p[1])
			} else {
				d.CloseNeed(p[1])
			}
			return nil
		})

	case "set":
		p := args(fs, in)
		if len(p) != 2 {
			return errors.New("usage: relief set FIELD VALUE")
		}
		return store.Store{Dir: *dir}.With(func(d *crdt.Doc) error { d.SetField(p[0], p[1]); return nil })

	case "show":
		args(fs, in)
		return store.Store{Dir: *dir}.With(func(d *crdt.Doc) error {
			st := d.State()
			fmt.Printf("device %s  ops %d  state %s\n", d.Device, d.OpCount(), d.Hash())
			items := make([]string, 0, len(st.Stock))
			for k := range st.Stock {
				items = append(items, k)
			}
			sort.Strings(items)
			for _, k := range items {
				fmt.Printf("  stock  %-14s %d\n", k, st.Stock[k])
			}
			for _, n := range st.Needs {
				fmt.Printf("  need   %s\n", n)
			}
			fk := make([]string, 0, len(st.Fields))
			for k := range st.Fields {
				fk = append(fk, k)
			}
			sort.Strings(fk)
			for _, k := range fk {
				fmt.Printf("  field  %-14s %s\n", k, st.Fields[k])
			}
			for _, c := range d.Conflicts() {
				fmt.Printf("  CHECK  %s\n", c)
			}
			return nil
		})

	case "vv":
		args(fs, in)
		return store.Store{Dir: *dir}.With(func(d *crdt.Doc) error { return json.NewEncoder(os.Stdout).Encode(d.VV()) })

	case "export":
		forVV := fs.String("for", "", "the other device's vv file; omit to export everything")
		out := fs.String("o", "changes.bundle", "bundle file to write")
		args(fs, in)
		peer := crdt.VV{}
		if *forVV != "" {
			b, err := os.ReadFile(*forVV)
			if err != nil {
				return err
			}
			if err := json.Unmarshal(b, &peer); err != nil {
				return err
			}
		}
		return store.Store{Dir: *dir}.With(func(d *crdt.Doc) error {
			ops := d.Missing(peer)
			fmt.Printf("exported %d ops (%d bytes est.) to %s\n", len(ops), crdt.EncodedSize(ops), *out)
			return store.WriteBundle(*out, d.Device, ops)
		})

	case "import":
		p := args(fs, in)
		if len(p) != 1 {
			return errors.New("usage: relief import FILE")
		}
		b, err := store.ReadBundle(p[0])
		if err != nil {
			return err
		}
		return store.Store{Dir: *dir}.With(func(d *crdt.Doc) error {
			n, err := d.Receive(b.Ops)
			fmt.Printf("imported %d new ops from %s (%d already known)\n", n, b.From, len(b.Ops)-n)
			return err
		})

	case "serve":
		addr := fs.String("addr", "127.0.0.1:7700", "listen address (loopback only)")
		args(fs, in)
		host, _, err := net.SplitHostPort(*addr)
		if err != nil {
			return err
		}
		if ip := net.ParseIP(host); host != "localhost" && (ip == nil || !ip.IsLoopback()) {
			return errors.New("serve only listens on loopback; to sync across devices use bundles, or an SSH/WireGuard tunnel")
		}
		return serve(*dir, *addr)

	case "sync":
		p := args(fs, in)
		if len(p) != 1 {
			return errors.New("usage: relief sync http://127.0.0.1:7700")
		}
		return syncWith(*dir, p[0])

	case "sim":
		runs := fs.Int("runs", 10000, "randomised runs")
		args(fs, in)
		return simReport(*runs)
	}
	return fmt.Errorf("unknown command %q", cmd)
}

type syncReq struct {
	VV  crdt.VV   `json:"vv"`
	Ops []crdt.Op `json:"ops"`
}

type syncResp struct {
	VV  crdt.VV   `json:"vv"`
	Ops []crdt.Op `json:"ops"`
	New int       `json:"new"`
}

func serve(dir, addr string) error {
	s := store.Store{Dir: dir}
	mux := http.NewServeMux()
	// POST /sync: client sends its vv and the ops it believes we lack; we
	// apply them and answer with ours that it lacks.
	mux.HandleFunc("POST /sync", func(w http.ResponseWriter, r *http.Request) {
		var req syncReq
		if err := json.NewDecoder(io.LimitReader(r.Body, 64<<20)).Decode(&req); err != nil {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		var resp syncResp
		err := s.With(func(d *crdt.Doc) error {
			n, err := d.Receive(req.Ops)
			resp = syncResp{VV: d.VV(), Ops: d.Missing(req.VV), New: n}
			return err
		})
		if err != nil {
			http.Error(w, err.Error(), http.StatusConflict)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(resp)
	})
	mux.HandleFunc("GET /vv", func(w http.ResponseWriter, r *http.Request) {
		s.With(func(d *crdt.Doc) error { return json.NewEncoder(w).Encode(d.VV()) })
	})
	hs := &http.Server{Addr: addr, Handler: mux, ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 30 * time.Second}
	fmt.Fprintf(os.Stderr, "relief serving %s on http://%s\n", dir, addr)
	return hs.ListenAndServe()
}

func syncWith(dir, url string) error {
	s := store.Store{Dir: dir}
	c := &http.Client{Timeout: 30 * time.Second}
	// 1. learn what the peer has
	resp, err := c.Get(url + "/vv")
	if err != nil {
		return err
	}
	var peer crdt.VV
	err = json.NewDecoder(resp.Body).Decode(&peer)
	resp.Body.Close()
	if err != nil {
		return err
	}
	// 2. send what it lacks, receive what we lack
	return s.With(func(d *crdt.Doc) error {
		out := d.Missing(peer)
		b, _ := json.Marshal(syncReq{VV: d.VV(), Ops: out})
		r, err := c.Post(url+"/sync", "application/json", bytes.NewReader(b))
		if err != nil {
			return err
		}
		defer r.Body.Close()
		if r.StatusCode != 200 {
			m, _ := io.ReadAll(r.Body)
			return fmt.Errorf("peer: %s", m)
		}
		var sr syncResp
		if err := json.NewDecoder(r.Body).Decode(&sr); err != nil {
			return err
		}
		n, err := d.Receive(sr.Ops)
		fmt.Printf("sent %d ops (peer applied %d), received %d new ops\n", len(out), sr.New, n)
		return err
	})
}

func simReport(runs int) error {
	t := time.Now()
	var conv, viol, baseLostRuns int
	var ops, sent, full, dbytes, fbytes, syncs int
	var baseLost int64
	var first string
	for i := 0; i < runs; i++ {
		r := sim.Run(sim.Config{Devices: 3 + i%6, Steps: 100 + i%400, Seed: uint64(i)})
		if r.Converged {
			conv++
		}
		if len(r.Violations) > 0 {
			viol++
			if first == "" {
				first = fmt.Sprintf("seed %d: %v", i, r.Violations)
			}
		}
		if r.BaselineLost > 0 {
			baseLostRuns++
			baseLost += r.BaselineLost
		}
		ops += r.OpsTotal
		sent += r.OpsSent
		full += r.FullStateSent
		dbytes += r.DeltaBytes
		fbytes += r.FullBytes
		syncs += r.Syncs
	}
	fmt.Printf("runs=%d (3-8 devices, 100-500 steps, random partitions, relays, duplicate deliveries, clock skew up to ±5 min) took %s\n",
		runs, time.Since(t).Round(time.Millisecond))
	fmt.Printf("converged: %d/%d   invariant violations: %d (no lost stock change, add-wins needs, latest-HLC fields)\n", conv, runs, viol)
	fmt.Printf("baseline (whole-document last-writer-wins): lost data in %d/%d runs, %d stock units wrong in total\n", baseLostRuns, runs, baseLost)
	fmt.Printf("sync cost: %d syncs, %.1f ops/sync with version vectors vs %.1f ops/sync sending everything (%.1f%% of the bytes)\n",
		syncs, float64(sent)/float64(syncs), float64(full)/float64(syncs), 100*float64(dbytes)/float64(fbytes))
	if first != "" {
		return errors.New(first)
	}
	return nil
}
