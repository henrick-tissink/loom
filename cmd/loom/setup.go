package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/henricktissink/loom/internal/discipline"
)

// The writing side of the discipline layer (docs/DISCIPLINE.md §5):
//
//	loom setup [--plan|--dry-run] [--json] [--yes]
//	           [--with env] [--without rules,statusline,deny] [--replace <file>,...]
//	loom uninstall [--plan|--dry-run] [--json] [--yes]
//	loom discipline status | diff <file> | keep <file> | reset <file> | restore <backup>
//
// Every command that writes refuses inside a Claude Code session or
// without a terminal (exit 2) and asks before writing unless --yes.

// exitError carries a specific exit status out of run().
type exitError struct {
	code int
	err  error
}

func (e exitError) Error() string { return e.err.Error() }

func refuseIfAgent() error {
	if err := discipline.RefuseIfAgent(os.Stdin); err != nil {
		return exitError{2, err}
	}
	return nil
}

func layoutFromEnv() (discipline.Layout, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return discipline.Layout{}, err
	}
	// Not config.Load(): that creates ~/.loom, and the read-only commands write nothing.
	ccd := os.Getenv("CLAUDE_CONFIG_DIR")
	if ccd == "" {
		ccd = filepath.Join(home, ".claude")
	}
	return discipline.NewLayout(ccd, home, os.Getenv("CLAUDE_STATE_DIR")), nil
}

func confirm(prompt string) bool {
	fmt.Printf("%s [y/N] ", prompt)
	line, _ := bufio.NewReader(os.Stdin).ReadString('\n')
	switch strings.ToLower(strings.TrimSpace(line)) {
	case "y", "yes":
		return true
	}
	return false
}

