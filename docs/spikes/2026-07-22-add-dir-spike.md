# Spike: `--add-dir` trust, permissions, resume, and transcript keying

Date: 2026-07-22
Environment: macOS (Darwin 25.3.0), `claude` **v2.1.217** (UI branding "Fable 5", Claude Max), tmux via isolated socket `-L loomspike2`. Real `CLAUDE_CONFIG_DIR` (`~/.claude`, 65 known projects).

**Method note.** The spec required a *genuinely untrusted* directory, because the 2026-07-02 spike came back inconclusive on trust: it ran inside an already-trusted ancestor and never triggered the dialog. Two fresh git repos were created at `/tmp/loom-addir-spike/{primary,sibling}` and `~/.claude.json` was checked first to confirm neither they nor any ancestor (`/`, `/tmp`) carried `hasTrustDialogAccepted`. The launch replicated Loom's exact argv shape, including `--settings '{"theme":"light"}'` and shell quoting.

The session ran in **auto mode** (the account default), which confounds question 3 — see there.

---

## 1. Does `--add-dir` against an untrusted sibling prompt?

**No. Only the primary cwd prompts, and the sibling is granted silently.**

The dialog on launch named the primary only:

```
 Accessing workspace:
 /private/tmp/loom-addir-spike/primary
 Quick safety check: Is this a project you created or one you trust? (Like your own
 code, a well-known open source project, or work from your team). If not, take a
 moment to review what's in this folder first.
 Claude Code'll be able to read, edit, and execute files here.
 Security guide
 ❯ 1. Yes, I trust this folder
   2. No, exit
 Enter to confirm · Esc to cancel
```

After confirming, no second dialog appeared for the sibling, and a `Write` to `/tmp/loom-addir-spike/sibling/probe.txt` **succeeded immediately** with no permission prompt and no `Allowed by …` annotation. The file was created with the expected contents.

**BINDING FINDING — the shipped trust marker was wrong.** `session.DefaultTrustMarker` was `"Do you trust the files in this folder?"`. That string **does not appear anywhere** in this build's dialog. The marker has never matched on v2.1.217, so `waitReady`'s trust gate was inert and the seed's only protection against being typed into the trust dialog was luck. The generic `selectCursorPattern` (`❯` followed by a numbered option) *does* match `❯ 1. Yes, I trust this folder`, and is what actually closes the hazard.

Consequences applied:
- `DefaultTrustMarker` updated to `"Is this a project you created or one you trust?"`, and demoted in the comment to belt-and-braces behind the version-independent select-cursor test.
- `TestWaitReadyBoundsTrustPendingWait` had hardcoded the stale string in its fixture, so it had stopped exercising the trust path without failing. It now derives the pane text from `l.TrustMarker` and cannot go stale that way again.

**Branch table (question resolved):** *prompts once, for the primary cwd only.* Loom needs no per-add-dir trust handling; the existing single gate is sufficient, provided the dialog is detected — which is now the select-cursor test's job, not the marker's.

## 2. Does the transcript still key on `Cwd` alone?

**Yes — but on the PHYSICAL cwd, which is not what Loom was storing.**

tmux was given `-c /tmp/loom-addir-spike/primary`. Claude recorded:

- transcript directory: `~/.claude/projects/-private-tmp-loom-addir-spike-primary/`
- `"cwd"` in every record: `/private/tmp/loom-addir-spike/primary`

`/tmp` is a symlink to `/private/tmp` on macOS, and any process's `getcwd()` returns the resolved path. So the transcript directory name is derived from the **physical** cwd regardless of the path Loom passes.

**BINDING FINDING — this invalidates spec §4's rule.** The spec said *"NEVER `EvalSymlinks` a stored cwd"*, reasoning that resolving would break transcript lookup. The opposite is true: storing the unresolved form makes `transcript.Path()` compute `-tmp-x` while claude wrote `-private-tmp-x`, so `transcript.Reader` opens a file that does not exist and degrades to `StateUnknown` forever. The session shows no title, no context-token gauge, never reaches `needs_you`, and workflow `{{prev.*}}` extraction silently yields empty. The adversarial spec review endorsed the same wrong rule; only the experiment caught it.

Note this bug predates the projects work — it affects any session launched into a path with a symlinked ancestor. It also produced an inconsistency: `status.Engine` adopts orphan sessions with `#{pane_current_path}`, which *is* physical, so adopted sessions worked while Loom-launched ones did not.

Consequences applied: `session.physicalDir` resolves cwd and every add-dir at `Launch` and `Resume`, after validation (so errors still name the path the user gave) and before both tmux and the store. `Resume` resolving as well repairs older rows. Regression pinned by `TestLaunchStoresPhysicalCwd`, which asserts the stored cwd equals the physical target and that `ProjectDirName` agrees.

## 3. Does `claude --resume` restore add-dirs?

**Almost certainly not — but this test was confounded and the conclusion is inferred, not proven.**

The session was killed and relaunched as `claude --resume <uuid>` with **no** `--add-dir`. A write to the sibling still succeeded — but the tool result carried an extra line absent from the original run:

```
⏺ Write(/tmp/loom-addir-spike/sibling/probe2.txt)
  ⎿  Wrote 1 line to ../../../../tmp/loom-addir-spike/sibling/probe2.txt
  ⎿  Allowed by auto mode classifier
```

Under `--add-dir` the write needed no such adjudication; on resume it fell through to the auto-mode classifier. That difference is the evidence that the add-dir grant was **not** restored — auto mode merely masked its absence. A resumed session under `default`, `plan` or `acceptEdits` would likely have been blocked or prompted.

Not proven because the account default is auto mode and the spike did not re-run under a stricter permission mode. **Loom's behaviour is correct regardless**: `Resume` re-passes the persisted `AddDirs`, so the grant is restored explicitly rather than depended upon. This finding explains why that matters instead of leaving it as defensive habit.

## 4. Incidental

- Claude's status line shows only the primary (`📁primary`); add-dirs are not surfaced in its chrome, so a user cannot see the extra grant from inside the session. Loom's own add-dir list is the only place it is visible.
- Version drift is real and fast: v2.1.198 → v2.1.217 between spikes, and it changed the trust dialog wording. Anything Loom matches against claude's rendered UI should be shape-based, not string-based, wherever a shape exists.

## Open

Question 3 deserves a follow-up under `--permission-mode default` to convert an inference into a measurement. Low priority: the code already does the safe thing.
