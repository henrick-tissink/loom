# Bundling the Agent Discipline Handbook with Loom — implementation plan

Status: plan, 2026-10-15. Nothing in this document is built yet.
Origin: a three-lens design consultation (packaging, adversarial review, product)
synthesised on 2026-10-15. The decisions in §1 are settled; everything below
them is the build order.

What the user can do when this lands that they cannot now: install loom on a
fresh machine, open it, and have the handbook's rules, hooks, state layout and
helpers installed and version-checked after one reviewed click; then see in
loom what the hooks are doing, per session and over time.

---

## 1. Decisions (settled)

| # | Decision | Why | Cost |
|---|---|---|---|
| D1 | Hooks stay plain Python files under the Claude config dir; loom never runs them from inside the app bundle. | `bash_guard` is registered fail-closed. A hook path into an app that moved would deny every Bash call in every session. Matches ARCHITECTURE §1 "a window, never an owner". | A python3 dependency, checked by doctor before anything is written. |
| D2 | The repo is the source of truth. `internal/discipline/bundle/` is seeded from the **live installed copy** (which is ahead of the published page), and becomes rev 3. The page is regenerated from the bundle; the page's `bootstrap.sh` is frozen at rev 2 and points at `loom setup`. | Two installers drift. The owner's machine already diverged from the page once (session lineage, 2026-10-14). | Hooks are edited in the repo from now on, then installed through the gate. |
| D3 | Loom embeds the files, not just a manifest, and has one installer in Go. The Python suites are embedded too, so the gate runs them under the user's own python3. | One installer, tested against fixture settings files. | Shipping a one-line hook fix needs a loom build. Accepted. |
| D4 | Install is atomic per revision: files go into an immutable `<ccd>/discipline/r<N>/`, and `<ccd>/hooks/enforce` becomes a symlink that is flipped in one rename. | Hooks are read from disk on every call. A file-by-file copy leaves a live session with a new `common.py` beside an old guard, the import fails, and the fail-closed guard denies every Bash call. | The logs must move out of the hook directory (rev 3 hook change, §3.3). |
| D5 | Only a human installs. Every loom command that writes to the Claude config refuses when `CLAUDECODE` is set, and needs a real terminal or the GUI. Loom never upgrades on its own. | Otherwise the agent being disciplined removes its own guards with one Bash call, which also bypasses the handbook's deny rule. | One click per upgrade, which is also where the human reviews replayed denials. |
| D6 | Loom owns only what its receipt says it wrote. Upgrade replaces a file only if it still matches a shipped rev. Uninstall undoes exactly the receipt, never restores a whole backup. | Local edits are the owner's; a whole-backup restore wipes everything written since. | Modified files need a Keep / Reset / Adopt choice. |
| D7 | `CLAUDE.md` is not block-managed. The rules ship as a file in the rev dir and `CLAUDE.md` carries one import line. Other text (the Lava section, the owner's own rules) stays outside. | The owner's whole file *is* the handbook text today; lava and mario already rewrite it by heading; a lost end marker would delete or orphan text. The hook messages quote the rules' wording, so rules version with hooks. | Migration on the owner's machine replaces the inline text with the import line, shown as a diff. |
| D8 | No wizard. One read-only health surface that always runs, and one gate sheet where writes happen after an explicit click. Manual steps (context diet, deny rule) are rows with measurements and copyable commands, never auto-applied. | A wizard stalls on the manual steps or pretends they were done. | — |
| D9 | The settings merge is surgical: identity = event + matcher + normalised script path; unknown keys preserved; key order preserved; re-read before rename; write through symlinks. | Live registrations use absolute home paths, the page uses `~`; Claude Code would run both. mario/lava entries carry `async`; Claude Code, lava, mario and plugins all write this file. | A small ordered-JSON editing type (§3.6). |
| D10 | Reports are shown "since the hook's last change" by default; a fixed window is split at change marks and never summed across them. | A 14-day total straddling the 13 Oct stop-guard fix misreported a solved problem as the top finding. | Loom must know when hooks changed: the receipt ledger plus a `rev` field in every log record. |

Not in scope of this plan: rewriting hooks in Go; changing any guard's
behaviour; automating the context diet (plugins, MCP, connectors); the
memory layer (nothing to install); publishing the handbook page (becomes a
`make` target later).

---

## 2. Target layout on a machine

`<ccd>` is the Claude config dir (`$CLAUDE_CONFIG_DIR`, else `~/.claude`).

```
<ccd>/discipline/
  r3/                          immutable, mode 0555 dirs / 0755 scripts
    REV                        "r3"
    enforce/*.py               7 hooks + common.py + 3 report/replay tools
    tests/*.py, tests/run_all.sh
    CLAUDE.how-to-work.md      the rules (How to work + Compact instructions)
    scripts/context-bar.sh
    bin/deadline
    manifest.json              copy of the embedded manifest for this rev
  current -> r3                symlink, flipped atomically
  local-20261015-1402/         the pre-existing hooks/enforce dir, moved here on adoption (backup)
<ccd>/hooks/enforce -> ../discipline/current/enforce      symlink (registrations keep their paths)
<ccd>/scripts/context-bar.sh -> ../discipline/current/scripts/context-bar.sh
<ccd>/state/{tasks,checkpoints,ctxbudget,testlog,by-pid}  created 0700, never deleted
<ccd>/state/log/decisions.jsonl, allows.jsonl             moved here from hooks/enforce (rev 3)
<ccd>/CLAUDE.md                contains the line  @discipline/current/CLAUDE.how-to-work.md
~/.local/bin/deadline          real file, 0755 (not a symlink: must survive loom's removal)
~/.loom/discipline/receipt.json
~/.loom/discipline/ledger.jsonl                            one line per install/upgrade/adopt/reset
~/.loom/backups/<ts>-<op>/                                 settings.json, CLAUDE.md before each write; last 20 kept
```

Registrations in `<ccd>/settings.json` keep today's shape and paths
(`<ccd>/hooks/enforce/<hook>.py`, bare script path, shebang python3), so an
upgrade normally writes **no** settings change at all. `statusLine` stays
`bash <ccd>/scripts/context-bar.sh`.

Why `deadline` is a real file: a session's `deadline 20m …` must keep working
if the user deletes loom; a dangling symlink there would make every
background command exit 127 while the guard still demands the wrapper.

---

## 3. Packages and formats

### 3.1 `internal/discipline/bundle/` (the source of truth)

Seeded by copying the live install, with these corrections:

- `tests/test_rerun_log.py` and `tests/test_rerun_report.py` are byte-identical
  copies of the hooks, not suites (`run_all.sh` runs 7 suites). Drop them.
- `tests/run_all.sh` gains `HOOKS_DIR` (env or `$1`), default unchanged, so the
  gate can test a staged copy.
- `__pycache__/` is never committed (`.gitignore`).
- `registrations.json` is new: the 7 hook registrations as data (event,
  matcher, script, timeout, onFailure) plus the statusLine command. Today they
  exist only inside the page's `bootstrap.sh`.
- `examples/task.example.md`, `examples/checkpoint.example.md` from the page.

Embedded with `//go:embed bundle` (not `all:`, which also skips `__pycache__`).
Embedded files are mode 0444; the installer sets modes from the manifest.

### 3.2 `manifest.json` (generated, committed)

```json
{
  "rev": "r3",
  "claudeCodeTested": ["2.1.296", "2.1.300"],
  "files": [
    {"path": "enforce/bash_guard.py", "sha256": "…", "mode": "0755", "target": "rev"},
    {"path": "bin/deadline",          "sha256": "…", "mode": "0755", "target": "~/.local/bin/deadline"},
    {"path": "scripts/context-bar.sh","sha256": "…", "mode": "0755", "target": "rev"},
    {"path": "CLAUDE.how-to-work.md", "sha256": "…", "mode": "0644", "target": "rev"},
    {"path": "tests/run_all.sh",      "sha256": "…", "mode": "0755", "target": "rev"}
  ],
  "registrations": "registrations.json",
  "history": {"r2": {"enforce/bash_guard.py": "…", "…": "…"}}
}
```

`target: "rev"` means "inside `<ccd>/discipline/r<N>/`". `history` holds every
earlier rev's hashes so doctor can label a file *stale* (matches an older rev,
safe to replace) rather than *modified*. Generated by `go generate
./internal/discipline` (a small Go program under `internal/discipline/gen/`);
`TestManifestCurrent` fails if any bundle file changes without a regenerated
manifest. A revision is frozen once it has a `history` entry: `gen` refuses to
regenerate changed files under a frozen REV, and bumping REV records the
outgoing revision's hashes into `history` automatically. (`TestManifestRevNotFrozen`
pins that the current rev is never also in history.) The page does not carry
file contents, so r2 hashes could not be recorded; the seed snapshot of the
owner's live install is the first history entry.

