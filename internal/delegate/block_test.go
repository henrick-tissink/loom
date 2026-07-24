package delegate

import (
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/henricktissink/loom/internal/store"
)

// §11.1's declaration is the ONLY machine-readable thing a blocked child
// produces, and §11.2 forbids a silent ignore. So the parser is tested from both
// ends: what it must accept (including sloppy but recoverable input) and what it
// must refuse loudly.
func TestParseBlockAcceptsRecoverableInputAndRefusesTheRest(t *testing.T) {
	tests := []struct {
		name    string
		raw     string
		wantErr bool
		check   func(*testing.T, Block)
	}{
		{
			name: "the spec's example verbatim",
			raw: `{ "block": 1,
			  "run": "atlas-rearchitecture-7",
			  "task": "auth-api",
			  "at": "2026-07-22T14:03:11Z",
			  "kind": "needs-artifact",
			  "need": { "artifact": "account-schema", "from": "schema" },
			  "summary": "auth handler needs the account table's final column set",
			  "detail": "internal/auth/session.go must read account.tenant_id",
			  "resume_when": "artifact account-schema is published" }`,
			check: func(t *testing.T, b Block) {
				if b.Kind != BlockNeedsArtifact || b.Task != "auth-api" || b.Need.Artifact != "account-schema" || b.Need.From != "schema" {
					t.Fatalf("decoded wrong: %+v", b)
				}
				if b.At.UTC().Format(time.RFC3339) != "2026-07-22T14:03:11Z" {
					t.Fatalf("At = %v", b.At)
				}
				if b.Author != AuthorChild {
					t.Fatalf("Author = %q, want the child (absent)", b.Author)
				}
			},
		},
		{
			// The common unforeseen-dependency case §11.3 exists for. A parser
			// that rejected an artifact nobody declares would reject exactly the
			// blocks this mechanism was built to carry.
			name:  "an artifact that appears in no manifest",
			raw:   `{"block":1,"task":"auth-api","kind":"needs-artifact","need":{"artifact":"nowhere","from":"nobody"}}`,
			check: func(t *testing.T, b Block) {},
		},
		{
			name: "a missing version is the current one",
			raw:  `{"task":"auth-api","kind":"needs-decision"}`,
			check: func(t *testing.T, b Block) {
				if b.Version != BlockVersion {
					t.Fatalf("Version = %d, want %d", b.Version, BlockVersion)
				}
			},
		},
		{
			// A timestamp is rendered and never scheduled on. Losing a real park
			// to a cosmetic field is the silent failure §11.2 forbids.
			name: "an unparseable timestamp costs the timestamp, not the block",
			raw:  `{"block":1,"task":"auth-api","kind":"blocked-external","at":"yesterday afternoon"}`,
			check: func(t *testing.T, b Block) {
				if !b.At.IsZero() {
					t.Fatalf("At = %v, want zero", b.At)
				}
				if b.Kind != BlockExternal {
					t.Fatalf("Kind = %q", b.Kind)
				}
			},
		},
		{
			name: "unknown fields are ignored, not rejected",
			raw:  `{"block":1,"task":"auth-api","kind":"needs-scope","confidence":0.4,"paths":["internal/db/**"]}`,
			check: func(t *testing.T, b Block) {
				if len(b.Paths) != 1 || b.Paths[0] != "internal/db/**" {
					t.Fatalf("Paths = %v", b.Paths)
				}
			},
		},
		{name: "not JSON at all", raw: "I am blocked on the schema task.", wantErr: true},
		{name: "empty", raw: "   \n", wantErr: true},
		{name: "no kind", raw: `{"block":1,"task":"auth-api"}`, wantErr: true},
		{name: "a kind nobody defined", raw: `{"block":1,"task":"auth-api","kind":"needs-coffee"}`, wantErr: true},
		{name: "no task", raw: `{"block":1,"kind":"needs-decision"}`, wantErr: true},
		{name: "a version this build cannot read", raw: `{"block":2,"task":"auth-api","kind":"needs-decision"}`, wantErr: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			b, err := ParseBlock([]byte(tc.raw))
			if tc.wantErr {
				if err == nil {
					t.Fatalf("ParseBlock(%q) = %+v, want an error", tc.raw, b)
				}
				return
			}
			if err != nil {
				t.Fatalf("ParseBlock: %v", err)
			}
			tc.check(t, b)
		})
	}
}

