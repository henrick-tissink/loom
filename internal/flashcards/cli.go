package flashcards

import (
	"bufio"
	"fmt"
	"io"
	"strings"

	"github.com/henricktissink/loom/internal/store"
)

// Pipeline generates, verifies, and stores cards for parts.
type Pipeline struct {
	Store *store.Store
	Gen   *Generator
	Ver   *Verifier
}

// needsVerify reports whether a card type must pass the correctness gate.
// concept/decision are rationale with no code ground truth (spec §6).
func needsVerify(t string) bool { return t == string(TypeCode) || t == string(TypeCloze) }

// GenerateForPart authors cards for one part, gates behavioral cards through the
// verifier, and stores survivors as drafts. Verify errors fail closed (reject).
func (pl *Pipeline) GenerateForPart(project string, p Part, now int64) (stored, rejected int, err error) {
	cards, err := pl.Gen.Generate(project, p, now)
	if err != nil {
		return 0, 0, fmt.Errorf("generate %s: %w", p.ID, err)
	}
	for _, c := range cards {
		if needsVerify(c.Type) {
			ok, _, verr := pl.Ver.Verify(c, p.Source)
			if verr != nil || !ok {
				rejected++
				continue
			}
		}
		if _, inserted, ierr := pl.Store.InsertFlashcard(c); ierr != nil {
			return stored, rejected, fmt.Errorf("store card: %w", ierr)
		} else if inserted {
			stored++
		}
	}
	return stored, rejected, nil
}

// RunCLI dispatches `loom flashcards <generate|curate|review|stats|export> <projectRoot> [args]`.
func RunCLI(args []string, st *store.Store, binary, workDir string, now int64, in io.Reader, out io.Writer) error {
	if len(args) < 2 {
		return fmt.Errorf("usage: loom flashcards <generate|curate|review|stats|export> <projectRoot> [args]")
	}
	verb, root := args[0], args[1]
	project := projectName(root)
	rv := &Reviewer{Store: st, Cfg: DefaultReviewConfig()}
	switch verb {
	case "generate":
		return runGenerate(args, st, binary, workDir, now, out)
	case "curate":
		return runCurate(args, st, project, now, out)
	case "review":
		return runReview(rv, project, now, in, out)
	case "stats":
		return runStats(rv, project, now, out)
	case "export":
		return runExport(args, st, project, out)
	default:
		return fmt.Errorf("unknown flashcards command %q", verb)
	}
}

// runGenerate implements `loom flashcards generate <projectRoot> [partSubstr]`.
func runGenerate(args []string, st *store.Store, binary, workDir string, now int64, out io.Writer) error {
	root := args[1]
	var filter string
	if len(args) > 2 {
		filter = args[2]
	}
	parts, err := BuildManifest(root)
	if err != nil {
		return fmt.Errorf("build manifest: %w", err)
	}
	pl := &Pipeline{
		Store: st,
		Gen:   &Generator{Binary: binary, WorkDir: workDir},
		Ver:   &Verifier{Binary: binary, WorkDir: workDir},
	}
	project := projectName(root)
	var totStored, totRej, done int
	for _, p := range parts {
		if filter != "" && !strings.Contains(p.ID, filter) {
			continue
		}
		s, r, gerr := pl.GenerateForPart(project, p, now)
		if gerr != nil {
			fmt.Fprintf(out, "  %-50s gen_failed: %v\n", p.ID, gerr)
			continue
		}
		done++
		totStored += s
		totRej += r
		fmt.Fprintf(out, "  %-50s stored=%d rejected=%d\n", p.ID, s, r)
	}
	fmt.Fprintf(out, "flashcards: %d parts, stored=%d rejected=%d\n", done, totStored, totRej)
	return nil
}

