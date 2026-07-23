package orchestrator

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/henricktissink/loom/internal/memory"
)

// Section caps (§4, binding). The order below is the order they appear in the
// brief, and the caps are per-section, not shared: a pathological notes file
// must not be able to eat `Recent work`, and neither may touch the scope.
//
// They sum to 54 KB against a 64 KB total, so briefCap is a BACKSTOP rather
// than the usual binding constraint — which is deliberate. A total cap that
// binds first would make which section gets cut depend on how long the fixed
// headers happen to be that day.
const (
	briefCap  = 64 * 1024
	scopeCap  = 1 * 1024
	projCap   = 4 * 1024
	driftCap  = 4 * 1024
	notesCap  = 24 * 1024
	recentCap = 16 * 1024
	whatCap   = 4 * 1024

	// notesPerFileCap is what each note is head-truncated to once the three
	// together exceed notesCap (§4.4).
	notesPerFileCap = 2 * 1024

	// intentCap bounds the user's typed intent everywhere it appears — in
	// `What to do` and in the seed (§4.6, §6).
	intentCap = 600

	// seedCap is §6's hard ceiling on the seed line. An order of magnitude
	// under workflows' 15 KB send-keys cap, so that cap's logic is never near
	// its edge.
	seedCap = 2 * 1024

	truncMarker = "…[truncated]"
)

// Section names, used both as headings and as the strings recorded in
// state.json's truncated_sections (which the overview renders — §4: "a silently
// short brief is the failure mode this exists to prevent").
const (
	SecScope  = "Authorization scope"
	SecProj   = "Project"
	SecDrift  = "Drift"
	SecNotes  = "Notes"
	SecRecent = "Recent work"
	SecWhat   = "What to do"
)

// eatOrder is the order sections are cut from if the assembled whole somehow
// still exceeds briefCap. Least load-bearing first, and the scope is absent
// from the list entirely — slice 1 §11 is explicit that removing scope text
// measurably raises overreach, so it is first, repeated last, and the one
// section a truncation may never touch. `What to do` is likewise absent: it
// carries the standing instruction, without which the orchestrator does not
// know that reconciling drift comes before new analysis.
var eatOrder = []string{SecRecent, SecNotes, SecDrift, SecProj}

// Input is everything Assemble reads. Gathered by the caller so that Assemble
// itself is a pure function of (project row, repo state, notes files, drift,
// recent rows, intent, clock) — §5.3, which is what makes the golden-file test
// meaningful and what lets state.json store a brief digest that means anything.
type Input struct {
	ProjectName string
	Root        string
	NotesDir    string
	Repos       []RepoState
	Notes       []noteFile
	Drift       []string
	Recent      []RecentRow
	Intent      string
	Now         time.Time
}

// Brief is an assembled brief plus the facts state.json and the overview need
// about it.
type Brief struct {
	Text      string
	Bytes     int
	SHA256    string
	Truncated []string
}

// Assemble renders brief.md (§4). Deterministic: identical inputs produce a
// byte-identical brief.
func Assemble(in Input) Brief {
	label := "LOOM-BRIEF " + in.ProjectName + " · assembled " +
		in.Now.UTC().Format(time.RFC3339)

	scope := scopeSection(in)
	// notesSection does its own §4.4 truncation (per-file, with the marker and
	// the absolute path) rather than letting capSection cut the section as a
	// block, so it reports the cut itself. A per-file truncation that never
	// reached truncated_sections would leave the overview saying the brief was
	// complete while three notes were clipped — the silent-short-brief failure
	// mode in a different costume.
	notesBody, notesCut := notesSection(in)
	sections := []struct {
		name string
		body string
		cap  int
	}{
		{SecProj, projectSection(in), projCap},
		{SecDrift, driftSection(in), driftCap},
		{SecNotes, notesBody, notesCap},
		{SecRecent, recentSection(in), recentCap},
		{SecWhat, whatSection(in), whatCap},
	}

	var truncated []string
	if notesCut {
		truncated = append(truncated, SecNotes)
	}
	bodies := make(map[string]string, len(sections))
	for _, s := range sections {
		body, cut := capSection(s.body, s.cap)
		bodies[s.name] = body
		if cut && !contains(truncated, s.name) {
			truncated = append(truncated, s.name)
		}
	}

	build := func() string {
		var b strings.Builder
		b.WriteString(label)
		b.WriteString("\n\n## " + SecScope + "\n" + scope + "\n")
		for _, s := range sections {
			b.WriteString("\n## " + s.name + "\n" + bodies[s.name] + "\n")
		}
		// Repeated last, verbatim: recency. §4 binds both positions.
		b.WriteString("\n## " + SecScope + " (repeated)\n" + scope + "\n")
		return b.String()
	}

	text := build()
	// Backstop. Only reachable if the fixed scaffolding is itself enormous —
	// e.g. a project with hundreds of repos, whose never-truncated scope list
	// legitimately outgrows its 1 KB budget. Sections are then cut in
	// eatOrder, and every cut is recorded, because a brief that is quietly
	// short is worse than one that says it is short.
	for i := 0; len(text) > briefCap && i < len(eatOrder); i++ {
		name := eatOrder[i]
		if strings.TrimSpace(bodies[name]) == "" {
			continue
		}
		bodies[name] = truncMarker + " — " + name + " was cut to fit the brief's 64 KB budget"
		if !contains(truncated, name) {
			truncated = append(truncated, name)
		}
		text = build()
	}

	return Brief{Text: text, Bytes: len(text), SHA256: digest([]byte(text)), Truncated: truncated}
}