// blockFixture is a one-run, one-task store plus a meta dir on disk. It is
// deliberately NOT newFixture: nothing here needs a git repo or a launcher, and
// a fixture that builds one would make these tests fail for reasons that have
// nothing to do with the rendezvous path.
type blockFixture struct {
	st  *store.Store
	run store.DelegationRun
	m   Manifest
	d   *Detector
}

func newBlockFixture(t *testing.T, state TaskState) *blockFixture {
	t.Helper()
	st := openStore(t)
	run, err := st.InsertDelegationRun("atlas", "/w/innostream", "{}", "{}", 1000)
	if err != nil {
		t.Fatal(err)
	}
	m := Manifest{
		Name:  "atlas",
		Tasks: []Task{{ID: "auth-api", Repo: "bankenstein"}},
	}
	if err := st.InsertDelegationTask(store.DelegationTask{
		RunID: run.ID, TaskID: "auth-api", State: string(state), RepoLabel: "bankenstein", UpdatedAt: 1000,
	}); err != nil {
		t.Fatal(err)
	}
	layout := NewLayout(t.TempDir())
	if err := os.MkdirAll(layout.MetaDir(run.Slug, "bankenstein", "auth-api"), 0o755); err != nil {
		t.Fatal(err)
	}
	return &blockFixture{st: st, run: run, m: m,
		d: &Detector{Layout: layout, Store: st, Now: func() time.Time { return time.Unix(2000, 0) }}}
}

func (f *blockFixture) writeBlock(t *testing.T, raw string) {
	t.Helper()
	if err := os.WriteFile(f.d.Layout.BlockPath(f.run.Slug, "bankenstein", "auth-api"), []byte(raw), 0o644); err != nil {
		t.Fatal(err)
	}
}

func (f *blockFixture) row(t *testing.T) store.DelegationTask {
	t.Helper()
	row, ok, err := f.st.GetDelegationTask(f.run.ID, "auth-api")
	if err != nil || !ok {
		t.Fatalf("task row missing (ok=%v err=%v)", ok, err)
	}
	return row
}

// §11.2: the FILE is the trigger and the task moves running → blocked under CAS.
// The state assertion is the one that matters — a detector that returned the
// event without parking the task leaves Loom believing a stopped child is working.
func TestBlockDetectorParksTheTaskAndRecordsTheDeclaration(t *testing.T) {
	f := newBlockFixture(t, StateRunning)
	f.writeBlock(t, `{"block":1,"task":"auth-api","kind":"needs-artifact","need":{"artifact":"account-schema","from":"schema"},"summary":"needs the column set"}`)

	events, err := f.d.Poll(f.run, f.m)
	if err != nil {
		t.Fatalf("Poll: %v", err)
	}
	if len(events) != 1 || events[0].Block.Kind != BlockNeedsArtifact || events[0].Malformed != nil {
		t.Fatalf("events = %+v", events)
	}
	row := f.row(t)
	if row.State != string(StateBlocked) {
		t.Fatalf("state = %q, want blocked", row.State)
	}
	if !strings.Contains(row.BlockJSON, "account-schema") {
		t.Fatalf("block_json = %q, want the raw declaration", row.BlockJSON)
	}
	if DecodeFlags(row.Flags)[FlagBlockMalformed] {
		t.Fatal("block-malformed set for a well-formed declaration")
	}

	// The fingerprint is what keeps a 2s poll from re-firing the transition
	// forever. A second event here would mean every tick re-parks a task.
	again, err := f.d.Poll(f.run, f.m)
	if err != nil {
		t.Fatalf("second Poll: %v", err)
	}
	if len(again) != 0 {
		t.Fatalf("second Poll re-fired: %+v", again)
	}
}

