# Loom — Projects Foundation — Implementation Plan

**Spec:** `docs/superpowers/specs/2026-07-22-projects-foundation-design.md` (Revision 2 — binding)
**Date:** 2026-07-22

## Execution model

Waves are **serial**; agents within a wave are **parallel and package-disjoint** — no two concurrent agents may write the same file. Between every wave: `go build ./... && go test ./...` must be green before the next wave starts. A wave that fails its gate is repaired before proceeding.

Isolation is structural (disjoint file ownership per wave), not instruction-level within shared files — see spec §11 for why.

## Waves

**W0 — rename sweep** (serial)
`registry.Project` → `registry.Repo` across the tree. Mechanical. Label semantics unchanged (spec §2 binds `project/repo`). On-disk workflow JSON untouched.
Owns: `internal/registry`, plus every reference site in `internal/ui`, `internal/workflow`, `cmd/loom`, `cmd/loom-gui`.

**W1 — foundations** (3 parallel)
1. `registry-discovery` — spec §3 ordered decision list; symlink skip; depth-1. Owns `internal/registry/*` only.
2. `store-schema` — migrations v7 + v8, `projects`/`project_repos` CRUD, `SessionRow.AddDirs`, Ungrouped seed row. Owns `internal/store/*` only.
3. `status-shape` — `Snapshot.NewlyNeedsYou` → session names (spec §4), plus the minimum consumer edits to keep the build green. Owns `internal/status/*`, `internal/ui/app.go`, `cmd/loom-gui/notify.go`, `cmd/loom-gui/app.go`.

**W2 — resolver and launch** (2 parallel)
4. `projects-pkg` — new `internal/projects`: attribution resolver (§4) + visibility predicate (§6.1) + discovery/DB reconciliation service (§7). Owns new files only.
5. `session-adddirs` — `Recipe.AddDirs`, `Argv`, resume threading, cwd validation, plus spec §12 fixes (`waitReady` timeout, `tmux -c` fallback). Owns `internal/session/*`.

**W3 — frontends, Go side** (2 parallel)
6. `tui-hiding` — §6.3 TUI surfaces + project scoping. Owns `internal/ui/*`.
7. `gui-go` — DTOs, project bindings, service wiring, sectioned `SessionDiff`, over-fetch-before-filter. Owns `cmd/loom-gui/*.go`.

**W4 — frontend** (serial)
8. `gui-frontend` — sectioned rail, project overview, create-project, hide/solo chip with armed confirm. Owns `cmd/loom-gui/frontend/*`.

**W5 — integration** (serial)
9. `verify` — full build + test; audit implementation against spec §1–§10 and §12; report gaps.
10. `fix` — close reported gaps.

## Out of scope

`--add-dir` spike (spec §5) is a prerequisite for the *behavioural* half of multi-repo launch, not for the plumbing. `AddDirs` is built and persisted here; the trust-dialog hardening lands with the spike.
