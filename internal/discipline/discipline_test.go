package discipline

import (
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestManifestCurrent pins the committed manifest to the embedded files: a
// bundle edit without `go generate ./internal/discipline` fails here.
func TestManifestCurrent(t *testing.T) {
	man, err := LoadManifest()
	if err != nil {
		t.Fatal(err)
	}
	if man.Rev == "" || man.Rev != BundledRev() {
		t.Fatalf("manifest rev %q, REV file %q", man.Rev, BundledRev())
	}
	got, err := ComputeFiles(Bundle())
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != len(man.Files) {
		t.Fatalf("manifest lists %d files, bundle has %d: regenerate with go generate ./internal/discipline", len(man.Files), len(got))
	}
	for i := range got {
		if got[i] != man.Files[i] {
			t.Fatalf("manifest entry differs for %s:\n have %+v\n want %+v\nregenerate with go generate ./internal/discipline", got[i].Path, man.Files[i], got[i])
		}
	}
	if len(man.History) == 0 {
		t.Fatal("manifest history is empty: the seed snapshot should be recorded")
	}
	for _, f := range man.Files {
		if strings.Contains(f.Path, "__pycache__") || strings.HasSuffix(f.Path, ".pyc") {
			t.Fatalf("bytecode in bundle: %s", f.Path)
		}
	}
}

// TestRegistrationsCoverBundle: every registered script exists in the
// bundle, and the seven hook events the handbook names are all present.
func TestRegistrationsCoverBundle(t *testing.T) {
	regs, err := LoadRegistrations()
	if err != nil {
		t.Fatal(err)
	}
	if len(regs.Hooks) != 7 {
		t.Fatalf("want 7 hook registrations, got %d", len(regs.Hooks))
	}
	events := map[string]bool{}
	for _, h := range regs.Hooks {
		if _, err := fs.Stat(Bundle(), h.Script); err != nil {
			t.Errorf("registration %s/%s: script not in bundle", h.Event, h.Script)
		}
		events[h.Event] = true
		if h.Script == "enforce/bash_guard.py" && h.OnFailure != "block" {
			t.Errorf("bash_guard must be fail-closed (onFailure: block)")
		}
	}
	for _, e := range []string{"PreToolUse", "Stop", "SubagentStop", "UserPromptSubmit", "PreCompact", "SessionStart"} {
		if !events[e] {
			t.Errorf("no registration for %s", e)
		}
	}
	if !strings.Contains(regs.StatusLine.Command, "context-bar.sh") {
		t.Errorf("statusLine should run the context bar, got %q", regs.StatusLine.Command)
	}
}

// TestBundleSuites runs the hooks' own python suites against the embedded
// bundle in a temp dir. Skipped without python3 unless CI is set.
func TestBundleSuites(t *testing.T) {
	if _, err := exec.LookPath("python3"); err != nil {
		if os.Getenv("CI") != "" {
			t.Fatal("python3 missing in CI")
		}
		t.Skip("python3 not found")
	}
	tmp := t.TempDir()
	if err := copyBundleDir(".", tmp); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "bash", filepath.Join(tmp, "tests", "run_all.sh"))
	cmd.Env = append(os.Environ(), "HOOKS_DIR="+filepath.Join(tmp, "enforce"), "HOME="+filepath.Join(tmp, "home"),
		"CLAUDE_DECISIONS_LOG=/dev/null", "CLAUDE_ALLOWS_LOG=/dev/null")
	out, err := cmd.CombinedOutput()
	if err != nil || !strings.Contains(string(out), "OVERALL: PASS") {
		t.Fatalf("suites failed (%v):\n%s", err, out)
	}
	if strings.Count(string(out), "PASS  test_") != 7 {
		t.Fatalf("want 7 suites, output:\n%s", out)
	}
}

