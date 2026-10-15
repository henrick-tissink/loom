## How to work

### Finish line first
- Before starting, pin down three things: **Outcome** (the behaviour or deliverable),
  **Scope** (what this task covers), **Done when** (the observable result and the checks
  that prove it). If the request doesn't make them clear, state your reading in one line
  and proceed; ask only if a wrong guess would change the outcome.
- The finish line is a file, not a thought. On the first prompt of a session the hook
  gives the path (`~/.claude/state/tasks/<session>.md`) and the shape; write it before the
  first edit, with named Done-when checks. A check written as `name: $ command` is run by
  the stop guard and holds when the command exits 0; use that form whenever a command can
  prove the check, with the evidence after a trailing `#`. A question or consultation gets
  `Status: consultation` and one line. When the work is finished, tick each check with
  its evidence and set `Status: done` before the hand-back; the stop guard blocks a "done"
  hand-back once while the file still says open. A new task in the same session rewrites
  the file.
- For larger work, answer in one sentence: "What can the user do or see when this lands
  that they can't now?" If the answer is scaffolding, a framework, or orchestration
  rather than the feature, stop and say so.
- Keep unrelated improvements out of the task. Report them briefly (or as a Lava finding).

### Work directly
- Do not spawn implementing subagents or teams unless I explicitly ask. A few shell commands
  don't need a team. A read-only Explore subagent for investigation that would otherwise fill
  the main context (many files, long logs) is fine without asking; say so in one line.
- When I do ask for parallel work: you assign each job explicitly, bound it, collect every
  result yourself, and set the model explicitly (use a cheaper model for routine
  collection). Helpers you spawn must not spawn their own.
- A helper is a lean worker that owns one delivery: it gets the repository instructions,
  the task (the task file content verbatim, or the slice of it that it owns), and the
  coordination interface. Do not pass it your memory, personal tooling,
  or the conversation history it doesn't need.
- After reading a helper's result, stop it with TaskStop in the same turn. The
  registry reports idle helpers as running until they are stopped.
- Batch independent tool calls. Return conclusions, not raw transcripts or file dumps.
- Independent verification: when a delivery task changes a hook, a settings file, or more
  than a handful of files, run one fresh-context verification subagent before the hand-back.
  It sees only the diff and the task file's Done-when, and reports only gaps that affect
  correctness or the stated checks; everything else is optional, per the review stopping
  rule. Stop it with TaskStop after reading its result.

### Questions
- Resolve routine implementation choices yourself. Ask only when the answer changes the
  outcome, scope, authority, or an irreversible choice.
- Every question: the decision in plain language, your recommendation and why, and the
  tradeoff. Put a descriptive label beside any identifier (ticket, commit, finding ID) so
  I don't have to reconstruct context. One question at a time.

### Consultation mode
- A question, a review request, or thinking aloud is a consultation: the deliverable is the
  assessment. Read and measure as needed, change nothing, and end with findings plus at most
  one recommended next action. The task file gets `Status: consultation`.
- Switching from consultation to delivery needs my explicit ask ("do it", "fix it", "install").
  "Should we…?" and "what would it take…?" are still questions.

### Testing and review have a stopping rule
- Run checks that match the change: the repo's build or compile/type/lint check and the
  tests for the changed code and its callers. Run them once; repeat only after a code
  change, a new failure, or to resolve a named concern.
- The full suite is an integration gate, not an iteration step. If the targeted set would be
  most of the suite, run the changed behaviour's tests and call the change cross-cutting.
  Solo work runs the full suite once, before the hand-back.
- Helpers never run full suites. A helper's Done-when is its targeted checks green plus a
  draft MR/PR (`--draft` in glab and gh) marked "full suite pending". Its report says whether
  the change is cross-cutting: shared types or signatures, DI, middleware, auth, test
  fixtures, migrations, global config. A full disk or other shared-machine problem goes to
  the coordinator; helpers don't clean up shared state.
- The coordinator gates each repo once its helpers are done, one full suite at a time per
  machine, on a local unpushed branch that merges all that repo's MRs onto current main. A
  conflict between MRs comes to me unresolved. CI replaces this run only for a single MR,
  and only if it runs the full suite before merge and a red result blocks the merge.
- A hang is a failure: give every run a deadline and a per-test hang timeout. On failure,
  rerun the failing tests alone on the same branch; a pass marks them flaky (add them to the
  project's known failures). A repeat failure is run on main (failing there: pre-existing,
  a finding) and on each MR branch: the failing branch is the cause, and if every branch
  passes alone, the combination is.
- When the gate is green, take the MRs out of draft; merging stays mine. A push after the
  gate reopens it.
- No mutation testing, watch-mode tests, or adversarial/"keep finding problems" review
  loops unless I ask for them.
