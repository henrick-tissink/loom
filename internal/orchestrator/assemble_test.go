package orchestrator

import (
	"strings"
	"testing"
)

// sections splits an assembled brief into heading → body, so a test can assert
// that changing one input changed EXACTLY one section (§13's determinism case).
func sections(text string) map[string]string {
	out := map[string]string{}
	for _, chunk := range strings.Split(text, "\n## ")[1:] {
		nl := strings.Index(chunk, "\n")
		if nl < 0 {
			continue
		}
		out[chunk[:nl]] = chunk[nl+1:]
	}
	return out
}

func note(name, content string, preExisting bool) noteFile {
	b := []byte(content)
	return noteFile{
		Name: name, Path: "/notes/" + name, Content: content,
		Bytes: len(b), SHA256: digest(b), Exists: true, PreExisting: preExisting,
	}
}

func baseInput() Input {
	return Input{
		ProjectName: "Innostream",
		Root:        "/w/Innostream",
		NotesDir:    "/notes",
		Repos: []RepoState{
			{Label: "bankenstein", Path: "/w/Innostream/bankenstein", Branch: "main", Head: "9b69827aaaaaaaa", Dirty: 2},
			{Label: "sidecar", Path: "/elsewhere/sidecar", Branch: "dev", Head: "a41f0c2bbbbbbbb"},
		},
		Notes: []noteFile{
			note("loom-map.md", "# map\ntwo repos\n", false),
			{Name: "loom-decisions.md", Path: "/notes/loom-decisions.md"},
			{Name: "loom-open.md", Path: "/notes/loom-open.md"},
		},
		Drift:  []string{"bankenstein: 14 commits since the last brief (9b69827 → a41f0c2)"},
		Recent: []RecentRow{{SessionID: "s1", LastTS: 1752000000, Label: "bankenstein", Title: "fix auth", Outcome: "done"}},
		Intent: "map the payment seam",
		Now:    fixedNow,
	}
}

// TestAssembleDeterministic pins §5.3: assembly is a pure function of its
// inputs, so the same inputs produce a byte-identical brief. Without this the
// brief digest in state.json means nothing and no golden test is possible.
func TestAssembleDeterministic(t *testing.T) {
	a := Assemble(baseInput())
	b := Assemble(baseInput())
	if a.Text != b.Text {
		t.Fatal("two assemblies of identical inputs differ")
	}
	if a.SHA256 != b.SHA256 || a.Bytes != len(a.Text) {
		t.Fatalf("digest/bytes inconsistent: %d %q", a.Bytes, a.SHA256)
	}
}

// TestAssembleOneChangedInputChangesOneSection is the other half of the
// determinism claim: a changed input must move the section it belongs to and
// nothing else, or the golden test cannot localize a regression.
func TestAssembleOneChangedInputChangesOneSection(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(*Input)
		section string
	}{
		{"intent", func(in *Input) { in.Intent = "something else entirely" }, SecWhat},
		{"repo head", func(in *Input) { in.Repos[0].Head = "ffffffffffff" }, SecProj},
		{"drift", func(in *Input) { in.Drift = []string{"notes edited (expected): loom-map.md"} }, SecDrift},
		{"notes", func(in *Input) { in.Notes[0] = note("loom-map.md", "# map\nthree repos\n", false) }, SecNotes},
		{"recent", func(in *Input) { in.Recent[0].Outcome = "reverted" }, SecRecent},
	}
	basis := sections(Assemble(baseInput()).Text)
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			in := baseInput()
			tc.mutate(&in)
			got := sections(Assemble(in).Text)
			for name, body := range got {
				changed := body != basis[name]
				if name == tc.section && !changed {
					t.Fatalf("section %q did not change", name)
				}
				if name != tc.section && changed {
					t.Fatalf("section %q changed unexpectedly:\n%s\n---\n%s", name, basis[name], body)
				}
			}
		})
	}
}

// TestAuthorizationScopeFirstLastAndIntact pins §4.1 and slice 1 §11: the scope
// is first, repeated last, and carries all five claims. Removing scope text
// measurably raises overreach, so this is the assertion the whole authorization
// story rests on.
func TestAuthorizationScopeFirstLastAndIntact(t *testing.T) {
	in := baseInput()
	text := Assemble(in).Text
	first := strings.Index(text, "## "+SecScope+"\n")
	last := strings.Index(text, "## "+SecScope+" (repeated)\n")
	if first < 0 || last < 0 || first >= last {
		t.Fatalf("scope not first-and-last: %d %d", first, last)
	}
	// Every heading between them must come after the first and before the last.
	for _, sec := range []string{SecProj, SecDrift, SecNotes, SecRecent, SecWhat} {
		i := strings.Index(text, "## "+sec+"\n")
		if i < first || i > last {
			t.Fatalf("section %q is outside the scope bookends", sec)
		}
	}
	for _, claim := range []string{
		"The only directory you may write to is /notes",
		"and within it only loom-map.md, loom-decisions.md, loom-open.md",
		"These repos are readable:",
		"may not commit, push, rebase",
		"may not start, resume, or kill other sessions",
		"Delegation does not exist yet",
		"say so and stop. Do not route around it.",
	} {
		if strings.Count(text, claim) != 2 {
			t.Fatalf("claim %q appears %d times, want 2", claim, strings.Count(text, claim))
		}
	}
}

