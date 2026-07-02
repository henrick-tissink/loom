# Spike: `--session-id`, readiness/trust markers, resume behavior

Date: 2026-07-02
Environment: macOS (Darwin 25.3.0), `claude --version` → **Claude Code v2.1.198**, UI branding observed in-app: **"Fable 5"**. tmux 3.x via `/opt/homebrew/bin/tmux`, isolated socket `-L loomspike`. Working directory under test: `/Users/henricktissink/Sauce/loom` (git repo, branch `phase-1-cockpit-core`).

**Method note (deviation from brief, authorized):** this spike was run by a non-interactive agent. Where the brief said "attach interactively and answer the trust dialog," capture-pane + send-keys was used instead. Also: the agent process this spike ran from is itself a nested Claude Code / Agent-SDK session, so its shell environment carries `CLAUDECODE=1`, `CLAUDE_CODE_CHILD_SESSION=1`, `CLAUDE_CODE_ENTRYPOINT=cli`, `CLAUDE_CODE_SSE_PORT`, `CLAUDE_CODE_SESSION_ID`, `AI_AGENT`, `CLAUDE_EFFORT`. tmux inherits environment from its invoking shell, so the first probe (raw launch) risked testing a *nested* claude, not the standalone process Loom will actually spawn. All tmux sessions from Step 1 onward were launched with these vars explicitly unset (`env -u ...`) to better approximate a real Loom-spawned subprocess. This made no observable difference to readiness/trust behavior, but is recorded for reproducibility.

---

## 1. Does `claude --session-id <fresh-uuid>` start a clean NEW interactive session? Does `<uuid>.jsonl` appear at the expected path?

**Yes, cleanly**, with one important timing caveat.

- `claude --session-id $UUID` launched in a fresh tmux pane started with no errors, no collision, and went straight to the normal welcome screen → ready input box for the given UUID.
- `<UUID>.jsonl` does **NOT** appear immediately at process launch. It is created **lazily on the first turn** (first prompt + response), not at startup. Before any prompt was sent, `~/.claude/projects/-Users-henricktissink-Sauce-loom/` contained no file for the fresh UUID at all.
- After sending one seed prompt (Step 3 below) the file appeared at exactly the expected path:
  `~/.claude/projects/-Users-henricktissink-Sauce-loom/<UUID>.jsonl`
- Confirmed the encoded project-directory name for `/Users/henricktissink/Sauce/loom` is:
  `-Users-henricktissink-Sauce-loom`
  — i.e. every non-`[a-zA-Z0-9]` character (both `/` in the path) is replaced with `-`, confirming the candidate encoding rule.
- Every JSON line in the transcript carries a `"sessionId"` field; all 30 lines in the test transcript had the **same** `sessionId`, equal to the UUID passed via `--session-id`. No stray/second UUID ever appeared.

**Implication for Loom:** correlating a launched pane to its transcript file by UUID works, but Loom must not expect the `.jsonl` to exist immediately after spawn — it must poll/wait for file creation (first turn), not just process start. This matters for Task 6's `NewestSince`/correlation logic.

---

## 2. EXACT ready-marker text (candidate `? for shortcuts`)

**The candidate is WRONG for this build/branding. `? for shortcuts` does not appear anywhere in the pane, ever** (verified by exhaustive grep across multiple captures, idle and post-response, over several minutes). This app build shows no persistent shortcuts hint at all.

What was actually observed, verbatim (captured via `tmux capture-pane -p`, `cat -n`/`nl` line numbers included for context):

**Idle/ready state** — bottom of pane, stable, no spinner line present:
```
────────────────────────────────────────────────────────────────────────────
❯ 
────────────────────────────────────────────────────────────────────────────
  Fable 5 | 📁loom | 🔀phase-1-cockpit-core (cmd/loom/main.go uncommitted, no upstream) | ░░░░░░░░░░ ~2% of 1M tokens
  ⏵⏵ auto mode on (shift+tab to cycle) · ← for agents
```
- The literal prompt glyph is `❯` followed by a single space (in raw bytes, actually `❯\xa0` — a non-breaking space) — this is the same glyph shown in *both* idle and busy states, so **by itself it is not sufficient** as a ready marker.
- The trailing status bar (`⏵⏵ auto mode on ...`) is *also* present during busy/generating states — **not** a ready marker either.

**Busy/generating state** — an animated spinner line appears above the input box, absent when idle:
```
✳ Architecting…
```
```
· Architecting… (6s · thinking)
```
```
✽ Determining…
```
The glyph cycles through frames (`✢ ✳ ✻ ✽ ·` observed), paired with a present-participle verb + ellipsis (`Architecting…`, `Determining…`), and after ~a few seconds gains a `(Ns · thinking)` suffix with an incrementing elapsed-seconds counter.