// §11.2's loud degrade. A swallowed block is a child parked forever with nobody
// told, so the assertions are: the raw text survives, the flag is set, and the
// task is STILL parked — Loom's view must not disagree with the only observable
// fact, which is a child sitting at a prompt.
func TestBlockDetectorFlagsAMalformedDeclarationWithTheRawText(t *testing.T) {
	f := newBlockFixture(t, StateRunning)
	f.writeBlock(t, "I am blocked: the account table has no tenant_id.")

	events, err := f.d.Poll(f.run, f.m)
	if err != nil {
		t.Fatalf("Poll: %v", err)
	}
	if len(events) != 1 || events[0].Malformed == nil {
		t.Fatalf("events = %+v, want one malformed", events)
	}
	if !strings.Contains(events[0].Malformed.Raw, "tenant_id") {
		t.Fatalf("raw content lost: %q", events[0].Malformed.Raw)
	}
	row := f.row(t)
	if !DecodeFlags(row.Flags)[FlagBlockMalformed] {
		t.Fatalf("flags = %q, want block-malformed", row.Flags)
	}
	if row.State != string(StateBlocked) {
		t.Fatalf("state = %q, want blocked even for a malformed declaration", row.State)
	}
	if !strings.Contains(row.BlockJSON, "tenant_id") {
		t.Fatalf("block_json = %q, want the raw text kept for the human", row.BlockJSON)
	}
}

// A declaration that disappears is an EVENT: a task sitting in `blocked` with
// nothing on disk is otherwise indistinguishable from one legitimately parked.
func TestBlockDetectorReportsAClearedDeclaration(t *testing.T) {
	f := newBlockFixture(t, StateRunning)
	f.writeBlock(t, `{"block":1,"task":"auth-api","kind":"needs-decision"}`)
	if _, err := f.d.Poll(f.run, f.m); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(f.d.Layout.BlockPath(f.run.Slug, "bankenstein", "auth-api")); err != nil {
		t.Fatal(err)
	}

	events, err := f.d.Poll(f.run, f.m)
	if err != nil {
		t.Fatalf("Poll: %v", err)
	}
	if len(events) != 1 || !events[0].Cleared {
		t.Fatalf("events = %+v, want one Cleared", events)
	}
	if got := f.row(t).BlockJSON; got != "" {
		t.Fatalf("block_json = %q, want cleared", got)
	}
}

// §9.2's producer conflict and §10.3's integration failure park through the SAME
// file, parser and resume as a child-authored block. The round-trip is the
// evidence that there is one mechanism and not two.
func TestBlockWriteRoundTripsALoomAuthoredDeclaration(t *testing.T) {
	f := newBlockFixture(t, StateRunning)
	err := f.d.Write(f.run.Slug, "bankenstein", "auth-api", Block{
		Run: f.run.Slug, Kind: BlockNeedsDecision,
		Summary: "producers `schema` and `config` disagree about db/schema.sql",
	})
	if err != nil {
		t.Fatalf("Write: %v", err)
	}
	got, err := f.d.Read(f.run.Slug, "bankenstein", "auth-api")
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if got.Author != AuthorLoom {
		t.Fatalf("Author = %q, want %q", got.Author, AuthorLoom)
	}
	if got.Task != "auth-api" || got.Kind != BlockNeedsDecision || got.Version != BlockVersion {
		t.Fatalf("round trip lost fields: %+v", got)
	}
	if got.At.IsZero() {
		t.Fatal("At is zero; a Loom-authored block must be timestamped")
	}
}

// Absence is not an error: every renderer asks this question of tasks that are
// mostly not blocked, and an error return for the normal case is how an error
// return gets ignored.
func TestBlockReadOfAnAbsentDeclarationIsNotAnError(t *testing.T) {
	f := newBlockFixture(t, StateRunning)
	b, err := f.d.Read(f.run.Slug, "bankenstein", "auth-api")
	if err != nil || !b.Empty() {
		t.Fatalf("Read = (%+v, %v), want (empty, nil)", b, err)
	}
}

// A malformed declaration read directly (the restart path, where `seen` is empty)
// must still carry the raw text out — the flag is set by Poll, but the human's
// remedy is reading what the child wrote and Read is what the UI calls.
func TestBlockReadSurfacesAMalformedDeclarationWithItsText(t *testing.T) {
	f := newBlockFixture(t, StateBlocked)
	f.writeBlock(t, "{not json")
	_, err := f.d.Read(f.run.Slug, "bankenstein", "auth-api")
	var mb *MalformedBlock
	if !errors.As(err, &mb) {
		t.Fatalf("err = %v, want *MalformedBlock", err)
	}
	if !strings.Contains(mb.Raw, "not json") {
		t.Fatalf("Raw = %q", mb.Raw)
	}
}