// TestNotesBudget pins §4.4's all-or-nothing rule: under the cap the three
// files go in verbatim; over it EACH is head-truncated with the marker and its
// absolute path, so the agent can fetch the rest itself.
func TestNotesBudget(t *testing.T) {
	tests := []struct {
		name     string
		each     int
		verbatim bool
	}{
		{"under cap inline verbatim", 23 * 1024 / 3, true},
		{"over cap head-truncated", 25 * 1024 / 3, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			in := baseInput()
			in.Notes = nil
			for _, n := range NoteFiles {
				in.Notes = append(in.Notes, note(n, strings.Repeat("x", tc.each), false))
			}
			body := sections(Assemble(in).Text)[SecNotes]
			if tc.verbatim {
				if strings.Contains(body, truncMarker) {
					t.Fatal("notes under the cap were truncated")
				}
				if !strings.Contains(body, strings.Repeat("x", tc.each)) {
					t.Fatal("notes under the cap were not inlined verbatim")
				}
				return
			}
			for _, n := range NoteFiles {
				want := truncMarker + " — read the rest at /notes/" + n
				if !strings.Contains(body, want) {
					t.Fatalf("missing truncation marker + abs path for %s", n)
				}
			}
			if len(body) > notesCap {
				t.Fatalf("notes section %d bytes > cap %d", len(body), notesCap)
			}
		})
	}
}

// TestPathologicalNotesKeepScope is §13's explicit worst case: a 500 KB note
// must not cost the brief its authorization scope, its 64 KB ceiling, or the
// visibility of the fact that something was cut.
func TestPathologicalNotesKeepScope(t *testing.T) {
	in := baseInput()
	in.Notes = []noteFile{
		note("loom-map.md", strings.Repeat("y", 500*1024), false),
		{Name: "loom-decisions.md", Path: "/notes/loom-decisions.md"},
		{Name: "loom-open.md", Path: "/notes/loom-open.md"},
	}
	br := Assemble(in)
	if br.Bytes > briefCap {
		t.Fatalf("brief %d bytes exceeds the 64 KB cap", br.Bytes)
	}
	if strings.Count(br.Text, "You are the orchestrator for the project") != 2 {
		t.Fatal("scope lost or duplicated under pathological truncation")
	}
	if !strings.Contains(br.Text, truncMarker) {
		t.Fatal("truncation was silent")
	}
	if !contains(br.Truncated, SecNotes) {
		t.Fatalf("truncated sections %v does not name Notes", br.Truncated)
	}
}

// TestRecentWorkBudget pins BOTH of §5.2's caps. Rows are capped upstream (in
// filterRecent); bytes are capped here, and the section says how many it
// dropped rather than ending mid-thought.
func TestRecentWorkBudget(t *testing.T) {
	in := baseInput()
	in.Recent = nil
	for i := 0; i < maxRecentRows; i++ {
		in.Recent = append(in.Recent, RecentRow{
			SessionID: "s", LastTS: 1752000000, Label: "bankenstein",
			Title: "t", Outcome: strings.Repeat("z", 1024),
		})
	}
	body := sections(Assemble(in).Text)[SecRecent]
	if len(body) > recentCap {
		t.Fatalf("recent section %d bytes > cap %d", len(body), recentCap)
	}
	if !strings.Contains(body, "older sessions omitted") {
		t.Fatal("dropped rows were not disclosed")
	}
	for _, line := range strings.Split(strings.TrimSpace(body), "\n") {
		if strings.HasPrefix(line, "- ") && !strings.HasSuffix(line, strings.Repeat("z", 1024)) {
			t.Fatal("a session line was cut mid-line")
		}
	}
}

// TestBootstrapWording pins §8's "first-ever spawn is not a special mode": no
// notes changes the WORDING, not the code path.
func TestBootstrapWording(t *testing.T) {
	in := baseInput()
	in.Notes = []noteFile{
		{Name: "loom-map.md", Path: "/notes/loom-map.md"},
		{Name: "loom-decisions.md", Path: "/notes/loom-decisions.md"},
		{Name: "loom-open.md", Path: "/notes/loom-open.md"},
	}
	secs := sections(Assemble(in).Text)
	if !strings.Contains(secs[SecNotes], "none yet") {
		t.Fatalf("Notes section: %q", secs[SecNotes])
	}
	if !strings.HasPrefix(secs[SecWhat], "This project has no notes yet. Write loom-map.md first") {
		t.Fatalf("What to do did not lead with the bootstrap instruction: %q", secs[SecWhat])
	}
}

