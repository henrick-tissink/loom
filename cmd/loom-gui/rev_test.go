package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/henricktissink/loom/internal/store"
)

// --- §7.2: the rev has to survive the wire ----------------------------------

// webviewRoundTrip is what actually happens to a rev between two polls, and
// nothing in the Go test suite exercised it before: the snapshot is marshalled,
// the webview's JSON.parse turns the number into an IEEE-754 double, and the
// client hands that double straight back as `sinceRev`. Modelling it as
// uint64 → float64 → uint64 is exactly that path.
func webviewRoundTrip(t *testing.T, rev uint64) uint64 {
	t.Helper()
	wire, err := json.Marshal(struct {
		Rev uint64 `json:"rev"`
	}{rev})
	if err != nil {
		t.Fatal(err)
	}
	// The webview has no uint64. Everything it parses out of a JSON number is
	// a double, and this is where the low bits go.
	var parsed struct {
		Rev float64 `json:"rev"`
	}
	if err := json.Unmarshal(wire, &parsed); err != nil {
		t.Fatal(err)
	}
	arg, err := json.Marshal(parsed.Rev)
	if err != nil {
		t.Fatal(err)
	}
	var back uint64
	if err := json.Unmarshal(arg, &back); err != nil {
		// A double large enough to stringify in exponent form does not even
		// decode into a uint64 — the other half of the same failure.
		t.Fatalf("sinceRev %s could not be decoded back into a uint64: %v", arg, err)
	}
	return back
}

// The regression this file exists for. §7.2 gates the layout half on
// `sinceRev == out.Rev`, and a rev the client cannot hold exactly compares
// unequal to itself forever: `unchanged` never fires, every 1.5s poll ships the
// full node and edge payload, and the client re-runs setTopology — which clears
// the card Map and replaceChildren()s both layers. That is the re-layout §7.3
// forbids, reached through the wire type rather than through the render code.
//
// The pre-existing rev test passes full.Rev straight back in Go, so it went
// green against a 64-bit rev that no client could ever return.
func TestArchRev_survivesTheWebviewRoundTrip(t *testing.T) {
	a := archApp(t)
	seedArchRun(t, a, visRoot, secretManifest(visRepoLbl))

	full := a.OrchestrationSnapshot(visRoot, 0, 0)
	if full.Rev == 0 {
		t.Fatal("first snapshot carried no rev")
	}
	if got := webviewRoundTrip(t, full.Rev); got != full.Rev {
		t.Fatalf("rev %d came back from the client as %d", full.Rev, got)
	}
	// And the gate must actually fire on the round-tripped value, which is the
	// behaviour the user sees.
	same := a.OrchestrationSnapshot(visRoot, 0, webviewRoundTrip(t, full.Rev))
	if !same.Unchanged || len(same.Nodes) != 0 {
		t.Fatalf("the layout half was re-sent for a rev the client returned verbatim: %+v", same)
	}
}

// jsSafeRev's two properties, stated as a table so neither can be dropped
// while the other keeps the file green.
func TestJSSafeRev(t *testing.T) {
	tests := []struct {
		name string
		in   uint64
		want uint64
	}{
		{"zero stays out of the client's 'I have nothing'", 0, 1},
		{"a value that truncates to zero also avoids it", uint64(1) << 53, 1},
		{"a small value is untouched", 42, 42},
		{"the largest exactly-representable value is untouched", (uint64(1) << 53) - 1, (uint64(1) << 53) - 1},
		{"a 64-bit value is truncated into range", ^uint64(0), (uint64(1) << 53) - 1},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := jsSafeRev(tc.in)
			if got != tc.want {
				t.Fatalf("jsSafeRev(%d) = %d, want %d", tc.in, got, tc.want)
			}
			if rt := webviewRoundTrip(t, got); rt != got {
				t.Fatalf("jsSafeRev(%d) = %d did not round-trip (got %d)", tc.in, got, rt)
			}
		})
	}
}

// --- §7.4: the document freshness probe -------------------------------------

