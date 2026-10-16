package main

import (
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/henricktissink/loom/internal/discipline"
	"github.com/henricktissink/loom/internal/hooklog"
)

// report prints what the discipline hooks did, from their own logs. Reads
// only; see docs/DISCIPLINE.md §3.7 and phase 4.
//
//	loom report hooks [--since change|--days N] [--by project] [--json]
//	loom report tests [--since change|--days N] [--json]
func report(args []string) error {
	const usage = "usage: loom report hooks|tests [--since change|--days N] [--by project] [--json]"
	if len(args) == 0 {
		return fmt.Errorf("%s", usage)
	}
	kind := args[0]
	var asJSON, byProject bool
	sinceChange := true
	days := 0
	for i := 1; i < len(args); i++ {
		switch args[i] {
		case "--json":
			asJSON = true
		case "--by":
			if i+1 >= len(args) || args[i+1] != "project" {
				return fmt.Errorf("%s", usage)
			}
			byProject = true
			i++
		case "--since":
			if i+1 >= len(args) || args[i+1] != "change" {
				return fmt.Errorf("%s", usage)
			}
			sinceChange = true
			i++
		case "--days":
			if i+1 >= len(args) {
				return fmt.Errorf("%s", usage)
			}
			n, err := strconv.Atoi(args[i+1])
			if err != nil || n < 0 {
				return fmt.Errorf("%s", usage)
			}
			days, sinceChange = n, false
			i++
		default:
			return fmt.Errorf("%s", usage)
		}
	}
	l, err := layoutFromEnv()
	if err != nil {
		return err
	}
	reader := hooklog.New(l.LogDir())
	legacy := legacyLogDirs(l)
	for _, d := range legacy {
		reader.AddLegacyDir(d)
	}
	reader.Refresh()
	recs := reader.Records()
	marks := changeMarks(l, recs, legacy)
	now := time.Now()
	var since time.Time
	label := "all records"
	switch {
	case days > 0:
		since = now.Add(-time.Duration(days) * 24 * time.Hour)
		label = fmt.Sprintf("last %d days", days)
	case sinceChange:
		if m, ok := hooklog.LastMark(marks); ok {
			since = m.At
			label = "since the last change (" + m.Label + ", " + m.At.Format("2006-01-02 15:04") + ")"
		}
	}
	switch kind {
	case "hooks":
		if byProject {
			return printJSONOr(asJSON, hooksByProject(recs, since), func() { renderByProject(hooksByProject(recs, since), label) })
		}
		rows := hooklog.Rates(recs, since, marks, now)
		if rows == nil {
			rows = []hooklog.HookRow{}
		}
		return printJSONOr(asJSON, rows, func() { renderRates(rows, label) })
	case "tests":
		rows := hooklog.Reruns(recs, since)
		if rows == nil {
			rows = []hooklog.ProjectRow{}
		}
		return printJSONOr(asJSON, rows, func() { renderReruns(rows, label) })
	}
	return fmt.Errorf("%s", usage)
}

func printJSONOr(asJSON bool, v any, text func()) error {
	if asJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(v)
	}
	text()
	return nil
}

// legacyLogDirs are the pre-loom log locations: a real hooks/enforce
// directory, and the directory loom moved one to.
func legacyLogDirs(l discipline.Layout) []string {
	var dirs []string
	if fi, err := os.Lstat(l.HooksDir()); err == nil && fi.IsDir() {
		dirs = append(dirs, l.HooksDir())
	}
	if rc, _ := discipline.LoadReceipt(l); rc != nil && rc.MigratedFrom != "" {
		dirs = append(dirs, rc.MigratedFrom) // the moved hooks/enforce directory itself
	}
	return dirs
}

// changeMarks are the ledger's hook changes plus the inferred ones.
func changeMarks(l discipline.Layout, recs []hooklog.Record, legacy []string) []hooklog.Mark {
	var solid []hooklog.Mark
	if led, _ := discipline.ReadLedger(l); len(led) > 0 {
		for _, e := range led {
			switch e.Op {
			case "setup", "upgrade", "uninstall", "reset", "restore":
				label := e.Op
				if e.FromRev != "" || e.ToRev != "" {
					label += " " + dash(e.FromRev) + " → " + dash(e.ToRev)
				}
				solid = append(solid, hooklog.Mark{At: e.TS, Label: label})
			}
		}
	}
	out := append(solid, hooklog.InferredMarks(recs, legacy, solid, time.Hour)...)
	sort.SliceStable(out, func(i, j int) bool { return out[i].At.Before(out[j].At) })
	return out
}