### 3.3 Rev 3 hook changes (plumbing only, no rule changes)

1. `common.py`: `LOG_PATH`/`ALLOW_PATH` default to `$STATE/log/…` (STATE already
   honours `CLAUDE_STATE_DIR`); the config dir default honours
   `CLAUDE_CONFIG_DIR`. `hook_report.py` and `replay_decisions.py` read the
   same default. Env overrides unchanged.
2. Every record from `log()`/`log_allow()` gains `"rev"`, read once from
   `<enforce dir>/../REV` (absent → `"local"`).
3. `run_all.sh` takes `HOOKS_DIR`.

Everything else is byte-for-byte the live copy. The suites must still pass
unchanged except for the log-path fixtures.

### 3.4 `internal/discipline` (Go)

```
embed.go        //go:embed bundle; Bundle() fs.FS; Manifest() Manifest
manifest.go     types; Load; FileState(target) → current|stale|modified|missing|extra
layout.go       paths for a config dir: RevDir, CurrentLink, HooksLink, StateDir, LogDir
doctor.go       Doctor(ctx, Env) Report      read-only
plan.go         PlanSetup(l, Report, PlanOptions, now) / PlanUninstall(l, Report, receipt) → Plan: Steps {kind, target, detail, group, enabled, diff}, Conflicts (keep|replace), Warnings, Errors; RenderPlan
gate.go         RunGate(ctx, stagedDir, installedHooks, logPath) GateResult: preflight (python3 runs, every hook parses, deadline works) → suites (staged tests, HOOKS_DIR=staged) → replay (30 days of denials through the staged guard vs the installed one: NewAllows / NewDenies); no writes outside the staged dir
apply.go        Stage(plan) writes discipline/<rev>/ (temp dir + rename) and runs the gate: invisible to sessions. Commit(plan, staged): backups → state dirs → legacy dir moved aside → symlinks (temp link + rename) → deadline → settings re-read and re-merged → CLAUDE.md → verify hashes, links and registrations → receipt + ledger; every step undone in reverse on failure. ApplyUninstall reverses the receipt. Lock file at ~/.loom/discipline/lock.
settings.go     MergeHooks/UnmergeHooks (identity event|matcher|basename), MergeStatusLine (only when unset), MergeEnv (absent keys only), MergeDeny; each Unmerge removes only what still equals the receipt
claudemd.go     PlanClaudeMD: inline rules text → import line, else append; UndoClaudeMD puts the text back when the install replaced it
receipt.go      ~/.loom/discipline/receipt.json, ledger.jsonl, Backup() into ~/.loom/backups/<ts>-<op>/ (origins.json maps each copy to its path; newest 20 kept), RestoreBackup
local.go        ResolveFile, FileDiff, Keep (records the edit in receipt.kept), Reset (bundle copy back, backed up)
guard.go        RefuseIfAgent(stdin): ErrAgent when CLAUDECODE is set, ErrNoTTY when stdin is not a terminal (TIOCGWINSZ, not a mode check: /dev/null is a character device)
textdiff.go     UnifiedDiff for the plan's previews
```