- A review finding needs one of: a realistic trigger and a concrete wrong result, a
  meaningful test gap, or a clearly labelled design cost. Hypothetical hardening is not
  automatically work. Finding nothing is a valid review result.

### Long-running work and background jobs
- Give every long-running command a deadline and keep its exit result. Background commands
  must start with `deadline <duration> ...` (e.g. `deadline 20m bash -c '...'`); it kills the
  whole process group on expiry (exit 124). Never detach with nohup/disown/setsid/trailing `&`.
  A hook blocks both.
- Before handing back, account for every background job and child process you started:
  finished (with result), or stopped.
- Never end with "I'll watch it and report back" unless a real completion mechanism is
  registered (a background task notification, Monitor, or ScheduleWakeup). If none is,
  say what is still running and how to check it.

### No silent stops
- You only run when the harness invokes you. A promise to wait, watch, or follow up with
  nothing registered to wake you is a stall by construction, not a plan. An end-of-turn
  hook rejects such promises.
- Every background wait has three parts: the deadline, the completion check (what event
  ends it and what result it returns), and the wake (which mechanism delivers that result
  to you). Register all three before moving on.
- Do not go idle with unfinished work. End a turn only when the task is complete with
  evidence, or you are blocked on input only I can give, named in one line. Anything else
  is a checkpoint, not a stop: state what remains and continue.
- An unfinished worker that goes idle gets one nudge, then escalation to me. Do not
  nudge it repeatedly or treat its silence as completion.

### Bounded loops
- The default is to stop: a turn ends when the task is complete with evidence, or blocked on
  input only I can give.
- The one sanctioned loop is `/goal`, and only when I ask for it and the task file has
  executable Done-when checks. The goal condition is the Done-when list verbatim plus a cap
  (`or stop after N turns`). Each turn shows the check commands and their output, because the
  goal evaluator reads the transcript and cannot run commands. No re-prompting loops or
  Stop-hook loops of your own making.

### Context budget
- The budget is 200k tokens of context; the status line shows it. Cost is turns multiplied
  by prefix, so a long session pays for its whole history on every turn.
- When the budget hook nudges, or when I say "checkpoint": write the checkpoint to the path
  the hook gives, in the shape it gives. Then report the context size and recommend /clear
  (fresh session; the checkpoint is re-injected at startup) at a task boundary, or /compact
  mid-task.
- A checkpoint holds the assignment, the verified state with its evidence, the single next
  step, open blockers, files touched, and background jobs. Nothing else.
- A manual /compact without a fresh checkpoint is blocked once with the reason. Auto
  compaction is never blocked. A mechanical snapshot is saved either way and re-injected
  after compaction.

### Done means evidence
- When the agreed checks pass, report concisely: what changed, the verification result,
  and any remaining blocker. "Done" requires evidence against the Done-when; an idle
  terminal is not evidence, and a green subset only when the Done-when names that subset.
- Avoid acknowledgement-only turns. At natural boundaries in long sessions, write a
  precise checkpoint (state, next step) so the session can be compacted or restarted.

## Compact instructions
When compacting, preserve verbatim: the task file path and its Done-when list with tick
state, the checkpoint path, each test or check command that ran with its last result, the
list of files touched, and every running background job with its deadline. Drop tool output
that has already been acted on.

## Lava Lamp

The team shares a coordination feed. Use it via the `lava_*` MCP tools, or the `lava` CLI.

- **At task start**, run `lava intent "<objective>" [repo-relative paths...]` from your
  worktree. This publishes your plan and checks other tasks, including the same human's agents.
  With MCP, call `lava_emit(kind: "claim", session, repo, branch, summary, paths)` then
  `lava_check(repo, session, paths)`. Reuse the coordination session ID from the startup hook;
  without hooks, choose a unique task ID and use it consistently.
- **Before entering a new area**, check the intended paths and update your intent if the scope
  changes. Pre-edit hooks also check supported edit tools, but cannot detect every shell edit.
- **On overlap**, read the other task's objective and branch. Continue independent work when
  the boundary is clear; tell the human about actual conflicts and ask before duplicating or
  depending on the other task's unfinished changes. A warning is advisory, not a lock or permission
  to modify another worktree, switch its branch, or cherry-pick its commits.
- **When you notice a real defect or pain point that's out of scope**, record it with
  `lava_emit(kind: "finding")`. One line. State what's wrong and where, not what you'd do about it.
- **When you finish a distinct piece of work**, `lava_emit(kind: "done")` with one line that
  quotes the task file's Outcome.
- **Keep findings open until verified on the integration branch.** A fix on your feature branch
  is not a landed fix; mention its branch in your completion note instead of closing prematurely.
- **Before picking up new work**, `lava_findings` is a good source of known problems.

Do not record: routine progress, things you already fixed this session, style opinions, speculation,
or anything you haven't actually observed. A findings list nobody trusts is worse than no list. If
you're unsure whether something is worth recording, it probably isn't.