**Just-completed state** (transient, appears once, then response text renders):
```
✻ Sautéed for 6s
✻ Churned for 12s
```
Past-tense summary, no ellipsis, static (does not re-increment) — this line lingers briefly after the response but is not the steady-state ready condition; on the next capture it's gone and only the plain `❯` input box remains.

**Corrected READY_MARKER definition (recommended for Loom):** readiness is **not** a fixed substring to match — it is the **absence** of the busy-spinner line pattern (regex sketch: `^[✢✳✻✽·⠋⠙⠹⠸⠼⠴⠦⠧⠇⠏]\s+\w+…`) combined with presence of the bare `❯` prompt line. In practice: poll capture-pane; call it "ready" only after **two consecutive captures a few seconds apart** show no spinner line — a single snapshot can catch the transient "done" summary line and misfire.

No "esc to interrupt" hint text (common in some Claude Code builds) was observed in this build either — grepped for, not present.

---

## 3. EXACT trust-dialog text (candidate `Do you trust the files in this folder?`)

**NOT OBSERVED — could not be triggered in this environment, for verifiable, documented reasons. Do not treat the candidate string as verified.**

Investigation:
- `/Users/henricktissink/Sauce/loom` itself has `hasTrustDialogAccepted: false` in `~/.claude.json` (`projects["/Users/henricktissink/Sauce/loom"]`) — i.e. per-project trust was never explicitly granted for `loom/`.
- However **no trust dialog appeared** across three separate launch attempts: (a) raw nested-env launch, (b) env-stripped launch approximating a real subprocess, (c) env-stripped launch with `--settings '{"permissions":{"defaultMode":"default"}}'` overriding the global auto-approve mode inline (no files modified).
- Root cause, confirmed by inspecting `~/.claude.json`: the **parent directory** `/Users/henricktissink/Sauce` has `hasTrustDialogAccepted: true`. Trust appears to be **inherited from an already-trusted ancestor directory** — `loom/` is a subdirectory of an already-trusted `Sauce/`, so the dialog is skipped even though `loom/`'s own flag is `false`.
- Additionally, this machine's global `~/.claude/settings.json` sets `"permissions": {"defaultMode": "auto"}` and `"skipAutoPermissionPrompt": true`, which independently suppresses trust/permission prompts (confirmed via the `⏵⏵ auto mode on` footer visible in all captures using the default global settings).
- A genuine fresh-onboarding attempt was made (`CLAUDE_CONFIG_DIR` pointed at an empty scratch config) to force a true first-run — this successfully reproduced the **theme-selection** onboarding screen (not asked for by the brief, but confirms onboarding flow triggers correctly), but then required an interactive OAuth login (no cached credentials in the scratch config), which is out of scope/unsafe to automate. Aborted before reaching the trust dialog.
- Deliberately clearing trust on the real `~/Sauce` parent entry to force-reproduce the dialog on this developer's real config was considered and **rejected** as unsafe (it is the user's live global state, shared by other trusted projects like `gloom`, `tavli`, etc.; a crash mid-spike before restoring it would leave those untrusted).

**What this means for Loom, concretely:**
- On **this developer's actual machine**, the trust dialog will effectively **never fire** for any project under `~/Sauce/` (all inherit trust from the parent), nor for any project on a machine with global `defaultMode: "auto"`. `TRUST_MARKER` detection is real-world-dead-code for this user's primary dev machine today.
- It should still be **implemented defensively** for portability (other users, other directories, machines without `defaultMode: auto`), but its exact text is **unverified** and must not be hardcoded from memory/assumption. Recommend either (a) re-running this spike against a genuinely untrusted, non-`~/Sauce`-nested directory with a throwaway `CLAUDE_CONFIG_DIR` + real login before Task 5/13 ship trust-handling logic, or (b) making trust-dialog handling best-effort/soft-fail (log + surface to user) rather than a hard-coded string match, since it could not be empirically confirmed.

---

## 4. Resume behavior: same `<uuid>.jsonl` or new file?

**Confirmed: `claude --resume <uuid>` appends to the SAME `<uuid>.jsonl`. It does NOT create a new file.**