Receipt rule for files outside the revision dir (`~/.local/bin/deadline`): they are
listed only when a setup wrote them, so uninstall never removes a wrapper that
was there before loom. A file already current at setup time is adopted, not owned.

`Env` carries: config dir, home, `$PATH` of the login shell (via `$SHELL -lc
'echo $PATH'`), the `env.PATH` from settings, the `tmux -L loom` environment,
and the resolved `claude --version`. Doctor never shells out to anything that
writes.

### 3.5 Receipt and ledger

```json
{
  "configDir": "/Users/x/.claude",
  "rev": "r3",
  "installedAt": "2026-10-16T09:12:00+02:00",
  "loomBuild": "e982269+dirty",
  "files":   {"/Users/x/.claude/discipline/r3/enforce/bash_guard.py": "sha…", "/Users/x/.local/bin/deadline": "sha…"},
  "links":   {"/Users/x/.claude/hooks/enforce": "../discipline/current/enforce", "/Users/x/.claude/discipline/current": "r3"},
  "settings": {"hooks": ["PreToolUse|Bash|hooks/enforce/bash_guard.py", "…"],
               "statusLine": "bash ~/.claude/scripts/context-bar.sh",
               "env": {"CLAUDE_CODE_MAX_SUBAGENT_SPAWN_DEPTH": "1"},
               "permissionsDeny": ["Edit(~/.claude/hooks/**)"]},
  "claudeMd": {"importLine": "@discipline/current/CLAUDE.how-to-work.md"},
  "kept":    {"/Users/x/.claude/discipline/r3/enforce/session_checkpoint.py": "sha… (user chose Keep)"}
}
```

