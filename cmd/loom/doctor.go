package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"time"

	"github.com/henricktissink/loom/internal/diet"
	"github.com/henricktissink/loom/internal/diet/fromstore"
	"github.com/henricktissink/loom/internal/discipline"
)

// doctor reports how this machine's Agent Discipline install compares to
// the bundle loom carries. It reads only; see docs/DISCIPLINE.md §4.
//
//	loom doctor            human-readable report
//	loom doctor --json     the full report as JSON (the GUI's and the checks' contract)
//	loom doctor --deep     also run the bundled suites against the installed hooks
//	loom doctor --strict   exit 1 on amber as well as red
func doctor(args []string) error {
	var asJSON, deep, strict bool
	for _, a := range args {
		switch a {
		case "--json":
			asJSON = true
		case "--deep":
			deep = true
		case "--strict":
			strict = true
		default:
			return fmt.Errorf("usage: loom doctor [--json] [--deep] [--strict]")
		}
	}
	l, err := layoutFromEnv()
	if err != nil {
		return err
	}
	rep := discipline.Doctor(context.Background(), l, discipline.DefaultEnv(), discipline.Options{Deep: deep})
	rep.Diet = buildDiet(context.Background(), l)
	if asJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(rep); err != nil {
			return err
		}
	} else {
		fmt.Print(discipline.Render(rep))
	}
	if rep.Health == "red" || (strict && rep.Health == "amber") {
		return fmt.Errorf("health %s", rep.Health)
	}
	return nil
}

// buildDiet assembles the diet group from the store's transcript index and
// project list. Without a store it still reports plugins and config, with
// a warning in place of the usage grid.
func buildDiet(ctx context.Context, l discipline.Layout) *diet.Report {
	in := diet.Inputs{
		ConfigDir:  l.ConfigDir,
		ClaudeJSON: diet.ClaudeJSONPath(l.ConfigDir, l.Home),
		Now:        time.Now(),
		Run: func(ctx context.Context, name string, args ...string) (string, error) {
			out, err := exec.CommandContext(ctx, name, args...).CombinedOutput()
			return string(out), err
		},
	}
	_, st, err := openStore()
	if err == nil {
		defer st.Close()
		fromstore.Fill(&in, st)
	}
	rep := diet.Build(ctx, in)
	if err != nil {
		rep.Warnings = append(rep.Warnings, "loom.db not opened ("+err.Error()+"): MCP usage unknown")
	}
	return &rep
}
