package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/henricktissink/loom/internal/brief"
)

const briefUsage = "usage: loom brief <project> [--window 24h|3d|5d|1w] [--evidence | --list]"

// briefCmd writes, lists or previews a project's study brief.
//
//	loom brief <project>              write a brief for the last 24 hours (uses Claude quota)
//	loom brief <project> --window 3d  …for the last 3 days (24h, 3d, 5d, 1w)
//	loom brief <project> --evidence   print the evidence it would be written from; no Claude call
//	loom brief <project> --list       the project's saved briefs, newest first
//
// <project> is a project's name (case-insensitive) or root path.
func briefCmd(args []string) error {
	var project, win string
	var evidence, list bool
	win = "24h"
	for i := 0; i < len(args); i++ {
		switch a := args[i]; {
		case a == "--evidence":
			evidence = true
		case a == "--list":
			list = true
		case a == "--window" && i+1 < len(args):
			win = args[i+1]
			i++
		case strings.HasPrefix(a, "--window="):
			win = strings.TrimPrefix(a, "--window=")
		case !strings.HasPrefix(a, "-") && project == "":
			project = a
		default:
			return fmt.Errorf("%s", briefUsage)
		}
	}
	if project == "" || (evidence && list) {
		return fmt.Errorf("%s", briefUsage)
	}
	w, err := brief.ParseWindow(win)
	if err != nil {
		return err
	}
	cfg, st, err := openStore()
	if err != nil {
		return err
	}
	defer st.Close()
	svc := &brief.Service{
		Store: st, ClaudeConfigDir: cfg.ClaudeConfigDir,
		Archive: brief.Archive{Dir: filepath.Join(cfg.LoomDir, "briefs")},
		Writer:  brief.Writer{WorkDir: cfg.LoomDir},
	}
	sc, err := svc.Scope(project)
	if err != nil {
		return err
	}
	if list {
		ms, err := svc.Archive.List(sc.Root, sc.Name)
		if err != nil {
			return err
		}
		for _, m := range ms {
			p, _ := svc.Archive.Path(m.ID, brief.FileBrief)
			fmt.Printf("%s  %-3s  %d sessions, %d commits  %s\n", time.Unix(m.CreatedAt, 0).Format("2006-01-02 15:04"), m.Window, m.Sessions, m.Commits, p)
		}
		return nil
	}
	ctx := context.Background()
	b, err := svc.Collect(ctx, sc, w)
	if err != nil {
		return err
	}
	s := b.Stats()
	if evidence {
		fmt.Print(b.Markdown())
		fmt.Fprintf(os.Stderr, "%d sessions, %d of your commits, %d by others · ~%dk tokens of evidence\n", s.Sessions, s.Commits, s.OtherCommits, s.InputTokens/1000)
		return nil
	}
	fmt.Fprintf(os.Stderr, "writing the %s brief for %s from %d sessions and %d of your commits (~%dk tokens in, %s); this takes a few minutes…\n",
		w.Label, sc.Name, s.Sessions, s.Commits, s.InputTokens/1000, brief.WriterModel)
	m, err := svc.Generate(ctx, b, w)
	if err != nil {
		return err
	}
	p, _ := svc.Archive.Path(m.ID, brief.FileBrief)
	fmt.Println(p)
	return nil
}
