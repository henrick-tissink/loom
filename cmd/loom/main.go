// loom is the command line over the same engine the GUI uses: list the live
// claude sessions with their fused status, attach to one, kill one, or search
// the indexed session history. It is a thin view, not a second frontend.
//
//	loom ls              live sessions: name, project, status, age, title
//	                     (claudes running in terminals outside loom are listed
//	                     too, marked "(terminal)"; attach/kill do not apply;
//	                     claude's background sessions are "bg-<id>" rows,
//	                     marked "(background)")
//	loom attach <name>   hand this terminal to the session's tmux pane
//	                     (a bg- row: `claude attach <id>`)
//	loom kill <name>     kill the session (the GUI retires its row on its next poll;
//	                     a bg- row is stopped with `claude stop <id>`, its
//	                     conversation kept)
//	loom search <query>  full-text search over indexed session history
//	loom doctor          how this machine's Agent Discipline install compares
//	                     to the bundle loom carries (read-only; docs/DISCIPLINE.md)
//	loom setup           install or upgrade the discipline layer behind its gate
//	loom uninstall       remove exactly what setup installed (from the receipt)
//	loom discipline …    status, and keep/reset/diff of local edits; restore a backup
//	loom brief <project> a deep study brief of what was done in a window
//	                     (--window 24h|3d|5d|1w, --evidence, --list)
package main

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/henricktissink/loom/internal/ccsessions"
	"github.com/henricktissink/loom/internal/config"
	"github.com/henricktissink/loom/internal/status"
	"github.com/henricktissink/loom/internal/store"
	"github.com/henricktissink/loom/internal/tmux"
)

const usage = `usage: loom ls | attach <name> | kill <name> | search <query>
       loom doctor [--json] [--deep] [--strict]
       loom setup [--plan|--dry-run] [--json] [--yes] [--with env] [--without rules,statusline,deny] [--replace <file>,...]
       loom uninstall [--plan|--dry-run] [--json] [--yes]
       loom discipline status | diff <file> | keep <file> | reset <file> | restore <backup>
       loom report hooks|tests [--since change|--days N] [--by project] [--json]
       loom brief <project> [--window 24h|3d|5d|1w] [--evidence | --list]`

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "loom:", err)
		var ee exitError
		if errors.As(err, &ee) {
			os.Exit(ee.code)
		}
		os.Exit(1)
	}
}

func run(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("%s", usage)
	}
	switch args[0] {
	case "ls":
		return ls()
	case "attach":
		if len(args) != 2 {
			return fmt.Errorf("%s", usage)
		}
		return attach(args[1])
	case "kill":
		if len(args) != 2 {
			return fmt.Errorf("%s", usage)
		}
		if id, ok := strings.CutPrefix(args[1], status.BackgroundPrefix); ok && id != "" {
			return ccsessions.Stop(id) // the supervisor keeps its conversation
		}
		return tmux.New().KillSession(args[1])
	case "search":
		if len(args) < 2 {
			return fmt.Errorf("%s", usage)
		}
		return search(strings.Join(args[1:], " "))
	case "doctor":
		return doctor(args[1:])
	case "setup":
		return setup(args[1:])
	case "uninstall":
		return uninstall(args[1:])
	case "discipline":
		return disciplineCmd(args[1:])
	case "report":
		return report(args[1:])
	case "brief":
		return briefCmd(args[1:])
	default:
		return fmt.Errorf("unknown command %q\n%s", args[0], usage)
	}
}

func openStore() (*config.Config, *store.Store, error) {
	cfg, err := config.Load()
	if err != nil {
		return nil, nil, err
	}
	st, err := store.Open(cfg.DBPath())
	if err != nil {
		return nil, nil, err
	}
	return cfg, st, nil
}

// ls runs one engine poll — the same reconcile pass the GUI runs every
// 1.5 s — so what it prints is what the GUI shows, including the registry
// merge and the rows it retires.
func ls() error {
	cfg, st, err := openStore()
	if err != nil {
		return err
	}
	defer st.Close()
	now := time.Now()
	eng := status.NewEngine(tmux.New(), st, cfg.ClaudeConfigDir)
	eng.SetAgents((&ccsessions.AgentsCache{Run: ccsessions.RunAgents}).List)
	snap, err := eng.Poll(now)
	if err != nil {
		return err
	}
	if len(snap.Live) == 0 {
		fmt.Println("no live sessions")
		return nil
	}
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "NAME\tPROJECT\tSTATUS\tAGE\tTITLE")
	for _, r := range snap.Live {
		stat := string(r.Status)
		if r.WaitingFor != "" {
			stat += " (" + r.WaitingFor + ")"
		}
		proj := r.ProjectLabel
		if r.External {
			proj += " (terminal)" // not in a loom pane: nothing to attach to
		} else if r.Background {
			proj += " (background)" // claude's supervisor runs it: attach and kill go through claude
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n", r.Name, proj, stat,
			age(now, r.CreatedAt), r.Title)
	}
	return w.Flush()
}

func attach(name string) error {
	if id, ok := strings.CutPrefix(name, status.BackgroundPrefix); ok && id != "" {
		cmd := exec.Command("claude", "attach", id)
		cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
		return cmd.Run()
	}
	tm := tmux.New()
	if !tm.HasSession(name) {
		return fmt.Errorf("no live session %q (try `loom ls`)", name)
	}
	cmd := tm.AttachCmd(name)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	return cmd.Run()
}

func search(query string) error {
	_, st, err := openStore()
	if err != nil {
		return err
	}
	defer st.Close()
	hits, err := st.SearchSessions(query, 25)
	if err != nil {
		return err
	}
	if len(hits) == 0 {
		fmt.Println("no matches")
		return nil
	}
	for _, h := range hits {
		when := time.Unix(h.LastTS, 0).Format("2006-01-02")
		title := h.Title
		if title == "" {
			title = h.Ask
		}
		fmt.Printf("%s  %s  %s\n    %s\n    %s\n", when, h.SessionID, filepath.Base(h.Cwd), title, snippet(h.Snippet))
	}
	return nil
}

// snippet flattens an FTS snippet to one line; \x01/\x02 wrap matched terms.
func snippet(s string) string {
	s = strings.NewReplacer("\x01", "[", "\x02", "]", "\n", " ", "\r", " ").Replace(s)
	return strings.Join(strings.Fields(s), " ")
}

func age(now time.Time, createdAt int64) string {
	if createdAt <= 0 {
		return "?"
	}
	d := now.Sub(time.Unix(createdAt, 0)).Round(time.Minute)
	switch {
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh%02dm", int(d.Hours()), int(d.Minutes())%60)
	default:
		return fmt.Sprintf("%dd", int(d.Hours()/24))
	}
}