// --- §11.3 amendments -----------------------------------------------------

// amendFixture is the two-task manifest every amendment test needs: `schema`
// produces `account-schema`, `auth-api` consumes it. One declared edge,
// schema → auth-api.
func amendFixture() (Manifest, EffectiveGraph) {
	m := Manifest{
		Name: "atlas",
		Tasks: []Task{
			{ID: "schema", Repo: "bankenstein", Produces: []Artifact{{ID: "account-schema", Path: "db/0007.sql"}}},
			{ID: "auth-api", Repo: "bankenstein", Needs: []string{"account-schema"},
				Produces: []Artifact{{ID: "auth-openapi", Path: "api/openapi.yaml"}}},
		},
	}
	return m, Effective(m, nil, nil)
}

func TestAmendmentProposalDependsOnTheBlockKind(t *testing.T) {
	m, e := amendFixture()
	tests := []struct {
		name     string
		block    Block
		wantKind AmendmentKind
		wantErr  error
		check    func(*testing.T, Amendment)
	}{
		{
			name:     "needs-artifact naming a produced artifact becomes an edge",
			block:    Block{Task: "schema", Kind: BlockNeedsArtifact, Need: BlockNeed{Artifact: "auth-openapi", From: "auth-api"}},
			wantKind: AmendEdge,
			check: func(t *testing.T, a Amendment) {
				if a.From != "auth-api" || a.Task != "schema" {
					t.Fatalf("edge = %s → %s, want auth-api → schema", a.From, a.Task)
				}
			},
		},
		{
			// Loom offers the task/artifact to add and never invents a task
			// (§16). The VALUE matters as much as the error: it is the offer.
			name:     "needs-artifact naming nothing anybody produces is a re-plan request",
			block:    Block{Task: "auth-api", Kind: BlockNeedsArtifact, Need: BlockNeed{Artifact: "tenant-model", From: "schema"}},
			wantKind: AmendReplan,
			wantErr:  ErrNoSuchArtifact,
			check: func(t *testing.T, a Amendment) {
				if a.Artifact != "tenant-model" || a.From != "schema" {
					t.Fatalf("re-plan offer = %+v, want tenant-model suggested on schema", a)
				}
			},
		},
		{
			name:     "a suggestion naming a task nobody has is dropped",
			block:    Block{Task: "auth-api", Kind: BlockNeedsArtifact, Need: BlockNeed{Artifact: "tenant-model", From: "ledger"}},
			wantKind: AmendReplan,
			wantErr:  ErrNoSuchArtifact,
			check: func(t *testing.T, a Amendment) {
				if a.From != "" {
					t.Fatalf("From = %q, want no suggestion for an unknown task", a.From)
				}
			},
		},
		{
			name:     "needs-scope becomes a proposed authorization widening",
			block:    Block{Task: "auth-api", Kind: BlockNeedsScope, Paths: []string{"internal/db/**", "internal/db/**", "cmd/**"}},
			wantKind: AmendScope,
			check: func(t *testing.T, a Amendment) {
				if len(a.Paths) != 2 || a.Paths[0] != "cmd/**" {
					t.Fatalf("Paths = %v, want sorted and de-duplicated", a.Paths)
				}
				if !a.ApprovedAt.IsZero() {
					t.Fatal("a proposal must not arrive pre-approved")
				}
			},
		},
		{
			name:  "needs-decision amends nothing",
			block: Block{Task: "auth-api", Kind: BlockNeedsDecision},
		},
		{
			name:  "blocked-external amends nothing",
			block: Block{Task: "auth-api", Kind: BlockExternal},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			a, err := Propose(e, m, tc.block)
			if tc.wantErr != nil && !errors.Is(err, tc.wantErr) {
				t.Fatalf("err = %v, want %v", err, tc.wantErr)
			}
			if tc.wantErr == nil && err != nil {
				t.Fatalf("Propose: %v", err)
			}
			if a.Kind != tc.wantKind {
				t.Fatalf("Kind = %q, want %q", a.Kind, tc.wantKind)
			}
			if tc.check != nil {
				tc.check(t, a)
			}
		})
	}
}

