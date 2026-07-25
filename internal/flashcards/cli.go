package flashcards

import (
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

// RunCLI implements `loom flashcards generate <projectRoot> [partSubstr]`.
func RunCLI(args []string, st *store.Store, binary, workDir string, now int64, out io.Writer) error {
	if len(args) < 2 || args[0] != "generate" {
		return fmt.Errorf("usage: loom flashcards generate <projectRoot> [partSubstr]")
	}
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

func projectName(root string) string {
	root = strings.TrimRight(root, "/")
	if i := strings.LastIndexByte(root, '/'); i >= 0 {
		return root[i+1:]
	}
	return root
}
