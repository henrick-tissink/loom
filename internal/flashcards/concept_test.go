package flashcards

import (
	"strconv"
	"strings"
	"testing"
)

// manyConceptCards is a fake `claude -p` author that emits n concept cards
// (ignoring stdin), each with a distinct question so they don't collapse on the
// (anchor, stem_hash) identity key.
func manyConceptCards(n int) string {
	var b strings.Builder
	b.WriteString("#!/bin/sh\ncat >/dev/null\ncat <<'JSON'\n{\"cards\":[")
	for i := 0; i < n; i++ {
		if i > 0 {
			b.WriteString(",")
		}
		b.WriteString(`{"type":"concept","front":"why does it work like q` + strconv.Itoa(i) +
			`","back":"because a` + strconv.Itoa(i) + `","source_ref":"internal/s"}`)
	}
	b.WriteString("]}\nJSON")
	return b.String()
}

func TestSubsystemGenerateProducesConceptCards(t *testing.T) {
	g := &Generator{Binary: fakeBin(t, manyConceptCards(3)), WorkDir: t.TempDir()}
	p := Part{Kind: PartSubsystem, ID: "internal/s", Title: "s", SourceRef: "internal/s",
		Source: subsystemFilePrefix + "internal/s/x.go ====\npackage s\n"}
	cards, err := g.Generate("loom", p, 100)
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if len(cards) != 3 {
		t.Fatalf("cards = %d, want 3", len(cards))
	}
	for _, c := range cards {
		if c.Type != "concept" {
			t.Fatalf("want concept type, got %q", c.Type)
		}
		if c.Anchor != "loom|concept|internal/s" {
			t.Fatalf("anchor = %q, want subsystem-cited anchor", c.Anchor)
		}
	}
}

// A subsystem that authors more than the cap keeps only maxSubsystemCards
// survivors, and every stored card passed the fair-characterization gate.
func TestSubsystemGenerateForPartCapsAtDenseSet(t *testing.T) {
	st := openStore(t)
	pl := &Pipeline{
		Store: st,
		Gen:   &Generator{Binary: fakeBin(t, manyConceptCards(9)), WorkDir: t.TempDir()},
		Ver:   &Verifier{Binary: fakeBin(t, fakeClaudeVerdictYes), WorkDir: t.TempDir()},
	}
	p := Part{Kind: PartSubsystem, ID: "internal/s", SourceRef: "internal/s",
		Source: subsystemFilePrefix + "internal/s/x.go ====\npackage s\n"}
	stored, _, err := pl.GenerateForPart("loom", p, 100)
	if err != nil {
		t.Fatalf("GenerateForPart: %v", err)
	}
	if stored != maxSubsystemCards {
		t.Fatalf("stored = %d, want cap %d", stored, maxSubsystemCards)
	}
}

// A subsystem card the judge contradicts is rejected (the new gate applies to
// concept cards, which previously shipped unverified).
func TestSubsystemGenerateForPartRejectsContradicted(t *testing.T) {
	st := openStore(t)
	pl := &Pipeline{
		Store: st,
		Gen:   &Generator{Binary: fakeBin(t, manyConceptCards(3)), WorkDir: t.TempDir()},
		Ver:   &Verifier{Binary: fakeBin(t, fakeClaudeVerdictNo), WorkDir: t.TempDir()},
	}
	p := Part{Kind: PartSubsystem, ID: "internal/s", SourceRef: "internal/s",
		Source: subsystemFilePrefix + "internal/s/x.go ====\npackage s\n"}
	stored, rejected, err := pl.GenerateForPart("loom", p, 100)
	if err != nil {
		t.Fatalf("GenerateForPart: %v", err)
	}
	if stored != 0 || rejected != 3 {
		t.Fatalf("stored=%d rejected=%d, want 0 stored / 3 rejected", stored, rejected)
	}
}