// BINDING (§11.3, last rule). This is the specific case where a loud block
// silently becomes a deadlock: the child stopped correctly, the human accepted an
// obvious-looking edge, and the run is now unsatisfiable with no error anywhere.
// The cycle must be REJECTED and it must be NAMED.
func TestAmendmentClosingACycleIsRejectedAndNamesTheCycle(t *testing.T) {
	m, e := amendFixture()
	// schema is blocked on auth-api's artifact. Accepting it would make
	// schema → auth-api → schema.
	a, err := Propose(e, m, Block{Task: "schema", Kind: BlockNeedsArtifact,
		Need: BlockNeed{Artifact: "auth-openapi", From: "auth-api"}})
	if err != nil {
		t.Fatalf("Propose: %v", err)
	}
	a.ApprovedAt = time.Unix(3000, 0) // a human said yes; the graph still says no

	got, err := Accept(e, a)
	if !errors.Is(err, ErrAmendmentCycle) {
		t.Fatalf("err = %v, want ErrAmendmentCycle", err)
	}
	var ce *CycleError
	if !errors.As(err, &ce) {
		t.Fatalf("err = %v, want a *CycleError to render", err)
	}
	msg := err.Error()
	for _, want := range []string{"schema", "auth-api", "account-schema", "auth-openapi", "atlas"} {
		if !strings.Contains(msg, want) {
			t.Fatalf("message %q does not name %q", msg, want)
		}
	}
	if len(got.Added) != 0 || len(got.Amendments) != 0 {
		t.Fatalf("a rejected amendment was folded into the graph: %+v", got)
	}
}

// §11.3: NEVER auto-granted. The refusal is asserted for every kind, not only
// `scope` — an unapproved edge gates a task on Loom's own say-so, which is the
// same failure wearing different clothes.
func TestAmendmentIsNeverAutoGranted(t *testing.T) {
	_, e := amendFixture()
	for _, a := range []Amendment{
		{Kind: AmendEdge, Task: "auth-api", From: "schema", Artifact: "account-schema"},
		{Kind: AmendScope, Task: "auth-api", Paths: []string{"internal/db/**"}},
		{Kind: AmendReplan, Task: "auth-api", Artifact: "tenant-model"},
	} {
		t.Run(string(a.Kind), func(t *testing.T) {
			got, err := Accept(e, a)
			if !errors.Is(err, ErrAmendmentNotApproved) {
				t.Fatalf("err = %v, want ErrAmendmentNotApproved", err)
			}
			if len(got.Added) != 0 || len(got.Amendments) != 0 {
				t.Fatalf("an unapproved amendment changed the graph: %+v", got)
			}
		})
	}
}

// The accepting half: an approved acyclic edge becomes a real edge, and the log
// keeps it. Without this the rejection test above would pass on a function that
// refuses everything.
func TestAmendmentAcceptedEdgeJoinsTheEffectiveGraph(t *testing.T) {
	m := Manifest{
		Name: "atlas",
		Tasks: []Task{
			{ID: "schema", Repo: "b", Produces: []Artifact{{ID: "account-schema"}}},
			{ID: "auth-api", Repo: "b"},
		},
	}
	e := Effective(m, nil, nil)
	a := Amendment{Kind: AmendEdge, Task: "auth-api", From: "schema", Artifact: "account-schema",
		ApprovedAt: time.Unix(3000, 0)}

	got, err := Accept(e, a)
	if err != nil {
		t.Fatalf("Accept: %v", err)
	}
	if len(got.Added) != 1 || got.Added[0] != (Edge{From: "schema", To: "auth-api", Artifact: "account-schema"}) {
		t.Fatalf("Added = %+v", got.Added)
	}
	if len(got.Amendments) != 1 {
		t.Fatalf("Amendments = %+v, want the log to keep it", got.Amendments)
	}
}

// An edge to a task this run does not have would gate a consumer on something
// that can never become verified — a park converted into a permanent one.
func TestAmendmentNamingAnUnknownTaskIsRejected(t *testing.T) {
	_, e := amendFixture()
	a := Amendment{Kind: AmendEdge, Task: "auth-api", From: "ledger", Artifact: "ledger-api",
		ApprovedAt: time.Unix(3000, 0)}
	if _, err := Accept(e, a); err == nil {
		t.Fatal("Accept accepted an edge from a task that is not in the run")
	}
}
