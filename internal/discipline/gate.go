package discipline

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"syscall"
	"time"
)

// The install gate (handbook, "operate" layer): a staged revision is
// never switched in until its suites pass under the machine's own python
// and the last 30 days of logged denials replay the same way through the
// staged guard as through the installed one. Everything here runs in
// temp dirs and the staged directory; nothing it does is visible to a
// running session.

type ReplayResult struct {
	Ran       bool     `json:"ran"`
	NewAllows []string `json:"newAllows"` // denied in the log, allowed by the staged guard, still denied by the installed one
	NewDenies []string `json:"newDenies"` // the reverse
	Output    string   `json:"output,omitempty"`
}

type GateResult struct {
	Preflight []string     `json:"preflightFailures"`
	Suites    SuitesReport `json:"suites"`
	Replay    ReplayResult `json:"replay"`
	Passed    bool         `json:"passed"`
}

// Flips reports whether the replay found verdict changes the person must
// acknowledge before the install goes ahead.
func (g GateResult) Flips() int { return len(g.Replay.NewAllows) + len(g.Replay.NewDenies) }

// RunGate checks a staged revision dir. installedHooks is the hook dir
// sessions use today ("" when none); logPath the decisions log to replay.
func RunGate(ctx context.Context, staged, installedHooks, logPath string) GateResult {
	g := GateResult{Preflight: []string{}, Replay: ReplayResult{NewAllows: []string{}, NewDenies: []string{}}}
	enforce := filepath.Join(staged, "enforce")

	// Preflight: python3 runs, every hook parses, the deadline wrapper works.
	if out, err := run(ctx, 10*time.Second, nil, "python3", "-c", "pass"); err != nil {
		g.Preflight = append(g.Preflight, "python3 does not run: "+errOut(out, err))
		return g
	}
	files, _ := filepath.Glob(filepath.Join(enforce, "*.py"))
	args := append([]string{"-I", "-c", "import ast, sys\nfor f in sys.argv[1:]:\n    ast.parse(open(f).read(), f)"}, files...)
	if out, err := run(ctx, 30*time.Second, nil, "python3", args...); err != nil {
		g.Preflight = append(g.Preflight, "a hook does not parse: "+errOut(out, err))
	}
	if out, err := run(ctx, 20*time.Second, nil, filepath.Join(staged, "bin", "deadline"), "5", "true"); err != nil {
		g.Preflight = append(g.Preflight, "deadline wrapper failed: "+errOut(out, err))
	}
	if len(g.Preflight) > 0 {
		return g
	}

	// Suites, from the staged tests against the staged hooks.
	env := []string{"HOOKS_DIR=" + enforce, "CLAUDE_DECISIONS_LOG=/dev/null", "CLAUDE_ALLOWS_LOG=/dev/null"}
	out, err := run(ctx, 180*time.Second, env, "bash", filepath.Join(staged, "tests", "run_all.sh"))
	g.Suites = SuitesReport{Ran: true, Output: out, Passed: err == nil && strings.Contains(out, "OVERALL: PASS")}
	if !g.Suites.Passed {
		return g
	}

	// Replay: flips of the staged guard versus the installed guard.
	if _, err := os.Stat(logPath); err == nil {
		g.Replay.Ran = true
		stagedFlips, sout := replayFlips(ctx, enforce, filepath.Join(enforce, "bash_guard.py"), logPath)
		g.Replay.Output = sout
		installedFlips := map[string]bool{}
		if installedHooks != "" {
			if _, err := os.Stat(filepath.Join(installedHooks, "bash_guard.py")); err == nil {
				installedFlips, _ = replayFlips(ctx, enforce, filepath.Join(installedHooks, "bash_guard.py"), logPath)
			}
		}
		for k := range stagedFlips {
			if !installedFlips[k] {
				g.Replay.NewAllows = append(g.Replay.NewAllows, k)
			}
		}
		for k := range installedFlips {
			if !stagedFlips[k] {
				g.Replay.NewDenies = append(g.Replay.NewDenies, k)
			}
		}
		sort.Strings(g.Replay.NewAllows)
		sort.Strings(g.Replay.NewDenies)
	}
	g.Passed = true
	return g
}

var flipLine = regexp.MustCompile(`(?m)^\s+(\S+)\s+(\S+)\s+->\s+allow\s+(.*)$`)

// replayFlips runs the staged replay tool with a given guard and returns
// the set of logged denials that guard would now allow.
func replayFlips(ctx context.Context, enforce, guard, logPath string) (map[string]bool, string) {
	env := []string{"CLAUDE_DECISIONS_LOG=" + logPath}
	out, _ := run(ctx, 120*time.Second, env, "python3", "-I", filepath.Join(enforce, "replay_decisions.py"), "--days", "30", "--guard", guard)
	flips := map[string]bool{}
	for _, m := range flipLine.FindAllStringSubmatch(out, -1) {
		flips[m[1]+" "+m[2]+" "+m[3]] = true
	}
	return flips, out
}

// run executes a command with a deadline, killing its whole process group
// on expiry, and returns combined output.
func run(ctx context.Context, timeout time.Duration, extraEnv []string, name string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Env = append(os.Environ(), extraEnv...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
	cmd.WaitDelay = 5 * time.Second
	out, err := cmd.CombinedOutput()
	if ctx.Err() != nil {
		return string(out), fmt.Errorf("timed out after %s", timeout)
	}
	return string(out), err
}

// RenderGate prints a gate result for the terminal.
func RenderGate(g GateResult) string {
	var b strings.Builder
	if len(g.Preflight) > 0 {
		for _, f := range g.Preflight {
			fmt.Fprintf(&b, "preflight FAIL  %s\n", f)
		}
		return b.String()
	}
	b.WriteString("preflight ok\n")
	if g.Suites.Ran {
		res := "PASS"
		if !g.Suites.Passed {
			res = "FAIL"
		}
		fmt.Fprintf(&b, "suites    %s\n", res)
		if !g.Suites.Passed {
			b.WriteString(indent(g.Suites.Output))
		}
	}
	if g.Replay.Ran {
		fmt.Fprintf(&b, "replay    %d newly allowed, %d newly denied (30 days of logged denials)\n", len(g.Replay.NewAllows), len(g.Replay.NewDenies))
		for _, s := range g.Replay.NewAllows {
			fmt.Fprintf(&b, "          now allow  %s\n", s)
		}
		for _, s := range g.Replay.NewDenies {
			fmt.Fprintf(&b, "          now deny   %s\n", s)
		}
	}
	return b.String()
}

func errOut(out string, err error) string {
	out = strings.TrimSpace(out)
	if out == "" {
		return err.Error()
	}
	return err.Error() + ": " + out
}