func printJSON(v any) error {
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

func setup(args []string) error {
	var planOnly, asJSON, yes bool
	opts := discipline.DefaultPlanOptions()
	for i := 0; i < len(args); i++ {
		a := args[i]
		next := func() (string, error) {
			if i+1 >= len(args) {
				return "", fmt.Errorf("%s needs a value", a)
			}
			i++
			return args[i], nil
		}
		switch a {
		case "--plan", "--dry-run":
			planOnly = true
		case "--json":
			asJSON = true
		case "--yes", "-y":
			yes = true
		case "--with", "--without":
			v, err := next()
			if err != nil {
				return err
			}
			on := a == "--with"
			for _, k := range strings.Split(v, ",") {
				switch strings.TrimSpace(k) {
				case "env":
					opts.Env = on
				case "deny", "deny-rule":
					opts.Deny = on
				case "rules":
					opts.Rules = on
				case "statusline", "statusLine":
					opts.StatusLine = on
				default:
					return fmt.Errorf("%s: unknown group %q (env, deny, rules, statusline)", a, k)
				}
			}
		case "--replace":
			v, err := next()
			if err != nil {
				return err
			}
			for _, f := range strings.Split(v, ",") {
				if f = strings.TrimSpace(f); f != "" {
					if !filepath.IsAbs(f) {
						f, _ = filepath.Abs(f)
					}
					opts.Replace = append(opts.Replace, f)
				}
			}
		default:
			return fmt.Errorf("usage: loom setup [--plan|--dry-run] [--json] [--yes] [--with env] [--without rules,statusline,deny] [--replace <file>,...]")
		}
	}
	l, err := layoutFromEnv()
	if err != nil {
		return err
	}
	now := time.Now()
	ctx := context.Background()
	rep := discipline.Doctor(ctx, l, discipline.DefaultEnv(), discipline.Options{})
	plan := discipline.PlanSetup(l, rep, opts, now)
	if planOnly {
		if asJSON {
			return printJSON(plan)
		}
		fmt.Print(discipline.RenderPlan(plan))
		if plan.HasErrors() {
			return errors.New("the plan has errors; nothing would be written")
		}
		fmt.Println("\n(dry run: nothing written)")
		return nil
	}
	if err := refuseIfAgent(); err != nil {
		return err
	}
	if !asJSON {
		fmt.Print(discipline.RenderPlan(plan))
	}
	if plan.HasErrors() {
		return errors.New("the plan has errors; nothing was written")
	}
	if !yes && !confirm("\nStage "+plan.Rev+", run the gate, then apply these changes?") {
		return errors.New("declined; nothing was written")
	}
	aopts := discipline.ApplyOptions{Now: now, Build: build()}
	if !asJSON {
		fmt.Println("\nstaging " + plan.Rev + " and running the gate …")
	}
	st, err := discipline.Stage(ctx, l, plan, aopts)
	if err != nil {
		if st != nil && !asJSON {
			fmt.Print(discipline.RenderGate(st.Gate))
		}
		if asJSON && st != nil {
			printJSON(map[string]any{"plan": plan, "gate": st.Gate, "error": err.Error()})
		}
		return err
	}
	if !asJSON {
		fmt.Print(discipline.RenderGate(st.Gate))
	}
	if st.Gate.Flips() > 0 && !yes && !confirm("The staged guard decides some logged commands differently. Accept these verdict changes?") {
		discipline.Abandon(st)
		return errors.New("declined; the staged revision was removed and nothing else was written")
	}
	res, err := discipline.Commit(ctx, l, plan, st, aopts)
	if asJSON {
		out := map[string]any{"plan": plan, "gate": st.Gate, "result": res}
		if err != nil {
			out["error"] = err.Error()
		}
		printJSON(out)
		return err
	}
	if res != nil {
		for _, line := range res.Log {
			fmt.Println("  " + line)
		}
	}
	if err != nil {
		return err
	}
	fmt.Printf("\ninstalled %s. Backups in %s. Run `loom doctor` to confirm; sessions started from now on use the new hooks.\n", plan.Rev, res.BackupDir)
	return nil
}

func uninstall(args []string) error {
	var planOnly, asJSON, yes bool
	for _, a := range args {
		switch a {
		case "--plan", "--dry-run":
			planOnly = true
		case "--json":
			asJSON = true
		case "--yes", "-y":
			yes = true
		default:
			return fmt.Errorf("usage: loom uninstall [--plan|--dry-run] [--json] [--yes]")
		}
	}
	l, err := layoutFromEnv()
	if err != nil {
		return err
	}
	rc, err := discipline.LoadReceipt(l)
	if err != nil {
		return err
	}
	ctx := context.Background()
	rep := discipline.Doctor(ctx, l, discipline.DefaultEnv(), discipline.Options{})
	plan := discipline.PlanUninstall(l, rep, rc)
	if planOnly {
		if asJSON {
			return printJSON(plan)
		}
		fmt.Print(discipline.RenderPlan(plan))
		if plan.HasErrors() {
			return errors.New("the plan has errors; nothing would be written")
		}
		fmt.Println("\n(dry run: nothing written)")
		return nil
	}
	if err := refuseIfAgent(); err != nil {
		return err
	}
	if !asJSON {
		fmt.Print(discipline.RenderPlan(plan))
	}
	if plan.HasErrors() {
		return errors.New("the plan has errors; nothing was written")
	}
	if !yes && !confirm("\nRemove what loom installed (your own edits and other tools' entries stay)?") {
		return errors.New("declined; nothing was written")
	}
	res, err := discipline.ApplyUninstall(ctx, l, plan, rc, discipline.ApplyOptions{Now: time.Now(), Build: build()})
	if asJSON {
		out := map[string]any{"plan": plan, "result": res}
		if err != nil {
			out["error"] = err.Error()
		}
		printJSON(out)
		return err
	}
	if res != nil {
		for _, line := range res.Log {
			fmt.Println("  " + line)
		}
	}
	return err
}

func disciplineCmd(args []string) error {
	const use = "usage: loom discipline status | diff <file> | keep <file> | reset <file> | restore <backup>"
	if len(args) == 0 {
		return errors.New(use)
	}
	l, err := layoutFromEnv()
	if err != nil {
		return err
	}
	kind, _ := l.Installed()
	now := time.Now()
	switch args[0] {
	case "status":
		return disciplineStatus(l)
	case "diff":
		if len(args) != 2 {
			return errors.New(use)
		}
		f, installed, err := discipline.ResolveFile(l, kind, args[1])
		if err != nil {
			return err
		}
		d, err := discipline.FileDiff(f, installed)
		if err != nil {
			return err
		}
		if d == "" {
			fmt.Printf("%s matches the bundle\n", installed)
			return nil
		}
		fmt.Print(d)
		return nil
	case "keep":
		if len(args) != 2 {
			return errors.New(use)
		}
		if err := refuseIfAgent(); err != nil {
			return err
		}
		f, installed, err := discipline.ResolveFile(l, kind, args[1])
		if err != nil {
			return err
		}
		if err := discipline.Keep(l, f, installed, now); err != nil {
			return err
		}
		fmt.Printf("kept: %s is yours; upgrades leave it alone and the doctor stops reporting it\n", installed)
		return nil
	case "reset":
		if len(args) != 2 {
			return errors.New(use)
		}
		if err := refuseIfAgent(); err != nil {
			return err
		}
		f, installed, err := discipline.ResolveFile(l, kind, args[1])
		if err != nil {
			return err
		}
		if d, _ := discipline.FileDiff(f, installed); d != "" {
			fmt.Print(d)
		}
		if !confirm("Replace " + installed + " with the bundle's copy (backed up first)?") {
			return errors.New("declined; nothing was written")
		}
		dir, err := discipline.Reset(l, f, installed, now)
		if err != nil {
			return err
		}
		fmt.Printf("reset %s (previous copy in %s)\n", installed, dir)
		return nil
	case "restore":
		if len(args) != 2 {
			names := discipline.ListBackups(l)
			if len(names) == 0 {
				return errors.New("no backups under " + l.BackupsDir())
			}
			fmt.Println("backups (oldest first):")
			for _, n := range names {
				fmt.Println("  " + n)
			}
			return errors.New(use)
		}
		if err := refuseIfAgent(); err != nil {
			return err
		}
		if !confirm("Copy the files in backup " + args[1] + " back over the current ones (which are backed up first)?") {
			return errors.New("declined; nothing was written")
		}
		done, err := discipline.RestoreBackup(l, args[1], now)
		for _, p := range done {
			fmt.Println("restored " + p)
		}
		return err
	default:
		return errors.New(use)
	}
}

func disciplineStatus(l discipline.Layout) error {
	rep := discipline.Doctor(context.Background(), l, discipline.DefaultEnv(), discipline.Options{})
	rc, err := discipline.LoadReceipt(l)
	if err != nil {
		return err
	}
	fmt.Printf("bundle %s · installed %s (%s layout) · health %s\n", rep.Rev.Bundled, rep.Rev.Installed, rep.Rev.Layout, rep.Health)
	if rc == nil {
		fmt.Println("receipt   none (this install was not made by loom setup)")
	} else {
		fmt.Printf("receipt   %s installed %s by loom %s\n", rc.Rev, rc.InstalledAt.Format("2006-01-02 15:04"), rc.LoomBuild)
		if len(rc.Kept) > 0 {
			fmt.Println("kept (yours, by loom discipline keep):")
			for p := range rc.Kept {
				fmt.Println("  " + p)
			}
		}
	}
	var local []discipline.FileState
	for _, f := range rep.Files {
		if f.State == discipline.StateModified || f.State == discipline.StateStale || f.State == discipline.StateMissing {
			local = append(local, f)
		}
	}
	if len(local) == 0 {
		fmt.Println("files     all match the bundle")
	} else {
		fmt.Println("files     differing from the bundle:")
		for _, f := range local {
			fmt.Printf("  %-9s %s\n", f.State, f.InstalledPath)
		}
		fmt.Println("          loom discipline diff <file> shows a change; keep records it as yours; reset puts the bundle's copy back")
	}
	led, _ := discipline.ReadLedger(l)
	if len(led) > 0 {
		fmt.Println("ledger    (newest last)")
		start := 0
		if len(led) > 8 {
			start = len(led) - 8
		}
		for _, e := range led[start:] {
			line := fmt.Sprintf("  %s  %-9s", e.TS.Format("2006-01-02 15:04"), e.Op)
			if e.FromRev != "" || e.ToRev != "" {
				line += " " + e.FromRev + "→" + e.ToRev
			}
			if e.Note != "" {
				line += " " + e.Note
			}
			fmt.Println(line)
		}
	}
	names := discipline.ListBackups(l)
	if len(names) > 0 {
		fmt.Printf("backups   %d under %s (loom discipline restore <name>)\n", len(names), l.BackupsDir())
	}
	return nil
}

// build is the loom build id for receipts: the module version when built
// with `go install`, else "dev".
func build() string {
	if v := buildVersion(); v != "" {
		return v
	}
	return "dev"
}