// ProjectDocumentsRev must be cheap, must move when the rendered set would,
// and must be a constant for a hidden project. The last is §3.1.1: a number
// that moved would report that a hidden client's files were being edited, one
// bit per poll.
func TestProjectDocumentsRev(t *testing.T) {
	a := archApp(t)

	// The fixture roots are synthetic paths; give the visible one a real tree.
	dir := t.TempDir()
	if err := a.st.UpsertProject(store.Project{
		Root: dir, Name: "Docs", Origin: "discovered", CreatedAt: 1, UpdatedAt: 1,
	}); err != nil {
		t.Fatal(err)
	}
	arch := filepath.Join(dir, "docs", "ARCHITECTURE.md")
	if err := os.MkdirAll(filepath.Dir(arch), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(arch, []byte("# Arch\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	before := a.ProjectDocumentsRev(dir)
	if before == 0 {
		t.Fatal("a visible project with a document returned the hidden constant")
	}
	if got := a.ProjectDocumentsRev(dir); got != before {
		t.Fatalf("rev is not stable across calls: %d then %d", before, got)
	}
	if rt := webviewRoundTrip(t, before); rt != before {
		t.Fatalf("document rev %d does not survive the client round-trip (got %d)", before, rt)
	}

	// A new decision record is the case the probe exists for: nothing already
	// known to the client changed, so only a directory read can see it.
	if err := os.MkdirAll(filepath.Join(dir, "docs", "decisions"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "docs", "decisions", "0001-a.md"),
		[]byte("# One\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if after := a.ProjectDocumentsRev(dir); after == before {
		t.Fatal("a new decision record did not move the document rev")
	}

	// §3.1: hidden and unattributable both return the same constant, and it is
	// the same one an empty project returns — there is nothing to compare.
	if err := a.SetProjectHidden(dir, true); err != nil {
		t.Fatal(err)
	}
	if got := a.ProjectDocumentsRev(dir); got != 0 {
		t.Fatalf("hidden project returned rev %d, want 0", got)
	}
	if got := a.ProjectDocumentsRev("/no/such/project"); got != 0 {
		t.Fatalf("unattributable root returned rev %d, want 0", got)
	}
	// A degraded App must not panic on the poll path.
	if got := (&App{}).ProjectDocumentsRev(dir); got != 0 {
		t.Fatalf("bare App returned rev %d, want 0", got)
	}
}

// The probe and the payload must be built from the SAME request. A probe over a
// different tree set would report "fresh" while the rendered set was stale, and
// there would be no symptom pointing at the disagreement.
func TestProjectDocumentsRev_agreesWithProjectDocuments(t *testing.T) {
	a := archApp(t)
	dir := t.TempDir()
	repo := t.TempDir()
	if err := a.st.UpsertProject(store.Project{
		Root: dir, Name: "Docs", Origin: "discovered", CreatedAt: 1, UpdatedAt: 1,
	}); err != nil {
		t.Fatal(err)
	}
	if err := a.st.UpsertProjectRepo(store.ProjectRepo{
		Path: repo, ProjectRoot: dir, Label: "api", AddedAt: 1,
	}); err != nil {
		t.Fatal(err)
	}
	write := func(root, name, body string) {
		p := filepath.Join(root, "docs", name)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write(dir, "ARCHITECTURE.md", "# Root\n")
	write(repo, "ARCHITECTURE.md", "# Member\n")

	if n := len(a.ProjectDocuments(dir).Documents); n != 2 {
		t.Fatalf("payload carries %d documents, want 2 (root + member repo)", n)
	}
	before := a.ProjectDocumentsRev(dir)
	// A member repo's document is in the payload, so it must be in the rev.
	write(repo, "ARCHITECTURE.md", "# Member, rewritten at a different length\n")
	if after := a.ProjectDocumentsRev(dir); after == before {
		t.Fatal("a member repo's document did not move the rev, but it is in the payload")
	}
}

// --- §10: the node inspector's memory block ---------------------------------

// SessionMemory reads what the indexer already wrote and computes nothing.
// The cases that matter are the ones the previous fallback got wrong: a RUNNING
// child (which ListRecent's summary never covers), a RESUMED child (whose tmux
// name changed), and a hidden one.
func TestSessionMemory(t *testing.T) {
	tests := []struct {
		name string
		// seed writes the session and transcript rows.
		seed func(t *testing.T, a *App)
		// ask is the session NAME the inspector would pass; "" means the
		// default child. Named because the resume case deliberately asks for a
		// different tmux name than the one the transcript was written under.
		ask  string
		want SessionMemoryDTO
	}{
		{
			name: "a running child's ask and outcome are readable",
			seed: func(t *testing.T, a *App) {
				putSession(t, a, "loom-child", "cs-1", visRepo, -1, "running")
				putTranscript(t, a, "cs-1", "Extract the account schema", "wrote db/schema.sql", "db/schema.sql")
			},
			want: SessionMemoryDTO{
				Ask: "Extract the account schema", Outcome: "wrote db/schema.sql", Files: "db/schema.sql",
			},
		},
		{
			name: "a resumed child resolves through the claude session id",
			// The whole reason this does not key on the tmux name: a resume
			// mints a new one (ARCHITECTURE §4.1), and the transcript row is
			// keyed on the claude id that survived it.
			seed: func(t *testing.T, a *App) {
				putSession(t, a, "loom-original", "cs-resume", visRepo, 200, "done")
				putSession(t, a, "loom-resumed", "cs-resume", visRepo, -1, "running")
				putTranscript(t, a, "cs-resume", "Ledger v2", "still working", "internal/ledger/x.go")
			},
			ask:  "loom-resumed",
			want: SessionMemoryDTO{Ask: "Ledger v2", Outcome: "still working", Files: "internal/ledger/x.go"},
		},
		{
			name: "a session with no transcript yet is empty, not an error",
			seed: func(t *testing.T, a *App) {
				putSession(t, a, "loom-child", "cs-1", visRepo, -1, "running")
			},
			want: SessionMemoryDTO{},
		},
		{
			name: "a session with no claude id yet is empty",
			seed: func(t *testing.T, a *App) {
				putSession(t, a, "loom-child", "", visRepo, -1, "spawning")
			},
			want: SessionMemoryDTO{},
		},
		{
			name: "a deleted transcript file is SAID, not rendered as silence",
			seed: func(t *testing.T, a *App) {
				putSession(t, a, "loom-child", "cs-1", visRepo, 300, "done")
				putTranscript(t, a, "cs-1", "Ledger v2", "done", "a.go")
				if err := a.st.SetFileMissing("cs-1", true); err != nil {
					t.Fatal(err)
				}
			},
			want: SessionMemoryDTO{Ask: "Ledger v2", Outcome: "done", Files: "a.go", Missing: true},
		},
		{
			name: "an unknown session name yields the empty DTO",
			seed: func(t *testing.T, a *App) {},
			want: SessionMemoryDTO{},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			a := archApp(t)
			tc.seed(t, a)
			name := tc.ask
			if name == "" {
				name = "loom-child"
			}
			got := a.SessionMemory(name)
			if got != tc.want {
				t.Fatalf("SessionMemory = %+v, want %+v", got, tc.want)
			}
		})
	}
}

// §3.1/§6: a session inside a hidden project returns the bare marker, and the
// marker carries none of the text. The ask and outcome are agent-authored prose
// about a client's work — exactly the class §3.1.4 keeps off a shared screen.
func TestSessionMemory_hiddenIsTheBareMarker(t *testing.T) {
	a := archApp(t)
	putSession(t, a, "loom-child", "cs-1", hidRepo, -1, "running")
	putTranscript(t, a, "cs-1", secretTitle, secretCheckOut, secretBrief)

	if err := a.SetProjectHidden(hidRoot, true); err != nil {
		t.Fatal(err)
	}
	got := a.SessionMemory("loom-child")
	if !got.Hidden {
		t.Fatalf("a hidden project's session memory was served: %+v", got)
	}
	if got != (SessionMemoryDTO{Hidden: true}) {
		t.Fatalf("the hidden marker carried other fields: %+v", got)
	}
	// And the marshalled form must contain none of the marker strings, which is
	// the assertion that would catch a field added later without a gate.
	blob := mustJSON(t, got)
	for _, s := range []string{secretTitle, secretCheckOut, secretBrief} {
		if strings.Contains(blob, s) {
			t.Fatalf("hidden payload leaked %q: %s", s, blob)
		}
	}
}

// A delegation child's cwd is a worktree under ~/.loom, which matches no
// project target. The bare resolver fails closed on it, so the gate has to be
// the attributor's (delegation §14.1) or this block is permanently empty for
// every child the moment anything is hidden — the exact surface it is for.
func TestSessionMemory_delegationChildIsNotFailedClosed(t *testing.T) {
	a := archApp(t)
	run := seedArchRun(t, a, visRoot, secretManifest(visRepoLbl))

	if err := a.st.Upsert(store.SessionRow{
		Name: "loom-child", ClaudeSessionID: "cs-1",
		Cwd:       filepath.Join(t.TempDir(), "wt", "acme-schema"), // a worktree, not a project path
		CreatedAt: 100, EndedAt: -1, ExitCode: -1, LastStatus: "running",
		Delegation: strconv.FormatInt(run.ID, 10) + ":acme-schema",
	}); err != nil {
		t.Fatal(err)
	}
	putTranscript(t, a, "cs-1", "Extract the account schema", "in progress", "db/schema.sql")

	// Hide the OTHER project, so the resolver is in its filtering mode.
	if err := a.SetProjectHidden(hidRoot, true); err != nil {
		t.Fatal(err)
	}
	got := a.SessionMemory("loom-child")
	if got.Hidden || got.Ask == "" {
		t.Fatalf("a delegation child's memory was suppressed: %+v", got)
	}
}

// A degraded App must not panic: this is reachable from an inspector click.
func TestSessionMemory_degradedAppIsEmpty(t *testing.T) {
	if got := (&App{}).SessionMemory("loom-child"); got != (SessionMemoryDTO{}) {
		t.Fatalf("bare App returned %+v", got)
	}
	a := archApp(t)
	if got := a.SessionMemory(""); got != (SessionMemoryDTO{}) {
		t.Fatalf("empty name returned %+v", got)
	}
}

func putSession(t *testing.T, a *App, name, claudeID, cwd string, endedAt int64, status string) {
	t.Helper()
	if err := a.st.Upsert(store.SessionRow{
		Name: name, ClaudeSessionID: claudeID, Cwd: cwd,
		CreatedAt: 100, EndedAt: endedAt, ExitCode: -1, LastStatus: status,
	}); err != nil {
		t.Fatal(err)
	}
}

func putTranscript(t *testing.T, a *App, claudeID, ask, outcome, files string) {
	t.Helper()
	if err := a.st.UpsertTranscript(store.Transcript{
		SessionID: claudeID, Ask: ask, Outcome: outcome, Files: files,
		FirstTS: 100, LastTS: 200, MsgCount: 4,
	}); err != nil {
		t.Fatal(err)
	}
}