// TestPreExistingNoteLabelled pins §3: a file at notes_dir that state.json does
// not know about is included verbatim and labelled, because Loom never
// truncates or moves a file it did not create.
func TestPreExistingNoteLabelled(t *testing.T) {
	in := baseInput()
	in.Notes[0] = note("loom-map.md", "hand written\n", true)
	body := sections(Assemble(in).Text)[SecNotes]
	if !strings.Contains(body, "pre-existing — treat as authoritative, do not rewrite wholesale") {
		t.Fatalf("pre-existing label missing: %q", body)
	}
	if !strings.Contains(body, "hand written") {
		t.Fatal("pre-existing note not included verbatim")
	}
}

// TestProjectSectionShape pins §4.2: one line per repo, and a missing or
// unreadable repo says so rather than vanishing.
func TestProjectSectionShape(t *testing.T) {
	in := baseInput()
	in.Repos = append(in.Repos,
		RepoState{Label: "gone", Path: "/w/gone", Missing: true},
		RepoState{Label: "broken", Path: "/w/broken", Err: "not a git repository"})
	body := sections(Assemble(in).Text)[SecProj]
	for _, want := range []string{
		"- bankenstein · /w/Innostream/bankenstein · main · 9b69827a · 2 dirty",
		"- gone · /w/gone · missing",
		"- broken · /w/broken · " + unavailable,
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("missing %q in:\n%s", want, body)
		}
	}
}

// TestSeed pins §6: one physical line, well under the cap, newline-free even
// from a multi-line paste, intent capped at 600 chars, and a standing tail when
// there is no intent.
func TestSeed(t *testing.T) {
	const brief = "/home/h/.loom/projects/Innostream-9f2c1ab4/brief.md"
	tests := []struct {
		name   string
		intent string
		want   string
		absent string
	}{
		{"empty intent gets the standing tail", "",
			"Then follow the brief's \"What to do\" section.", "Then: "},
		{"intent is appended", "map the payment seam", "Then: map the payment seam", ""},
		{"multi-line paste is flattened", "line one\nline two\r\nline three",
			"Then: line one line two line three", ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s := Seed(brief, tc.intent)
			if strings.ContainsAny(s, "\n\r") {
				t.Fatalf("seed contains a newline — it would submit early: %q", s)
			}
			if len(s) >= seedCap {
				t.Fatalf("seed %d bytes >= cap %d", len(s), seedCap)
			}
			if !strings.Contains(s, "Read "+brief+" first") {
				t.Fatalf("seed does not point at the brief: %q", s)
			}
			if !strings.Contains(s, "Authorization scope") {
				t.Fatalf("seed does not name the scope section: %q", s)
			}
			if !strings.Contains(s, tc.want) {
				t.Fatalf("seed %q lacks %q", s, tc.want)
			}
			if tc.absent != "" && strings.Contains(s, tc.absent) {
				t.Fatalf("seed %q contains %q", s, tc.absent)
			}
		})
	}
}

// TestSeedCapsLongIntent pins the 600-character intent cap on both the seed and
// the brief — a 5 KB paste must not become a 5 KB seed.
func TestSeedCapsLongIntent(t *testing.T) {
	long := strings.Repeat("q", 5*1024)
	s := Seed("/b/brief.md", long)
	if n := strings.Count(s, "q"); n != intentCap {
		t.Fatalf("intent contributed %d chars, want %d", n, intentCap)
	}
	in := baseInput()
	in.Intent = long
	if n := strings.Count(sections(Assemble(in).Text)[SecWhat], "q"); n != intentCap {
		t.Fatalf("brief intent contributed %d chars, want %d", n, intentCap)
	}
}

// TestStandingInstructionAlwaysPresent pins §4.6's "always": the instruction to
// reconcile drift before new analysis is what makes drift reporting worth
// anything.
func TestStandingInstructionAlwaysPresent(t *testing.T) {
	for _, intent := range []string{"", "do a thing"} {
		in := baseInput()
		in.Intent = intent
		body := sections(Assemble(in).Text)[SecWhat]
		if !strings.Contains(body, "Reconcile anything listed under Drift before starting new analysis.") {
			t.Fatalf("standing instruction missing for intent %q", intent)
		}
		if !strings.Contains(body, "Record every decision in loom-decisions.md as it is made") {
			t.Fatalf("decision-log instruction missing for intent %q", intent)
		}
	}
}

// TestHeadTruncRuneBoundary guards the truncation helper against splitting a
// multi-byte character into a replacement glyph — the brief is read by an LLM
// and a mojibake tail is noise it will try to interpret.
func TestHeadTruncRuneBoundary(t *testing.T) {
	s := strings.Repeat("é", 10) // 2 bytes each
	for limit := 0; limit <= len(s); limit++ {
		got := headTrunc(s, limit)
		if !strings.HasPrefix(s, got) || len(got)%2 != 0 {
			t.Fatalf("headTrunc(%d) split a rune: %q", limit, got)
		}
	}
}