// scopeSection is §4.1: verbatim-fixed text plus three substituted lists.
//
// It is NEVER truncated, which is why it is not run through capSection. The
// 1 KB figure in §4's table is a budget the fixed text plus a normal repo set
// sits inside, not a clamp — clamping it is precisely the failure §4 declares
// out of bounds, and a scope that silently loses its last two bullets is worse
// than a brief that is 200 bytes over budget.
func scopeSection(in Input) string {
	var b strings.Builder
	fmt.Fprintf(&b, "You are the orchestrator for the project %q, rooted at %s.\n\n",
		in.ProjectName, in.Root)
	notes := in.NotesDir
	if notes == "" {
		notes = "(not yet materialized)"
	}
	fmt.Fprintf(&b, "- The only directory you may write to is %s, and within it only %s.\n",
		notes, strings.Join(NoteFiles, ", "))
	fmt.Fprintf(&b, "- These repos are readable: %s. Nothing outside them is in scope.\n",
		joinPaths(in))
	b.WriteString("- You may not commit, push, rebase, or run destructive commands in any repo.\n")
	b.WriteString("- You may not start, resume, or kill other sessions. Delegation does not exist yet — " +
		"if the work needs to be split, write the split into loom-open.md and stop.\n")
	b.WriteString("- If you believe you need something outside this scope, say so and stop. " +
		"Do not route around it.\n")
	return b.String()
}

func joinPaths(in Input) string {
	paths := make([]string, 0, len(in.Repos)+1)
	seen := map[string]bool{}
	if in.Root != "" {
		paths = append(paths, in.Root)
		seen[in.Root] = true
	}
	for _, r := range in.Repos {
		if r.Path == "" || seen[r.Path] {
			continue
		}
		seen[r.Path] = true
		paths = append(paths, r.Path)
	}
	if len(paths) == 0 {
		return "(none)"
	}
	return strings.Join(paths, ", ")
}

// projectSection is §4.2: name, root, and ONE line per repo. Nothing else — no
// file listings, no language detection, no README (§11's table says why: a
// preloaded guess is how a 200 k window becomes 40 k of usable room).
func projectSection(in Input) string {
	var b strings.Builder
	fmt.Fprintf(&b, "name: %s\nroot: %s\n\n", in.ProjectName, in.Root)
	if len(in.Repos) == 0 {
		b.WriteString("(no member repos registered)\n")
		return b.String()
	}
	for _, r := range in.Repos {
		switch {
		case r.Missing:
			fmt.Fprintf(&b, "- %s · %s · missing\n", r.Label, r.Path)
		case r.Err != "":
			fmt.Fprintf(&b, "- %s · %s · %s (%s)\n", r.Label, r.Path, unavailable, r.Err)
		default:
			fmt.Fprintf(&b, "- %s · %s · %s · %s · %d dirty\n",
				r.Label, r.Path, r.Branch, short(r.Head), r.Dirty)
		}
	}
	return b.String()
}

// driftSection is §8's report. Absent state.json produces no drift lines at all
// — "first-ever spawn is not a special mode", so the wording differs and the
// code path does not.
func driftSection(in Input) string {
	if len(in.Drift) == 0 {
		return "nothing moved since the last brief.\n"
	}
	var b strings.Builder
	for _, l := range in.Drift {
		b.WriteString("- " + l + "\n")
	}
	return b.String()
}

// notesSection is §4.4: the three files verbatim iff they total ≤ notesCap,
// otherwise each head-truncated to notesPerFileCap with the marker and the
// absolute path.
//
// Verbatim inclusion is a LATENCY optimization, not a necessity: the files are
// on disk inside an --add-dir and the agent can always read them itself. That
// is what makes head-truncation an acceptable answer to a 500 KB notes file
// rather than a loss.
func notesSection(in Input) (string, bool) {
	if !anyNotes(in.Notes) {
		return "none yet.\n", false
	}
	total := 0
	for _, n := range in.Notes {
		total += n.Bytes
	}
	verbatim := total <= notesCap

	var b strings.Builder
	for _, n := range in.Notes {
		switch {
		case n.ReadErr != "":
			fmt.Fprintf(&b, "### %s — %s (%s)\n\n", n.Name, unavailable, n.ReadErr)
			continue
		case !n.Exists:
			fmt.Fprintf(&b, "### %s — not written yet\n\n", n.Name)
			continue
		}
		head := "### " + n.Name
		if n.PreExisting {
			head += " (pre-existing — treat as authoritative, do not rewrite wholesale)"
		}
		b.WriteString(head + "\n\n")
		if verbatim {
			b.WriteString(n.Content)
		} else {
			b.WriteString(headTrunc(n.Content, notesPerFileCap))
			b.WriteString("\n" + truncMarker + " — read the rest at " + n.Path)
		}
		b.WriteString("\n\n")
	}
	return b.String(), !verbatim
}

