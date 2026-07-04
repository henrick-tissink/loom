# Fan-out + Wall Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Phase 4 per spec `docs/superpowers/specs/2026-07-04-fanout-wall-design.md` (Revision 2 — §§2-3 binding, §5 test list binding).

**Architecture:** Two new self-contained view modes (`internal/ui/fanout.go`, `internal/ui/wall.go`) on existing substrate; zero non-ui package changes.

**Tech Stack:** existing only.

## Global Constraints
- Spec Rev 2 §§2-6 binding; all UI invariants (exact width, truncate-before-style, ONE tickAfter per tickMsg, captured targets/in-flight guards, stale-discard, nil-Deps safety).
- gofmt -w; conventional commits; `go vet ./... && go test -race -count=1 ./...` green each task end.

---

### Task 1: Fan-out
**Files:** Create `internal/ui/fanout.go`; Modify `internal/ui/app.go`, `app_test.go`.
Implements spec §2 verbatim: fanoutForm (own state; shares only modelOptions/modeOptions/optLabel/cycle + a seed textinput), N key, focus rules, sequential launch+SetTags cmd with untagged accounting, fanInFlight, stay-until-result then viewDash + persistent fanHint (wfHint discipline; SURVIVES snapMsg — binding test), fanResult fires pollCmd, `· fan` marker in the activity cell (seed-failed precedent), keybar N entry in the elision tier.
- [ ] Failing tests per spec §5 fan-out row → implement → PASS + full suite → commit `feat: fan-out launcher with group tagging`.

### Task 2: Wall
**Files:** Create `internal/ui/wall.go`; Modify `internal/ui/app.go`, `app_test.go`.
Implements spec §3 verbatim: W key, stable CreatedAt-then-name order, 2-col exact-width grid (extra cell RIGHT), tailH clamp, pagination + right-annotation indicator, per-tick one-shot visible-page capture cmd (wallMsg, stale-discard), capture-error cells `(pane unavailable)` with gated ↵, name-keyed selection with nearest-neighbor fallback, esc/q/ctrl+c, keybar W entry.
- [ ] Failing tests per spec §5 wall row (incl. stable-order-on-status-flip and ONE-tickAfter sweep) → implement → PASS + full suite → commit `feat: read-only session wall`.

### Task 3: e2e + README + polish
- [x] E2E per spec §5 (scratch env isolation per Phase-3/R-3 precedent; fake claude; only own sessions killed; real DB/tmux verified untouched); captures 100/46; designer eyeball; nits fixed+disclosed.
- [x] README: Fan-out + Wall section.
- [x] `gofmt -l . && go vet ./... && go test -race -count=1 ./...` → commit `feat: fan-out+wall e2e + README`.

## Self-Review
Spec §2→T1, §3→T2, §5 distributed, §6 respected. No placeholders (spec §§2-3 carry the exact semantics; tasks reference them as binding). Types: fanResultMsg/wallMsg local to ui; no cross-package changes.