func TestNormalizeCommand(t *testing.T) {
	l := NewLayout("/Users/me/.claude", "/Users/me", "")
	want := "/Users/me/.claude/hooks/enforce/stop_guard.py"
	for _, in := range []string{
		"/Users/me/.claude/hooks/enforce/stop_guard.py",
		"~/.claude/hooks/enforce/stop_guard.py",
		"$HOME/.claude/hooks/enforce/stop_guard.py",
		"python3 -I ~/.claude/hooks/enforce/stop_guard.py",
		"  $CLAUDE_CONFIG_DIR/hooks/enforce/../enforce/stop_guard.py ",
		`/usr/bin/python3 -I "$HOME/.claude/hooks/enforce/stop_guard.py"`,
		"'~/.claude/hooks/enforce/stop_guard.py'",
	} {
		if got := l.NormalizeCommand(in); got != want {
			t.Errorf("%q → %q, want %q", in, got, want)
		}
	}
	if got := l.NormalizeCommand("/Users/me/.local/bin/lava hook"); got != "/Users/me/.local/bin/lava hook" {
		t.Errorf("non-script command changed: %q", got)
	}
}

// fixture builds a machine under a temp dir. kind is LayoutRev or
// LayoutLegacy. It returns the layout and the settings map it wrote, so a
// test can perturb the machine before running the doctor.
func fixture(t *testing.T, kind string) (Layout, map[string]any) {
	t.Helper()
	root := t.TempDir()
	home := filepath.Join(root, "home")
	ccd := filepath.Join(home, ".claude")
	l := NewLayout(ccd, home, "")
	man, err := LoadManifest()
	if err != nil {
		t.Fatal(err)
	}
	switch kind {
	case LayoutRev:
		rev := l.RevDir(man.Rev)
		if err := copyBundleDir(".", rev); err != nil {
			t.Fatal(err)
		}
		must(t, os.Symlink(man.Rev, l.CurrentLink()))
		must(t, os.MkdirAll(filepath.Dir(l.HooksDir()), 0o755))
		must(t, os.Symlink(filepath.Join("..", "discipline", "current", "enforce"), l.HooksDir()))
		must(t, os.MkdirAll(l.ScriptsDir(), 0o755))
		must(t, os.Symlink(filepath.Join("..", "discipline", "current", "scripts", "context-bar.sh"), l.ContextBar()))
		must(t, os.WriteFile(l.ClaudeMD(), []byte("# mine\n\n"+ImportLine+"\n\n## Lava Lamp\nteam text\n"), 0o644))
	case LayoutLegacy:
		must(t, copyBundleDir("enforce", l.HooksDir()))
		must(t, copyBundleDir("tests", filepath.Join(l.HooksDir(), "tests")))
		must(t, copyBundleDir("scripts", l.ScriptsDir()))
		rules, _ := fs.ReadFile(Bundle(), rulesName)
		must(t, os.WriteFile(l.ClaudeMD(), rules, 0o644))
	}
	must(t, os.MkdirAll(filepath.Dir(l.Deadline()), 0o755))
	b, _ := fs.ReadFile(Bundle(), "bin/deadline")
	must(t, os.WriteFile(l.Deadline(), b, 0o755))
	for _, s := range StateSubdirs {
		must(t, os.MkdirAll(filepath.Join(l.StateDir, s), 0o700))
	}
	regs, err := LoadRegistrations()
	if err != nil {
		t.Fatal(err)
	}
	hooks := map[string]any{}
	for _, h := range regs.Hooks {
		entry := map[string]any{"type": "command", "command": filepath.Join(l.HooksDir(), filepath.Base(h.Script)), "timeout": h.Timeout}
		if h.OnFailure != "" {
			entry["onFailure"] = h.OnFailure
		}
		group := map[string]any{"hooks": []any{entry}}
		if h.Matcher != "" {
			group["matcher"] = h.Matcher
		}
		hooks[h.Event] = append(asSlice(hooks[h.Event]), group)
	}
	// Another tool's hook alongside ours, with a key loom must not care about.
	hooks["Stop"] = append(asSlice(hooks["Stop"]), map[string]any{"hooks": []any{map[string]any{"type": "command", "command": filepath.Join(home, ".local/bin/lava hook"), "async": true}}})
	env := map[string]any{"PATH": filepath.Dir(l.Deadline()) + ":/usr/bin:/bin"}
	for k, v := range regs.Env {
		env[k] = v
	}
	settings := map[string]any{
		"hooks":       hooks,
		"statusLine":  map[string]any{"type": "command", "command": "bash " + l.ContextBar()},
		"env":         env,
		"permissions": map[string]any{"deny": []any{"Edit(" + ccd + "/hooks/**)", "Write(" + ccd + "/hooks/**)"}},
	}
	writeJSON(t, l.SettingsPath(), settings)
	return l, settings
}

