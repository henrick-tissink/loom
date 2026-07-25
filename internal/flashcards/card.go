// Package flashcards generates and verifies spaced-repetition study cards over
// a managed project's source and docs (spec docs/superpowers/specs/2026-07-25-
// flashcards-design.md). This slice is headless: manifest, generation, verify.
package flashcards

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"unicode"
)

type CardType string

const (
	TypeConcept  CardType = "concept"
	TypeDecision CardType = "decision"
	TypeCode     CardType = "code"
	TypeCloze    CardType = "cloze"
)

// ValidType reports whether t is a schedulable card type this slice authors.
// (trace is an unscored walkthrough in a later slice and is not authored here.)
func ValidType(t CardType) bool {
	switch t {
	case TypeConcept, TypeDecision, TypeCode, TypeCloze:
		return true
	}
	return false
}

// Anchor is a card's stable source-location key (spec §7): project|type|sourceRef.
// Never includes card text, so a reworded card re-links to the same row.
func Anchor(project string, t CardType, sourceRef string) string {
	return project + "|" + string(t) + "|" + sourceRef
}

// StemHash normalizes a question to its recall stem (lowercase, punctuation
// dropped, whitespace collapsed) and hashes it. With Anchor it forms the
// (anchor, stem_hash) dedup/identity key (spec §8): true restatements collapse;
// distinct questions about the same source stay distinct.
func StemHash(front string) string {
	var b strings.Builder
	prevSpace := false
	for _, r := range strings.ToLower(front) {
		switch {
		case unicode.IsLetter(r) || unicode.IsDigit(r):
			b.WriteRune(r)
			prevSpace = false
		case unicode.IsSpace(r):
			if !prevSpace {
				b.WriteByte(' ')
				prevSpace = true
			}
		}
	}
	return Hash(strings.TrimSpace(b.String()))
}

// Hash is the shared 64-bit-ish content fingerprint (first 16 hex of sha256)
// used for stem, source, and answer hashes.
func Hash(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:8])
}
