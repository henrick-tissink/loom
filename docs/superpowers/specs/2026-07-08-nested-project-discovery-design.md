# Nested Project Discovery

**Date:** 2026-07-08
**Status:** Approved

## Problem

`registry.Discover` only scans the immediate children of the workspace root
and includes a directory when it has a `.git` or an existing Claude
transcript directory. Group folders that hold several repos one level down
(e.g. `~/Sauce/urban-elephant/urban-elephant-web`) are invisible: the
wrapper has neither marker, and discovery never looks deeper.

## Decision

One-level descent into non-project subdirectories (approaches rejected:
full recursive walk — YAGNI, risks crawling large non-repo trees; explicit
group-dir config — needless config surface).

## Behavior

- The "is a project" predicate is unchanged: has `.git` (dir or file) or an
  existing Claude transcript dir for its absolute path.
- A workspace subdir that passes the predicate is included as today
  (label = basename) and is **not** descended into — repos nested inside a
  real project (worktrees, vendored checkouts) stay hidden.
- A workspace subdir that fails the predicate is treated as a group dir:
  each of its non-hidden child directories is tested with the same
  predicate; those that pass are included with label `parent/child` and
  `Path` = the child's absolute path. The group dir itself is never listed.
- Depth is strictly one extra level. Hidden dirs are skipped at both
  levels. A group dir that can't be read (permissions) is skipped, not
  fatal. Sorting stays alphabetical by label.

## Impact

`Project.Label` is display-only and a workflow-definition map key; it is
never used to build filesystem paths, so a `/` in the label is safe.
Everything downstream (cwd, transcript correlation, memory, store) keys off
`Path`. Workflow step display that recovers a short label via
`filepath.Base(Path)` shows the child basename — acceptable.

## Testing

Extend `TestDiscover`: group dir with a git child (included, slash label);
group dir with only plain children (excluded); project dir containing a
nested repo (nested repo not listed); unreadable group dir skipped
implicitly via the error-tolerant path. Update MANUAL.md's one-line
discovery rule.