func asSlice(v any) []any {
	s, _ := v.([]any)
	return s
}

func writeJSON(t *testing.T, p string, v any) {
	t.Helper()
	b, err := json.MarshalIndent(v, "", "  ")
	must(t, err)
	must(t, os.WriteFile(p, b, 0o644))
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

// fakeEnv answers like a healthy machine whose login PATH contains the
// deadline dir of the fixture.
func fakeEnv(l Layout) Env {
	return Env{
		LookPath: func(name string) (string, error) { return "/usr/bin/" + name, nil },
		Run: func(_ context.Context, name string, args ...string) (string, error) {
			switch {
			case name == "python3":
				return "3.12.4", nil
			case name == "claude":
				return "2.1.300 (Claude Code)", nil
			case name == "tmux":
				return "PATH=" + filepath.Dir(l.Deadline()) + ":/usr/bin", nil
			default: // the login shell
				return filepath.Dir(l.Deadline()) + ":/usr/bin:/bin", nil
			}
		},
		Writable:    func(string) bool { return true },
		Quarantined: func(string) bool { return false },
		Shell:       "/bin/zsh",
		Now:         func() time.Time { return time.Date(2026, 10, 15, 15, 0, 0, 0, time.Local) },
	}
}

func TestDoctorRevLayoutIsGreen(t *testing.T) {
	l, _ := fixture(t, LayoutRev)
	r := Doctor(context.Background(), l, fakeEnv(l), Options{})
	if r.Health != "green" {
		t.Fatalf("health %s, issues %+v", r.Health, r.Issues)
	}
	if r.Rev.Installed != BundledRev() || r.Rev.Layout != LayoutRev {
		t.Fatalf("rev %+v", r.Rev)
	}
	if r.Registrations.Matched != 7 || len(r.Registrations.Others) != 1 {
		t.Fatalf("registrations %+v", r.Registrations)
	}
	for _, f := range r.Files {
		if f.State != StateCurrent {
			t.Errorf("%s: %s", f.Path, f.State)
		}
	}
	if !r.Operate.DenyRule || !r.Settings.StatusLineOurs || !r.Path.Login.Deadline || !r.Settings.AutoCompactOK {
		t.Fatalf("settings/operate/path: %+v %+v %+v", r.Operate, r.Settings, r.Path)
	}
	if r.Rules.ImportLines != 1 || r.Rules.Inline {
		t.Fatalf("rules %+v", r.Rules)
	}
	if !strings.Contains(Render(r), "7/7 registrations") {
		t.Fatalf("render:\n%s", Render(r))
	}
}

func TestDoctorLegacyLayoutFindsDrift(t *testing.T) {
	l, settings := fixture(t, LayoutLegacy)
	// A locally modified hook and a missing registration.
	guard := filepath.Join(l.HooksDir(), "bash_guard.py")
	b, _ := os.ReadFile(guard)
	must(t, os.WriteFile(guard, append(b, []byte("\n# local tweak\n")...), 0o755))
	hooks := settings["hooks"].(map[string]any)
	delete(hooks, "PreCompact")
	writeJSON(t, l.SettingsPath(), settings)

	r := Doctor(context.Background(), l, fakeEnv(l), Options{})
	if r.Rev.Installed != "local" || r.Rev.Layout != LayoutLegacy {
		t.Fatalf("rev %+v", r.Rev)
	}
	if r.Health != "red" {
		t.Fatalf("health %s (a missing registration is red); issues %+v", r.Health, r.Issues)
	}
	if r.Registrations.Matched != 6 {
		t.Fatalf("matched %d, want 6", r.Registrations.Matched)
	}
	states := map[string]string{}
	for _, f := range r.Files {
		states[f.Path] = f.State
	}
	if states["enforce/bash_guard.py"] != StateModified {
		t.Errorf("bash_guard: %s", states["enforce/bash_guard.py"])
	}
	if states["enforce/stop_guard.py"] != StateCurrent {
		t.Errorf("stop_guard: %s", states["enforce/stop_guard.py"])
	}
	if states[rulesName] != StateInline || !r.Rules.Inline {
		t.Errorf("rules: %s %+v", states[rulesName], r.Rules)
	}
	if states["examples/task.example.md"] != StateAbsent || states["REV"] != StateAbsent {
		t.Errorf("legacy layout should report bundle-only files absent: %v", states)
	}
	var sawMissing, sawModified bool
	for _, is := range r.Issues {
		if is.Group == "registrations" && strings.Contains(is.Message, "PreCompact") {
			sawMissing = true
		}
		if is.Group == "files" && strings.Contains(is.Message, "bash_guard.py") && is.Severity == SevInfo {
			sawModified = true
		}
	}
	if !sawMissing || !sawModified {
		t.Fatalf("issues: %+v", r.Issues)
	}
}

func TestDoctorNothingInstalled(t *testing.T) {
	home := t.TempDir()
	l := NewLayout(filepath.Join(home, ".claude"), home, "")
	env := fakeEnv(l)
	env.LookPath = func(name string) (string, error) {
		if name == "python3" {
			return "", fmt.Errorf("not found")
		}
		return "/usr/bin/" + name, nil
	}
	r := Doctor(context.Background(), l, env, Options{})
	if r.Rev.Installed != "none" || r.Rev.Layout != LayoutNone || r.Health != "red" {
		t.Fatalf("rev %+v health %s", r.Rev, r.Health)
	}
	if r.Registrations.SettingsParsed || r.Registrations.Matched != 0 {
		t.Fatalf("registrations %+v", r.Registrations)
	}
	var py bool
	for _, is := range r.Issues {
		if is.Group == "prereq" && strings.Contains(is.Message, "python3") {
			py = true
		}
	}
	if !py {
		t.Fatalf("python3 absence not reported: %+v", r.Issues)
	}
	if _, err := json.Marshal(r); err != nil {
		t.Fatal(err)
	}
}

func TestClassifyUsesHistory(t *testing.T) {
	f := ManifestFile{Path: "enforce/common.py", SHA256: "new"}
	man := Manifest{History: map[string]map[string]string{
		"r2":              {"enforce/common.py": "old2"},
		"seed-2026-10-15": {"enforce/common.py": "seed"},
	}}
	for _, tc := range []struct{ sum, state, matches string }{
		{"new", StateCurrent, ""},
		{"old2", StateStale, "r2"},
		{"seed", StateStale, "seed-2026-10-15"},
		{"mine", StateModified, ""},
	} {
		st, m := classify(tc.sum, f, man)
		if st != tc.state || m != tc.matches {
			t.Errorf("sum %s: got %s/%s want %s/%s", tc.sum, st, m, tc.state, tc.matches)
		}
	}
}

func TestManifestRevNotFrozen(t *testing.T) {
	man, err := LoadManifest()
	if err != nil {
		t.Fatal(err)
	}
	if _, frozen := man.History[man.Rev]; frozen {
		t.Fatalf("rev %s is both current and in history: a shipped revision's content must not change; bump REV", man.Rev)
	}
}

// TestHookLogsUnderStateDirWithRev runs one hook from a temp copy of the
// bundle with the log overrides unset: the record must land under
// <state>/log and carry the bundle's rev.
func TestHookLogsUnderStateDirWithRev(t *testing.T) {
	if _, err := exec.LookPath("python3"); err != nil {
		if os.Getenv("CI") != "" {
			t.Fatal("python3 missing in CI")
		}
		t.Skip("python3 not found")
	}
	tmp := t.TempDir()
	if err := copyBundleDir(".", tmp); err != nil {
		t.Fatal(err)
	}
	state := filepath.Join(tmp, "state")
	payload := `{"session_id":"t","cwd":"/tmp","hook_event_name":"PreToolUse","tool_name":"Bash","tool_input":{"command":"echo hi"}}`
	cmd := exec.Command("python3", "-I", filepath.Join(tmp, "enforce", "bash_guard.py"))
	cmd.Stdin = strings.NewReader(payload)
	cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + tmp, "CLAUDE_STATE_DIR=" + state}
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("bash_guard: %v\n%s", err, out)
	}
	b, err := os.ReadFile(filepath.Join(state, "log", "allows.jsonl"))
	if err != nil {
		t.Fatalf("no allows log under state/log: %v", err)
	}
	if !strings.Contains(string(b), `"rev": "`+BundledRev()+`"`) {
		t.Fatalf("record lacks the rev field: %s", b)
	}
	if _, err := os.Stat(filepath.Join(tmp, "enforce", "allows.jsonl")); err == nil {
		t.Fatal("log written beside the hooks")
	}
}