A settings value still equal to the receipt may be updated or removed by
loom; a value that differs is user-tuned, kept and reported. With no receipt
(adopting an existing install) any value equal to what a shipped rev would
have written counts as loom's. Ledger lines: `{ts, op, fromRev, toRev,
files:{…}, gate:{suites, replayFlips}}`; they are the change marks for §5.

### 3.6 `internal/jsonedit`

An order-preserving JSON object (`[]Member{Key, Value json.RawMessage}` with
nested parse on demand). Operations: get path, set path, delete path, append
to array, and `WriteFile(path)` that resolves symlinks (`filepath.EvalSymlinks`)
and writes temp + fsync + rename into the real file's directory. `Apply`
re-reads the file immediately before the rename and re-plans if its hash
changed since Plan. Tests use fixtures: the owner's real `settings.json`
shape (mario/lava `async` entries, plugin Stop hook, `permissions.autoMode`),
a tilde-path page install, a duplicated registration, a dotfiles symlink.

Hook identity for merge: `event + "|" + matcher + "|" + normalise(command)`
where normalise expands `~` and `$HOME`, resolves the config dir, and
`filepath.Clean`s the script path; a `python3 -I <path>` form and a bare
`<path>` form are the same identity. Duplicates found at merge time are
reported, never silently collapsed.

### 3.7 `internal/hooklog`

Incremental reader for `state/log/decisions.jsonl` and `allows.jsonl`:
remembers `(inode, offset)` per file, tolerates truncation (restarts at 0),
reads only complete lines, drops a file's records when it vanishes. Legacy
dirs (`AddLegacyDir`: the current `hooks/enforce` dir and the receipt's
`migratedFrom` dir) are read once, read-only, for history. Exposes:

- `Records()` / `BySession(sid)` — decisions and allows merged by time;
  SubagentStop records carry the parent's session id and are attributed to it.
- `Rates(recs, since, marks, now) []HookRow` — per hook: rev and one `Span`
  per change mark with runs, silent, holds, nudges, tests, the decision
  histogram and a daily series. A span never crosses a mark and the rates
  are never summed across marks.
- `InferredMarks(recs, legacyDirs, solid, within)` — the dashed marks: the
  first record of each `rev` value, the `.py` mtimes of a legacy hooks dir
  and the stamps in backup names, clustered within an hour and dropped when
  a ledger mark already stands within the hour.
- `Reruns(recs, since) []ProjectRow` — the test re-run report per project.
- `Session(recs, since) SessionStats` — holds, nudges, re-runs, the held
  group (same hook + decision ≥ `HeldThreshold` = 3 times) and the last stop
  verdict; `Classes(recs, since)` gives the "N of M" lines.

Privacy: the record's `command` (≤800 chars) and `cwd` are stored; the GUI
applies the project hide rules by `cwd`, and `allows.jsonl` records (which
have no `cwd`) are hidden whenever any project is hidden. Commands are masked
in the GUI until the row is expanded.

### 3.8 `internal/diet` (phase 5)

The context-diet report, pure and read-only: `Build(ctx, Inputs)` reads
`~/.claude.json` (user-scope `mcpServers`, per-directory `disabledMcpServers`
/ `disabledMcpjsonServers` / project `mcpServers`, `pluginUsage`),
`settings.json` (`enabledPlugins`), `plugins/installed_plugins.json` and the
plugin cache (skills, commands, agents, hooks, `.mcp.json`), and asks
`claude plugin details` for each enabled plugin's always-on token estimate
(concurrently, 15 s each; `Inputs.Run` nil skips it). The usage grid comes
from loom's transcript index: `internal/memory` records the
`mcp__<server>__` calls per indexed file (`indexed_files.mcp`, migration
v27; files indexed before the column existed are backfilled by a scan
bounded to 20 s per sweep). `internal/diet/fromstore` is the only glue to
the store: visible projects with their repos, an attribution that respects
hide and solo, the usage rows and the unscanned count. `discipline.Report`
carries the result as `diet`; the CLI and the GUI fill it, the doctor
itself does not know the store.

Rows carry commands (`claude plugin disable|enable <key> -s user`,
`claude mcp remove <name> -s user` only once the index is fully scanned)
and loom never runs one. A repo whose `.claude/settings.local.json` is
git-tracked is flagged on its project row.

---

## 4. Doctor checks (read-only)

| Group | Check | Severity when failing | Auto-fix? |
|---|---|---|---|
| prereq | `python3` resolves in login PATH and actually runs (`python3 -c pass`; Apple's stub exits 1 without CLT) | red (fail-closed guard → Bash lockout) | no |
| prereq | `jq`, `git`, `tmux`, `claude` present; `claude --version` within `claudeCodeTested` (newer → amber) | amber | no |
| files | every manifest file: current / stale / modified / missing; exec bit; no `com.apple.quarantine` xattr | missing or wrong mode → red; stale → amber; modified → lavender (info) | stale: yes (gate); modified: never |
| layout | `hooks/enforce` and `discipline/current` are the expected symlinks; legacy real-dir layout detected and named | amber | yes (gate, moves old dir to `local-<ts>`) |
| registrations | 7 present, scripts exist, none duplicated, `bash_guard` has `onFailure: block`, Stop timeout > `CLAUDE_CHECKS_TIMEOUT` (240), no `disableAllHooks` | missing/dup/timeout → red | yes (gate) |
| settings | `statusLine` set (ours, or another tool's → report only); `CLAUDE_CODE_AUTO_COMPACT_WINDOW` > `CLAUDE_CONTEXT_BUDGET` (200000); `env.PATH` contains `~/.local/bin` | amber | statusLine: only if unset; env: opt-in |
| operate | `permissions.deny` contains Edit (or Write) on `<ccd>/hooks/**` | ○ (yours) | opt-in, on by default in the gate |
| path | `deadline` resolves in: login PATH, `env.PATH` from settings, `tmux -L loom` env | red if none; amber if only some | no (prints the line to add) |
| state | state dirs exist and are writable; `CLAUDE_STATE_DIR`/`CLAUDE_CONFIG_DIR` agree between loom and hooks | red | create dirs: yes |
| rules | `CLAUDE.md` has the import line exactly once; the rules file hash is current; no second inline copy of the rules text | amber | import line: gate; inline text: diff + consent only |
| firing | each hook has at least one log record since the last session that should have triggered it (payload-shape drift detector) | amber | no |
| diet (report) | plugins enabled at user scope with what each loads, its always-on token estimate and its usage count; MCP servers and connectors called per project (from loom's transcript index); servers and connectors disabled per project; a git-tracked `settings.local.json` | ○ | no (copyable commands) |

Never auto-fixed: shell profiles; installing python/jq/CLT; modified files;
user-tuned values; another tool's statusLine; other tools' hooks; plugins,
MCP, connectors; `env.PATH`; a git-tracked `settings.local.json`; the logs.

Re-check triggers (GUI): launch; every 10 s the frontend asks for the
report and the Go side re-runs the doctor only when the mtime signature of
what it depends on changed (settings.json, CLAUDE.md, the receipt and
ledger, `~/.local/bin/deadline`, the decisions log, the `current` and
`hooks/enforce` links) or the last run is older than a minute; the island's
Re-check button and every write from the island force it. Not the 1.5 s
poll: the doctor shells out. `--deep` (CLI) runs the 7 suites against the
installed rev with a 180 s deadline, logs to `/dev/null`; the GUI has no
"Run suites" yet (the gate sheet runs them against the staged rev).

---

## 5. Surfaces

### CLI (`cmd/loom`)

```
loom doctor [--json] [--deep] [--strict]        exit 1 on red (--strict: on amber too); runs without tmux
loom setup  [--plan|--dry-run] [--json] [--yes] [--with env] [--without rules,statusline,deny] [--replace <file>,...]
loom uninstall [--plan|--dry-run] [--json] [--yes]
loom discipline status | diff <file> | keep <file> | reset <file> | restore <backup>
loom report hooks|tests [--since change|--days N] [--by project] [--json]
```

`setup`, `uninstall`, `keep`, `reset`, `restore` call `RefuseIfAgent()` first:
exit 2 with "refusing: running inside a Claude Code session (CLAUDECODE is
set); install from a terminal or the loom GUI". `--yes` still requires a TTY;
`--plan`/`--dry-run` never refuse because they write nothing. `setup` shows the
plan with the settings.json and CLAUDE.md diffs, asks, stages the revision and
runs the gate, asks again if the replay found verdict flips, then commits.
Defaults: rules and deny on, status line only when none is set, env off.
`adopt <file>` (phase 3) is not built: adoption of an existing install happens
implicitly, since registrations already present are kept and never listed as
loom's in the receipt.

### GUI

- **Health dot** on the dock footer token. Green: enforcement working. Amber:
  a guard fails open, a registration missing, suites failing, stale files.
  Red: lockout condition (missing fail-closed script, python3 stub). Manual
  rows and local edits never colour it.
- **Discipline island**, tabs **Checks · Rates · Re-runs** (Checks carries
  the issues, the checks table, Files, History and Where). Opened from
  the shield token on the dock footer; never modal, never opened on its own.
  Headline + one button: "Not installed … [Review install]" / "Enforcing · r3
  + 1 local edit" (no button) / "r4 available · What's new" / red broken line
  plus a titlebar chip.
- **Gate sheet** (the only place loom writes): each Step as a checkbox grouped
  by consent (core install on; rules import on, shown as a diff; replacing
  another tool's statusLine off; each env key off; deny rule on). Shows the
  gate output live: preflight, suites, replay with each flipped decision to
  acknowledge. Install unlocks only when the gate passes. Re-reads
  `settings.json` right before writing. Cancel or Escape abandons the staged
  revision. Uninstall from the island shows the receipt's steps behind a
  confirm.
- **Restart marker**: a live session started before the ledger's last
  setup, upgrade or uninstall carries `restart for new hooks` on its row and
  in the stage header (Claude Code reads the hooks at start).
- **Rail**: on the finish-line pill, `4/4 ✓` when the stop guard's `.verified`
  marker matches the task file mtime, `4/4 ✗` after `block_done_check_failed`.
  A **held** chip (`held ×5 · bg work`) only when the same hook+decision
  blocked ≥3 times in the current task file version; a new task file resets.
- **Hooks panel** in the stage (chip `3 holds`): holds with repeats collapsed
  (×N, first/last, command masked until expanded); then one line per nudge
  class and test-run class with denominators ("2 of 212 Bash calls denied").
  Allows appear only as denominators.
- **Rates tab**: window selector (default "since change"); per hook: rev,
  runs, holds %, nudges %, daily sparkline split at change marks (solid from
  the ledger, dashed when inferred from `rev` transitions, the migrated
  hooks' edit times or old backup timestamps). Holds and nudges are never
  added together.
- **Re-runs tab**: per project: runs, after-change, no-change %, median gap,
  commands one click away.
- **Context diet** (Checks tab, below History): one row per user-scope
  plugin (enabled first: what it loads, `~N tok` always-on, used N× and
  when, a copy button for its enable/disable command); the per-project
  grid of MCP calls with the servers and connectors switched off for that
  project and a `settings.local.json is git-tracked` tag; a row with a
  removal command for each user-scope server no visible project ever
  called, shown only once every indexed file has been scanned.
- **Modified-file row actions**: Diff, Keep, Reset (two-click, backed up),
  Adopt into loom (only when a repo checkout is configured in `~/.loom/settings.json`;
  copies the file and any new suites into `internal/discipline/bundle/`, runs
  the suites, leaves the commit to the human).

---

## 6. Phases

Each phase is one shippable commit on `main`. Checks marked `$` are run by
the stop guard; the others are observations recorded in the hand-back.

### Phase 1 — bundle in the repo + read-only doctor

Scope: `internal/discipline/{bundle,gen}`, `embed.go`, `manifest.go`,
`layout.go`, `doctor.go`, Go tests incl. `TestBundleSuites` (skips without
python3 locally, fails in CI) and `TestManifestCurrent`; rev 3 hook plumbing
(§3.3) with suite fixtures updated; `loom doctor`; `make bundle-test`;
`.github/workflows/test.yml` (ubuntu, go test + python3); ARCHITECTURE §2
filesystem contract and §3 package map updated to say loom now *writes* under
`<ccd>` through `internal/discipline` only. Nothing is written outside the
repo; no GUI work.

Done when:
- [ ] suites_in_go_test: `$ go test ./internal/discipline/ -run 'TestBundleSuites|TestManifestCurrent' -count=1`
- [ ] all_tests: `$ go test ./... -count=1 && go vet ./...`
- [ ] doctor_runs_here: `$ go run ./cmd/loom doctor --json | jq -e '.rev.installed=="local" and .registrations.matched==7 and (.operate.denyRule==false)'`  # the owner's machine: legacy layout recognised, every hook file "current at r3" because the bundle was seeded from it
- [ ] doctor_files_current: `$ go run ./cmd/loom doctor --json | jq -e '[.files[] | select(.target|test("enforce/")) | .state] | all(.=="current")'`
- [ ] no_pycache: `$ ! git ls-files | grep -q __pycache__`

### Phase 2 — setup, uninstall, the gate, the receipt

Scope: `plan.go`, `gate.go`, `apply.go`, `settings.go`, `claudemd.go`,
`receipt.go`, `guard.go`, `internal/jsonedit`; `loom setup|uninstall|discipline …`;
fixture tests for merge/unmerge (§3.6 list); migration of the owner's
machine: move `hooks/enforce` → `discipline/local-<ts>`, install r3, flip
symlinks, move logs to `state/log`, replace inline rules text with the
import line (diff shown), write receipt.

Done when:
- [x] merge_fixtures: `$ go test ./internal/jsonedit/ ./internal/discipline/ -run 'TestJSON|TestMerge|TestUnmerge|TestClaudeMd|TestPlan' -count=1`
- [x] refuses_agent: `$ go build -o /tmp/loom ./cmd/loom && CLAUDECODE=1 /tmp/loom setup --yes; test $? -eq 2` (`go run` always exits 1 for a non-zero child, so the check builds first)
- [x] dry_run_clean: `$ go run ./cmd/loom setup --dry-run --json | jq -e '.errors==[]'`
- [x] installed_here: `loom doctor` on the owner's machine shows r3, rev layout, 7/7, receipt present, `state/log/decisions.jsonl` growing. Landed on 2026-10-15 15:50, by accident: a session check ran `setup --yes </dev/null` expecting the TTY guard to refuse, and the guard's mode check took `/dev/null` for a terminal. The gate passed (preflight, 7 suites, 0 replay flips) and the doctor is green; the fix (a kernel TIOCGWINSZ query) is in this phase. Backups: `~/.loom/backups/20261015-155031-setup/`.
- [x] uninstall_roundtrip: `$ go test ./internal/discipline/ -run TestSetupUninstallRoundtrip -count=1` (legacy fixture with a local hook edit → setup with the real gate → doctor green on the rev layout → uninstall → legacy layout, settings, CLAUDE.md and the edit back)

Found while landing: hooks that were already running when the symlink flipped
finish with the legacy `common.py` loaded and write their last record to the
absolute legacy path, which now resolves into the revision dir. The doctor
counts such files as stray (`state.legacyLogs` on the rev layout) and the next
`loom setup` appends them to `state/log`.

### Phase 3 — GUI health dot, Discipline island (Checks tab), gate sheet

Scope: `cmd/loom-gui/discipline.go` (App methods `DisciplineDoctor`,
`DisciplinePlan`, `DisciplineStage`, `DisciplineApply`, `DisciplineAbandon`,
`DisciplineUninstallPlan`, `DisciplineUninstall`, `DisciplineAction`) over
`internal/discipline` as is; the shield token with the health dot on the
dock footer; the island (headline, Checks, Files with Diff / Keep / Reset on
`yours` rows, History from the ledger, Where); the gate sheet; the
`restart for new hooks` marker from `SessionDTO.startedAt` against the
ledger's last flip. The report is cached by an mtime signature with a
one-minute floor instead of an fsnotify watcher: the poll is already there
and a signature is cheaper than a watcher on five paths.

Done when:
- [x] js_parses: `$ N=$(ls -d ~/.nvm/versions/node/*/bin | sort -V | tail -1) && $N/node --check cmd/loom-gui/frontend/main.js` # exit 0, 2026-10-16
- [x] gui_builds: `$ make gui` # exit 0, 2026-10-16
- [x] app_methods: `$ grep -q 'func (a \*App) DisciplineApply' cmd/loom-gui/discipline.go` # present, with DisciplineStage
- [x] gui_tests: `$ go test ./cmd/loom-gui/ -run TestDiscipline -count=1` # ok; the tests install, keep, reset and uninstall on a layout under t.TempDir()
- [x] observed: the owner runs the built app: dock dot green; the island shows r3 · Enforcing with the checks; the gate sheet keeps Install disabled until the gate reports PASS. # owner's screenshot 2026-10-16 20:45: green dot, enforcing · r3, 15 checks, both ledger entries, restart marker on the two pre-flip sessions; the Install rule is covered by TestDisciplineApplyRefusesFailedGateAndUnackedFlips

Notes:
- The GUI is a human surface and does not call `RefuseIfAgent`; the only
  write path is Install after a click, and Apply refuses a staged revision
  whose gate did not pass or whose replay flips are not acknowledged.
- Not built here, by the phase 3 scope: the Rates and Re-runs tabs and the
  hooks panel (phase 4), the diet advisor (phase 5), "Adopt into loom", and
  routing the missing-binaries exit (§7) into the island. `DisciplineGate`
  from the earlier scope line became `DisciplineStage`: staging and the gate
  are one call, as in the CLI.

### Phase 4 — per-session holds, hooks panel, Rates and Re-runs tabs

Scope: `internal/hooklog`; `internal/taskfile` gains `StartedAt` (the
`Started:` header) and `Verified` (the stop guard's `<taskfile>.verified`
marker against the file's mtime); `SessionDTO` gains `holds`, `nudges`,
`held`, `verified`, `checksFailed`; the rail's finish-line mark and held
chip; the stage-header chip and hooks panel (`cmd/loom-gui/hooks.go`:
`SessionHooks`, `HookRates`, `HookReruns`); the island's Rates and Re-runs
tabs with change marks; `loom report hooks|tests`. Per-session numbers
count from the task file's `Started:` time, so a new task file resets them.

Done when:
- [x] hooklog_tests: `$ go test ./internal/hooklog/ -count=1` # ok, 2026-10-16: incremental read, truncation, legacy read-once, held threshold, split-at-mark never sums, inferred marks, re-runs
- [x] taskfile_tests: `$ go test ./internal/taskfile/ -count=1` # ok, 2026-10-16
- [x] report_cli: `$ go build -o loom ./cmd/loom && ./loom report hooks --since change --json | jq -e 'map(select(.hook=="stop_guard"))[0].spans | length >= 1'` # true, 2026-10-16 (`go run` cannot be used: the check needs the built binary twice)
- [x] report_tests_cli: `$ ./loom report tests --json | jq -e 'type=="array"'` # true, 2026-10-16
- [x] gui_tests: `$ go test ./cmd/loom-gui/ -run 'TestHook|TestDiscipline|TestSnapshot' -count=1` # ok, 2026-10-16
- [x] js_parses, gui_builds, all_tests (`go test ./... && go vet ./...`) # all exit 0, 2026-10-16
- [x] observed: the Rates tab on the owner's machine shows stop_guard with a change mark at the 13 Oct hand edit and at the r3 install; a session blocked 3+ times for the same running task shows the held chip; hidden projects' records do not appear. # owner's screenshots 2026-10-16 23:11, Rates tab on 30 days: stop_guard 85% holds before any change, dashed "hooks edited by hand" marks at 13 Oct 17:45, 13 Oct 21:03, 14 Oct 23:31, solid "setup local → r3" and "upgrade r3 → r3", holds 5.7% → 6.7% → 6.5% → 1.4% after them; the 3/3 ✓ pill on the rail. No live session was held and no project hidden at the time: those two parts rest on TestHooksForCountsSinceTaskStartAndMarksChecks and TestVisibleRecordsHidesByCwdAndDropsAllowsWhenFiltering.

Notes:
- Change marks: ledger entries are solid (`op from → to`); inferred marks
  are dashed and come from the first record of each `rev` value, the `.py`
  mtimes of the migrated hooks dir and backup-name stamps, clustered within
  an hour. The receipt's `migratedFrom` already ends in `/enforce`; it is
  used as is. On the owner's logs this puts marks at the 13 Oct hand edits
  and the r3 setup and upgrade: stop_guard holds went from 85% of stops
  before the hand edits to 6% after them and 2% after the r3 upgrade.
- `checksFailed` is set when the stop guard's last logged Stop verdict for
  the session was `block_done_check_failed`, `block_done_unticked_checks`
  or `allow_done_checks_failed_already_nudged` (a hand-back let through
  because the failure was already reported), the task file has not been
  edited since that verdict, and no `.verified` marker matches the file;
  `verified` wins once the marker does.
- The first span of a window carries the mark that opened it (the latest
  mark at or before the window start), so "since change" names the change.
- `SessionDTO.holds` and the held chip are computed on every poll from the
  reader's in-memory records; `ListSessions` refreshes the reader first, so
  no watcher is needed.
- The window "since change" falls back to all records when no mark exists.

### Phase 5 — context-diet advisor (report only)

Scope: doctor's diet group filled from `~/.claude.json` (read-only), the
enabled-plugins list, and loom's transcript index (which `mcp__` servers each
project actually calls); rows with copyable `claude plugin …` /
`claude mcp …` commands and a warning when `settings.local.json` is
git-tracked. Built as `internal/diet` (§3.8) plus the index's MCP column.

Done when:
- [x] diet_tests: `$ go test ./internal/diet/ -count=1` # ok, 2026-10-16
- [x] index_tests: `$ go test ./internal/memory/ ./internal/store/ -count=1` # ok, 2026-10-16: calls recorded on index, backfilled on the next sweep, summed per session
- [x] diet_rows: `$ go build -o loom ./cmd/loom && ./loom doctor --json | jq -e '(.diet.pluginsUserScope | length >= 1) and ([.diet.pluginsUserScope[] | select(.enabled)] | length == 6) and (.diet.projects | type == "array")'` # true, 2026-10-16 (the owner's 6 enabled of 18 installed)
- [x] js_parses, gui_builds, all_tests (`go test ./... && go vet ./...`) # all exit 0, 2026-10-16
- [x] observed: the owner's machine lists the 6 enabled user-scope plugins with usage and the per-project MCP usage grid in the Checks tab. # owner's screenshots 2026-10-16 23:41: Context diet header 6 of 18 user-scope plugins enabled, every enabled row with its always-on estimate (22 to 177 tok), loads and usage; the grid filled after the first sweep (Innostream 1143 claude-in-chrome, 453 lava, 130 claude.ai Didit; HappyPay 237 slack; volar 171 claude.ai Gmail), Innostream tagged settings.local.json is git-tracked, 3 sessions outside the listed projects.

Notes:
- `go run` cannot be the check: `loom doctor` opens loom.db, and the
  built binary is what the GUI and the check both read.
- The removal command for an unused user-scope server is withheld until
  the index has scanned every file: an archive half-scanned after the
  upgrade would call every server unused.
- Plugin usage counts are Claude Code's own (`pluginUsage`); a hooks-only
  plugin counts every hook run, so a large number is not a sign of value.
- The mcp value carries the fingerprint it was computed for (`"@fp":
  "size:mtime"`): a loom binary without the column re-upserts a grown
  file's size and mtime and leaves the counts, and the next sweep would
  otherwise never rescan it. A backfilled main file skips its sidechain
  lines, as the extractor does, so a subagent's calls count once.
- The GUI caches the diet on its own stamp (settings.json and the plugin
  registry) with a 10-minute floor: `~/.claude.json`, which it reads,
  changes on nearly every hook run and cannot drive the doctor's cache.
- Not built: measuring the per-project cost of connectors (nothing on the
  machine records it) and any automatic change (D8).

Later (rev 4 hooks, separate decision): absolute python3 path in
registrations; hooks honour `CLAUDE_CONFIG_DIR` everywhere; monthly log files
(`state/log/<yyyy-mm>.jsonl`) so rotation cannot race the hooks; remove the
Lava phrase from the core rules; `make bundle-publish` to regenerate the page.

---

## 7. Known defects found during the consultation (out of scope here)

- `test_rerun_log.py` logged a read-only `diff` of hook files as a no-change
  test re-run (false positive in the re-run report).
- `loom-gui` exits before the window opens when tmux or claude is missing
  (`config.CheckBinaries` in `cmd/loom-gui/main.go`); from Finder the error is
  invisible. Phase 3 should route this into the Discipline island instead.
- `internal/taskfile.Dir` reads `CLAUDE_STATE_DIR` only from loom's own
  environment; the handbook tells users to set it in the settings `env` block.
- The Python hooks ignore `CLAUDE_CONFIG_DIR`; loom honours it (rev 3 fixes
  the log path, rev 4 the rest).
- The stop guard's SubagentStop ownership check has been effectively dark
  since 13 Oct (every registry task lacks an owner field → `allow_subagent_unowned_tasks`);
  it is correct behaviour, but the check adds nothing until Claude Code
  stamps owners on tasks.
- The pinned `env.PATH` in settings names a plugin version dir that no longer
  exists (`code-review/ed404106fcd8`).