- Before resume: `a2d7cad4-3d42-4553-92a2-f2fc79887a43.jsonl` was 69,671 bytes (after the seed "pong" turn).
- Killed the tmux session, started a fresh tmux session running `claude --resume a2d7cad4-3d42-4553-92a2-f2fc79887a43`.
- After resume launch + idle: `ls -t .../*.jsonl` showed **only one** `.jsonl` file in the whole project directory — the same UUID. No second file appeared.
- The file grew slightly on resume (a `{"type":"last-prompt", ...}` metadata line was appended, still carrying the identical `sessionId`).
- Sent a further prompt in the resumed session; the response was appended to the same file; `grep -o '"sessionId":"[^"]*"' | sort -u` across the entire 30-line/81KB final transcript returned exactly **one** unique sessionId, matching the original UUID throughout.

**Decision for Task 13:** resume reuses the same file — **`Launcher.Resume` does NOT need a `NewestSince` fallback for the common case** (session-id known, file already exists). Per the brief's own instruction, this should simplify Task 13 accordingly. `transcript.NewestSince` is still worth building anyway (per Task 6) as a fallback for cases where the session-id isn't known upfront (e.g. very first launch, before the file exists — see Finding 1's lazy-creation caveat) or if a future claude version changes this behavior, but it is not load-bearing for the resume path specifically.

---

## 5. Idle-activity probe: does `session_activity` advance while claude idles?

**No — `session_activity` did NOT advance while idle at the input box.**

```
A1: spike2 1782996979
A2: spike2 1782996979   (+5s, unchanged)
A3: spike2 1782996979   (+10s, unchanged)
```
Sampled 3 times, 5 seconds apart, while the resumed session sat idle at its empty input prompt (no spinner, no in-flight request). `session_activity` (tmux's last-activity Unix timestamp for the session) stayed byte-identical across all three samples — no idle-spinner/cursor-blink registers as pane activity in this build.

**Implication for Task 12:** `paneActive`/fusion-heuristic logic can use `session_activity` directly as a genuine idle signal — **no larger threshold or dropping is needed**, since it does not spuriously advance while claude is truly idle. (Not independently verified here: whether `session_activity` *does* advance during active generation/spinner — this was not part of Step 5's scope, but Step 3/generation captures showed visibly different pane content across time during a busy spinner, which would be expected to bump `session_activity`; worth a quick confirmation in a later task if `paneActive` timing precision matters.)

---

## Summary of corrected constants

| Constant | Brief's candidate | Verified value |
|---|---|---|
| `READY_MARKER` | `? for shortcuts` | **Not a fixed string.** Ready = absence of busy-spinner-line pattern (`^[✢✳✻✽·]\s+\w+…`) for 2 consecutive polls, with bare `❯` prompt visible. Candidate string does not exist in this build (Claude Code v2.1.198, "Fable 5" branding). |
| `TRUST_MARKER` | `Do you trust the files in this folder?` | **Unverified — dialog could not be triggered in this environment** (trust inherited from already-trusted parent `~/Sauce`, plus global `defaultMode: auto`). Do not hardcode from assumption; re-verify against a genuinely untrusted directory before relying on it. |
| `--session-id` fresh launch | assumed clean | **Confirmed clean.** `<uuid>.jsonl` created lazily on first turn, not at launch. |
| `--resume` transcript behavior | assumed same-file (plan hedges with `NewestSince` fallback) | **Confirmed: same file, same `sessionId` throughout.** `Launcher.Resume` (Task 13) does not need the `NewestSince` fallback for its primary path. |
| `session_activity` while idle | assumed possibly advancing (spinner) | **Confirmed static/non-advancing while idle.** `paneActive` (Task 12) can use it directly without a larger threshold. |

## Decision-matrix outcome

Per the brief's decision matrix: `--session-id` **does** cleanly create a fresh session (Finding 1) — proceed unchanged, no fallback-only architecture needed for Tasks 5/13. `transcript.NewestSince` (Task 6) should still be built as originally planned (useful for the pre-first-turn window and as a defensive fallback), but per Finding 4, Task 13's `Launcher.Resume` can be simplified to not depend on it for the common resume case.

## Open follow-ups for later tasks

1. Re-verify `TRUST_MARKER` text against a genuinely untrusted directory (outside `~/Sauce`, fresh `CLAUDE_CONFIG_DIR` + real login) before Task 5/13 hard-code any trust-dialog handling.
2. Confirm `READY_MARKER`/busy-spinner regex against at least one more prompt shape (e.g. one that triggers a tool call, not just plain text) to make sure the spinner-absence heuristic holds when tool-use lines are interleaved.
3. Confirm whether `session_activity` advances during the busy/spinner state (not just idle) if Task 12's timing precision depends on distinguishing "generating" from "idle" via tmux metadata alone.