func dash(s string) string {
	if s == "" {
		return "—"
	}
	return s
}

func pct(n, of int) string {
	if of == 0 {
		return "   —"
	}
	return fmt.Sprintf("%4.1f%%", 100*float64(n)/float64(of))
}

func renderRates(rows []hooklog.HookRow, label string) {
	fmt.Printf("hook interventions, %s\n", label)
	fmt.Println("holds = blocks and denies; nudges = advisory; the two are never added. Spans are split at hook changes and never summed across one.")
	if len(rows) == 0 {
		fmt.Println("no records in this window")
		return
	}
	for _, r := range rows {
		rev := ""
		if r.Rev != "" {
			rev = " (" + r.Rev + ")"
		}
		fmt.Printf("\n%s%s\n", r.Hook, rev)
		for _, s := range r.Spans {
			from := "start"
			if !s.From.IsZero() {
				from = s.From.Format("01-02 15:04")
			}
			mark := ""
			if s.Mark != "" {
				mark = "  after: " + s.Mark
				if s.Inferred {
					mark += " (inferred)"
				}
			}
			fmt.Printf("  %s → %s  runs=%-6d holds=%-5d %s  nudges=%-5d %s%s\n", from, s.To.Format("01-02 15:04"), s.Runs, s.Holds, pct(s.Holds, s.Runs), s.Nudges, pct(s.Nudges, s.Runs), mark)
			decs := make([]string, 0, len(s.Decisions))
			for d := range s.Decisions {
				decs = append(decs, d)
			}
			sort.Slice(decs, func(i, j int) bool { return s.Decisions[decs[i]] > s.Decisions[decs[j]] })
			if len(decs) > 6 {
				decs = decs[:6]
			}
			for _, d := range decs {
				fmt.Printf("      %6d  %s\n", s.Decisions[d], d)
			}
		}
	}
}

type projectHooks struct {
	Project string         `json:"project"`
	Logged  int            `json:"logged"`
	Holds   int            `json:"holds"`
	Nudges  int            `json:"nudges"`
	ByHook  map[string]int `json:"byHook"`
}

// hooksByProject counts logged decisions per cwd. Silent allows carry no
// cwd, so there is no denominator here: these are counts, not rates.
func hooksByProject(recs []hooklog.Record, since time.Time) []projectHooks {
	per := map[string]*projectHooks{}
	for _, r := range recs {
		if r.Silent || (!since.IsZero() && r.TS.Before(since)) {
			continue
		}
		p := per[r.Cwd]
		if p == nil {
			p = &projectHooks{Project: r.Cwd, ByHook: map[string]int{}}
			per[r.Cwd] = p
		}
		p.Logged++
		p.ByHook[r.Hook]++
		switch r.Class() {
		case "hold":
			p.Holds++
		case "nudge":
			p.Nudges++
		}
	}
	out := make([]projectHooks, 0, len(per))
	for _, p := range per {
		out = append(out, *p)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Logged != out[j].Logged {
			return out[i].Logged > out[j].Logged
		}
		return out[i].Project < out[j].Project
	})
	return out
}

func renderByProject(rows []projectHooks, label string) {
	fmt.Printf("logged decisions per project, %s (counts; silent passes carry no project)\n", label)
	home, _ := os.UserHomeDir()
	for _, r := range rows {
		proj := r.Project
		if home != "" && strings.HasPrefix(proj, home) {
			proj = "~" + proj[len(home):]
		}
		fmt.Printf("  %5d logged  %4d holds  %4d nudges  %s\n", r.Logged, r.Holds, r.Nudges, proj)
	}
}

func renderReruns(rows []hooklog.ProjectRow, label string) {
	fmt.Printf("test runs, %s\n", label)
	if len(rows) == 0 {
		fmt.Println("no test runs logged")
		return
	}
	home, _ := os.UserHomeDir()
	fmt.Println("  runs  no-change  after-change  first  median gap  project")
	for _, r := range rows {
		proj := r.Project
		if home != "" && strings.HasPrefix(proj, home) {
			proj = "~" + proj[len(home):]
		}
		gap := "—"
		if r.MedianGapS > 0 {
			gap = fmt.Sprintf("%ds", r.MedianGapS)
		}
		fmt.Printf("  %4d  %4d (%3s)  %12d  %5d  %10s  %s\n", r.Runs, r.NoChange, strings.TrimSpace(pct(r.NoChange, r.Runs)), r.AfterChange, r.First, gap, proj)
		for _, c := range r.Commands {
			fmt.Printf("           re-run: %s\n", c)
		}
	}
}
