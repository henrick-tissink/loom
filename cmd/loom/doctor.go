package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"

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
