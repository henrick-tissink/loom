# High-Level Subsystem Flashcards — Design

**Date:** 2026-07-26
**Status:** Approved (brainstormed with the user across a calibration dialogue)
**Supersedes generation altitude of:** `docs/superpowers/specs/2026-07-25-flashcards-design.md` §4 (author prompt) and §3 (manifest unit for code).

## Problem

The flashcards feature generates cards per **source file** with a prompt that says
*"test RECALL of specific facts."* For loom itself this produces cards that are "way
too granular" — exact return values, constants, method signatures. The user wants to
**understand loom as a system**: its subsystems, the key design decisions, why they're
built that way, and — where the mechanism is the point — how they actually work.

## The altitude rule (the core of this change)

Generation follows one rule, applied per topic:

> **Default to high-level.** Ask about a subsystem's *job*, its *key design decision*,
> *why* it's built that way, and its *trade-off*. **Drop one level into the mechanism**
> *only* where understanding requires the moving parts (signing/attestation, the
> status-fusion algorithm, the SM-2 scheduler). **Never** descend to literal values —
> exact constants, return values, signatures, config-key strings. Flags are fair game;
> flag *strings* are not.

Calibrated against three sample decks in the design dialogue (loom `status`; a
hypothetical Atlas ingestion/schema/security platform; a signing-mechanism deck) — the
target is the altitude of those samples.

## Design

### 1. Unit: subsystem, not file

A **subsystem** is a directory containing source files (language-agnostic; maps to a Go
package). The generator feeds the model the *whole subsystem at once* — every non-test
code file in the directory, with comments intact — and asks the conceptual question.

- New `PartKind` `PartSubsystem`. `Part.ID`/`SourceRef` = project-relative dir
  (e.g. `internal/status`, `cmd/loom-gui`). `Part.Title` = last path segment.
  `Part.Source` = the dir's non-test code files, sorted by name, joined by a separator
  line `// ==== loom:file <rel> ====`, bounded to `maxSubsystemSource` (48_000 bytes;
  whole files added in sorted order until the budget, truncation noted in-source).
- **Subsystem parts REPLACE file-level code parts** in the manifest. Doc-section parts
  (`PartDoc`) stay unchanged — they already carry "the why" at a good altitude. The
  manifest is therefore: subsystem parts (code, high-level) + doc parts.
- Consequence: existing granular file-part cards in the DB become **orphans** (their
  part no longer exists) — which is correct; they're the cards being replaced. A fresh
  generate produces the new deck.

### 2. Author pass: conceptual synthesis

- New `conceptPrompt` encoding the altitude rule. Asks for a **small, dense** set (aim
  4–6) of cards on the subsystem's job / key decision / why / trade-off / mechanism-where-
  it-matters. Forbids: "what does X return", exact constants, signatures, config strings,
  "describe/define" prompts. Emits `concept`/`decision` typed cards as strict JSON (same
  envelope as `authorPrompt`).
- `Generator.Generate` routes the prompt by `p.Kind`: `PartSubsystem` → `conceptPrompt`;
  `PartDoc` → existing `authorPrompt` (unchanged). Hard-cap survivors at
  `maxSubsystemCards` (6), deterministic order.
- **Model:** the concept author pass runs on `sonnet` (synthesis over a whole package is
  materially better than haiku); the verify pass stays on `haiku` (judging is easier than
  authoring). `runClaude` gains a `model` parameter; all existing callers keep `haiku`.
  The hardened argv (no tools/MCP/slash/session/settings, scrubbed env) is unchanged —
  only the `--model` value differs.

### 3. Verify pass: fair characterization (new gate for concept cards)

Today `needsVerify()` sends only `code`/`cloze` through the strict literal-quote gate;
`concept`/`decision` ship unverified. High-level cards are interpretive, so the literal
gate is wrong for them — but shipping them unverified is worse. So:

- New `conceptVerifyPrompt` / `Verifier.VerifyCharacterization`: an independent pass
  judges whether the answer is a **fair, accurate characterization of the subsystem,
  consistent with — not contradicted by — the source**. Rejects: contradicted by source,
  invents a mechanism absent from the source, or asserts a specific value that's wrong.
- `Pipeline.GenerateForPart` routes by `p.Kind`: `PartSubsystem` → every card through
  `VerifyCharacterization`; other kinds → existing `needsVerify` + strict `Verify`.
- Fail-closed unchanged: a verify error or parse failure rejects the card.

### 4. Structural-hash staleness over a multi-file subsystem

`StructuralHash` must ignore comment/whitespace churn for subsystem parts too (comments
live in the authoring Source but must not churn the hash). Refactor:

- Extract `normalizeGo(src)` (the current parse-mode-0 + printer reprint).
- `StructuralHash(ref, source)`: `#` in ref → doc → `Hash(source)` (unchanged);
  `.go` suffix → `Hash(normalizeGo(source))` (unchanged); else → **subsystem**: split
  `source` on the `loom:file` separator, `normalizeGo` each Go chunk (raw fallback),
  hash the rejoined normalization.
- No `Part`-struct change; `ChangedParts`/`CheckStale` call sites are untouched (they
  already call `StructuralHash(p.SourceRef, p.Source)`).

### 5. Scope + surfaces

- New `ScopeSubsystems` (`p.Kind == PartSubsystem`). Since file parts are gone,
  `ScopeAll` now naturally = subsystems + docs (the high-level deck); `ScopeUncovered`
  likewise. GUI generate dialog gains a "Subsystems (high-level)" scope and defaults to
  it; `FlashcardGenerateScopes` DTO gains the count. Per-part regenerate and
  regenerate-changed already key on `Part.ID` (now subsystem dirs) and need no change.

## Deferred (explicitly out of scope)

- **Reference decks:** verbatim per-file/per-config cards enumerating literal values
  (flag strings, constants), with the strict literal-quote gate kept on. A separate card
  altitude, generated on demand from re-enumerated file parts. The user asked for this as
  a *later, separate* thing.

## Testing

- Manifest: subsystem parts emitted per dir; files sorted+joined+bounded; no file parts;
  doc parts unchanged.
- StructuralHash: a comment-only edit in one file of a subsystem does **not** change the
  hash; a structural edit does.
- Generate: subsystem part → `conceptPrompt` + sonnet + cap; doc part → `authorPrompt`.
- Verify routing: subsystem cards go through `VerifyCharacterization`; a card contradicted
  by source is rejected.
- Scope: `ScopeSubsystems` selects only subsystem parts.
- (LLM child calls are stubbed in tests as today — no live `claude` in unit tests.)

## Migration

After merge, loom's own deck is regenerated (delete the old granular cards, generate the
subsystem deck) so the user studies the new cards. Offered as a one-shot after the build.