// recentSection renders §4.5's one-line-per-session list under BOTH of §5.2's
// caps: 40 rows (applied upstream, in filterRecent) and 16 KB (applied here, by
// stopping at the row that would cross it rather than by slicing mid-line — a
// half-rendered session line reads as a real one).
func recentSection(in Input) string {
	if len(in.Recent) == 0 {
		return "no prior indexed sessions in this project.\n"
	}
	var b strings.Builder
	dropped := 0
	for i, r := range in.Recent {
		line := "- " + r.Render() + "\n"
		if b.Len()+len(line) > recentCap {
			dropped = len(in.Recent) - i
			break
		}
		b.WriteString(line)
	}
	if dropped > 0 {
		b.WriteString(truncMarker + " — " + strconv.Itoa(dropped) +
			" older sessions omitted to fit the section budget\n")
	}
	return b.String()
}

// whatSection is §4.6: the user's typed intent plus, always, the standing
// instruction. "Always" is load-bearing — the standing instruction is where
// "reconcile drift before starting new analysis" lives, and an orchestrator
// that skips reconciliation writes its successor a brief built on a stale
// picture.
func whatSection(in Input) string {
	var b strings.Builder
	if !anyNotes(in.Notes) {
		// Bootstrap wording, same code path (§8).
		b.WriteString("This project has no notes yet. Write loom-map.md first: " +
			"what the repos are and where the seams between them run.\n\n")
	}
	if intent := cleanIntent(in.Intent); intent != "" {
		b.WriteString("The human asked for:\n\n" + intent + "\n\n")
	}
	b.WriteString("Standing instruction (applies every session):\n")
	b.WriteString("- Keep " + strings.Join(NoteFiles, ", ") + " current.\n")
	b.WriteString("- Reconcile anything listed under Drift before starting new analysis.\n")
	b.WriteString("- Record every decision in loom-decisions.md as it is made, not at the end.\n")
	return b.String()
}

// cleanIntent applies §4.6's two rules to the user's typed intent: CleanText
// (so a multi-line paste cannot smuggle a newline into the seed, where a
// newline is an Enter and submits early) and a 600-character cap.
func cleanIntent(s string) string {
	s = strings.TrimSpace(memory.CleanText(s))
	return headTrunc(s, intentCap)
}

// capSection clamps one section to its budget, cutting at a rune boundary and
// saying so inline. Truncation is VISIBLE — §4: "a silently short brief is the
// failure mode this exists to prevent".
func capSection(body string, limit int) (string, bool) {
	if len(body) <= limit {
		return body, false
	}
	marker := "\n" + truncMarker + " — section cut at its budget\n"
	keep := limit - len(marker)
	if keep < 0 {
		keep = 0
	}
	return headTrunc(body, keep) + marker, true
}

// headTrunc keeps the first limit BYTES of s, backing off to the previous rune
// boundary so a multi-byte character is never split into a replacement glyph.
func headTrunc(s string, limit int) string {
	if len(s) <= limit {
		return s
	}
	for limit > 0 && !utf8Boundary(s[limit]) {
		limit--
	}
	return s[:limit]
}

// utf8Boundary reports whether b can begin a UTF-8 sequence (i.e. is not a
// 10xxxxxx continuation byte).
func utf8Boundary(b byte) bool { return b&0xC0 != 0x80 }

func contains(ss []string, s string) bool {
	for _, x := range ss {
		if x == s {
			return true
		}
	}
	return false
}

// Seed builds §6's delivery: the brief is a POINTER, not a payload.
//
// Workflows measured the tmux send-keys argv ceiling at ≈16.3 KB and set a
// 15 KB seed cap; a multi-repo brief does not fit and must not try. A single
// line naming the file stays an order of magnitude under that cap, makes the
// newline hazard structurally impossible (every substituted value goes through
// memory.CleanText, exactly as workflow substitution already does), and reuses
// session.seedWhenReady and the ready/trust gate completely unchanged —
// including selectCursorPattern, which the add-dir spike proved is the real
// defence against typing a seed into a dialog.
func Seed(briefPath, intent string) string {
	tail := "Then follow the brief's \"What to do\" section."
	if i := cleanIntent(intent); i != "" {
		tail = "Then: " + i
	}
	s := memory.CleanText("Read " + briefPath +
		" first — it is your assembled context for this project. " +
		"Follow its \"Authorization scope\" section exactly. " + tail)
	return headTrunc(s, seedCap)
}