func runCurate(args []string, st *store.Store, project string, now int64, out io.Writer) error {
	drafts, err := st.DraftsForProject(project)
	if err != nil {
		return err
	}
	activateAll := false
	for _, a := range args[2:] {
		if a == "--activate-all" {
			activateAll = true
		}
	}
	if !activateAll {
		fmt.Fprintf(out, "%d draft card(s) awaiting curation:\n", len(drafts))
		for _, c := range drafts {
			fmt.Fprintf(out, "  [%d] %-14s %s\n", c.ID, c.Type, c.Front)
		}
		fmt.Fprintln(out, "re-run with --activate-all to activate them")
		return nil
	}
	n := 0
	for _, c := range drafts {
		if err := st.SetCardStatus(c.ID, "active", now); err != nil {
			return err
		}
		n++
	}
	fmt.Fprintf(out, "activated %d card(s)\n", n)
	return nil
}

func runReview(rv *Reviewer, project string, now int64, in io.Reader, out io.Writer) error {
	dayStart := now - (now % 86400)
	// mark which queued cards were due (for the log's was_due flag)
	due, err := rv.Store.DueReviewCards(project, now, 1000)
	if err != nil {
		return err
	}
	dueIDs := map[int64]bool{}
	for _, c := range due {
		dueIDs[c.ID] = true
	}
	queue, err := rv.BuildQueue(project, now, dayStart)
	if err != nil {
		return err
	}
	sc := bufio.NewScanner(in)
	reviewed := 0
	for _, c := range queue {
		fmt.Fprintf(out, "Q: %s\n", c.Front)
		fmt.Fprintf(out, "A: %s\n", c.Back)
		fmt.Fprint(out, "grade (1=again 2=hard 3=good 4=easy, q=quit): ")
		if !sc.Scan() {
			break
		}
		line := strings.TrimSpace(sc.Text())
		if line == "q" || line == "" {
			break
		}
		g, ok := parseGrade(line)
		if !ok {
			fmt.Fprintf(out, "  ignored %q\n", line)
			continue
		}
		if _, err := rv.Record(c.ID, g, dueIDs[c.ID], now); err != nil {
			return err
		}
		reviewed++
	}
	fmt.Fprintf(out, "reviewed %d card(s)\n", reviewed)
	return nil
}

func runStats(rv *Reviewer, project string, now int64, out io.Writer) error {
	cov, err := rv.Coverage(project, now)
	if err != nil {
		return err
	}
	for _, c := range cov {
		fmt.Fprintf(out, "  %-40s total=%d active=%d draft=%d due=%d\n", c.Part, c.Total, c.Active, c.Draft, c.Due)
	}
	rate, n, err := rv.PassRate(project, 0)
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "pass-rate: %.0f%% over %d review(s)\n", rate*100, n)
	return nil
}

// runExport writes the project's active deck to out in the chosen format:
// export <projectRoot> [csv|md]. csv is Anki-importable; md is a greppable
// study document. The caller redirects to a file.
func runExport(args []string, st *store.Store, project string, out io.Writer) error {
	format := "csv"
	if len(args) > 2 {
		format = args[2]
	}
	// Validate the format before touching the DB, so a typo doesn't pay for a
	// full deck query it will only discard.
	if format != "csv" && format != "md" && format != "markdown" {
		return fmt.Errorf("unknown export format %q (want csv or md)", format)
	}
	cards, err := st.ExportCards(project)
	if err != nil {
		return err
	}
	if format == "csv" {
		fmt.Fprint(out, ToCSV(cards))
	} else {
		fmt.Fprint(out, ToMarkdown(cards))
	}
	return nil
}

func parseGrade(s string) (Grade, bool) {
	switch s {
	case "1":
		return GradeAgain, true
	case "2":
		return GradeHard, true
	case "3":
		return GradeGood, true
	case "4":
		return GradeEasy, true
	}
	return 0, false
}

func projectName(root string) string {
	root = strings.TrimRight(root, "/")
	if i := strings.LastIndexByte(root, '/'); i >= 0 {
		return root[i+1:]
	}
	return root
}
