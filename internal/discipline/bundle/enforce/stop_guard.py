#!/usr/bin/env python3
"""Stop / SubagentStop guard.

1. Unbacked promise: the final message promises to watch / wait / report back,
   but no background task or scheduled wakeup exists to make that happen.
2. Unaccounted background work: tasks are still in flight and the final message
   doesn't mention them.

On SubagentStop the registry we are handed is the parent session's. A helper can
only account for tasks it started itself, so we filter to tasks that name this
agent as owner. If no task carries an owner field at all, check 2 is skipped for
helpers (and logged, with the task keys, so the payload shape gets recorded).

3. Finish line (Stop only): the final message claims the work is done, but the
   session's task file (~/.claude/state/tasks/<sid>.md) still says Status: open,
   or says done with Done-when checks left unticked. Nudged once per task-file
   version: a marker records the mtime that was nudged, so rewriting the file
   for a new task re-arms it.

Each check nudges once: when stop_hook_active is set (we already blocked this
turn) the stop is allowed, so the hook can never loop.
"""
import json
import os
import re
import sys

sys.path.insert(0, __file__.rsplit("/", 1)[0])
from common import (run_checks, TASK_CONSULTATION, TASK_DONE, TASK_OPEN,  # noqa: E402
                    load_task, log, log_allow)

I_WILL = r"(?:I(?:'|’)?ll|I\s+will|I\s+am\s+going\s+to|I'm\s+going\s+to)"
PROMISE = re.compile(
    rf"""\b{I_WILL}\s+(?:
          keep\s+(?:an\s+eye|watching|monitoring|checking)
        | monitor | watch | poll
        | check\s+(?:back|in|on\s+(?:it|this|that|them|the))
        | report\s+back | follow\s+up | circle\s+back
        | get\s+back\s+to\s+you
        | let\s+you\s+know\s+(?:when|once|as\s+soon|if\s+it)
        | (?:update|ping|notify)\s+you\s+(?:when|once|as\s+soon)
        | wait\s+for
    )\b
    | \bwill\s+report\s+back\b
    """,
    re.IGNORECASE | re.VERBOSE,
)
# Quoted or code-formatted text is a mention, not a promise ("the rule catches 'I'll report back'").
QUOTED = re.compile(r'"[^"\n]*"|“[^”\n]*”|`[^`\n]*`')
MENTIONS_BACKGROUND = re.compile(r"\b(background|still\s+running|in\s+flight|running)\b", re.I)

# A hand-back that claims the work is finished. Calibrated on real final messages:
# "Done.", "All deployed and verified.", "It's live.", "Nothing left open on my side."
DONE_WORDS = r"(?:done|complete|completed|finished|shipped|landed|live|merged|installed|in\s+place)"
CLAIMS_DONE = re.compile(
    rf"""^\W*(?:all\s+)?(?:done|complete|finished|shipped|landed|deployed|installed|merged|verified)\b
      | \b(?:it(?:'|’)?s|everything|all\s+of\s+it|that(?:'|’)?s|this\s+is
            |the\s+(?:task|work|feature|fix|change|implementation|migration|deploy)\s+is)
        \s+(?:now\s+)?(?:all\s+)?{DONE_WORDS}\b
      | \b(?:all|everything)\s+(?:is\s+)?(?:built|deployed|merged|verified|installed|shipped)\b
      | \b(?:all|every|both)\b[^.\n]{{0,40}}?\b(?:are|is)\s+(?:now\s+)?(?:built|deployed|merged|verified|installed|shipped|{DONE_WORDS})\b
      | \bnothing\s+(?:left|else|more)\s+(?:open|to\s+do|outstanding|pending)\b
      | \bverified\s+(?:live|on\s+production|end[\s-]to[\s-]end)\b
      | \bdone[\s-]when\b[^.\n]{{0,60}}\b(?:met|satisfied|pass(?:es|ed)?)\b
    """, re.IGNORECASE | re.VERBOSE)
# "not done", "isn't finished", "once it's live" are not claims.
NEGATED_DONE = re.compile(
    rf"\b(?:not|isn(?:'|’)?t|aren(?:'|’)?t|wasn(?:'|’)?t|never|until|till|once|before|when|unless|if"
    rf"|after|the\s+moment|as\s+soon\s+as)"
    rf"\s+(?:yet\s+|it(?:'|’)?s\s+|everything\s+|all\s+|that(?:'|’)?s\s+)?{DONE_WORDS}\b"
    # "Done reading", "finished looking": an activity ended, not the task.
    rf"|\b(?:done|finished)\s+(?:reading|looking|checking|investigating|reviewing|digging|scanning|searching|exploring)\b", re.I)

# Fields a task record might use to name the agent that started it.
OWNER_KEYS = ("agent_id", "agentId", "owner", "owner_id", "parent_id", "parentId",
              "spawned_by", "spawnedBy", "session_id", "sessionId")

# A task in one of these states has nothing left to account for. Anything else
# (running, pending, in_progress, unknown, or no status at all) still counts.
SETTLED = {"idle", "available", "done", "complete", "completed", "finished", "exited",
           "stopped", "killed", "cancelled", "canceled", "failed", "error", "errored"}


def block(reason):
    print(json.dumps({"decision": "block", "reason": reason}))


def status_of(task):
    return str(task.get("status") or "").strip().lower()


def live_tasks(tasks):
    """Drop tasks that have already settled; keep the rest."""
    return [t for t in tasks if isinstance(t, dict) and status_of(t) not in SETTLED]


def status_counts(tasks):
    counts = {}
    for t in tasks:
        if isinstance(t, dict):
            s = status_of(t) or "(none)"
            counts[s] = counts.get(s, 0) + 1
    return counts


def own_tasks(tasks, data):
    """Return (tasks this agent is accountable for, whether ownership was decidable)."""
    if data.get("hook_event_name") != "SubagentStop":
        return tasks, True
    me = {str(v) for v in (data.get("agent_id"), data.get("session_id")) if v}
    owned, decidable = [], False
    for t in tasks:
        if not isinstance(t, dict):
            continue
        owners = {str(t[k]) for k in OWNER_KEYS if t.get(k)}
        if owners:
            decidable = True
            if owners & me:
                owned.append(t)
    return owned, decidable


def claims_done(message):
    head = QUOTED.sub(" ", message)[:600]
    head = NEGATED_DONE.sub(" ", head)
    return CLAIMS_DONE.search(head)


def verify_commands(data, path, task, mtime):
    """Status: done with every check ticked. Run the `$ command` checks once per task-file
    version; a failure blocks once (same nudge-once marker), a pass is remembered."""
    if not task.get("commands"):
        return None
    verified = path + ".verified"
    try:
        with open(verified) as f:
            if float(f.read().strip() or 0) >= mtime:
                return None
    except (OSError, ValueError):
        pass
    marker = path + ".nudged"
    try:
        with open(marker) as f:
            if float(f.read().strip() or 0) >= mtime:
                # Already blocked once for this file version: do not re-run the commands.
                log("stop_guard", "allow_done_checks_failed_already_nudged", data)
                return None
    except (OSError, ValueError):
        pass
    failures = run_checks(task, data.get("cwd"))
    if not failures:
        try:
            with open(verified, "w") as f:
                f.write(repr(mtime))
        except OSError:
            pass
        log("stop_guard", "allow_done_checks_passed", data, commands=len(task["commands"]))
        return None
    try:
        with open(marker, "w") as f:
            f.write(repr(mtime))
    except OSError:
        pass
    log("stop_guard", "block_done_check_failed", data, failed=[n for n, *_ in failures],
        commands=len(task["commands"]))
    lines = []
    for name, cmd, rc, out in failures:
        lines.append(f"- {name}: `{cmd}` exited {rc}" + (f"\n  {out}" if out else ""))
    return (f"The task file {path} says Status: done, but these executable Done-when checks "
            "fail when the stop guard runs them:\n" + "\n".join(lines) +
            "\nA finished hand-back needs each command to exit 0. Fix the work and run the "
            "command again, or set Status: open and say in the hand-back what is not done.")


def finish_line(data, message):
    """Check 3. Returns a block reason, or None."""
    if data.get("hook_event_name") != "Stop":
        return None
    sid = data.get("session_id") or ""
    path, task, mtime = load_task(sid)
    claim = claims_done(message)
    if task is None:
        if claim:
            log("stop_guard", "allow_done_no_task_file", data, phrase=claim.group(0))
        return None
    if task["status"] == TASK_CONSULTATION:
        return None
    unticked = [t for ticked, t in task["checks"] if not ticked]
    if task["status"] == TASK_DONE and not unticked:
        return verify_commands(data, path, task, mtime)
    if task["status"] == TASK_OPEN and not claim:
        return None
    # Something to nudge. Once per task-file version.
    marker = path + ".nudged"
    try:
        with open(marker) as f:
            if float(f.read().strip() or 0) >= mtime:
                log("stop_guard", "allow_finish_line_already_nudged", data, status=task["status"])
                return None
    except (OSError, ValueError):
        pass
    try:
        with open(marker, "w") as f:
            f.write(repr(mtime))
    except OSError:
        pass
    if task["status"] == TASK_DONE:
        log("stop_guard", "block_done_unticked_checks", data, unticked=len(unticked))
        return (f"The task file {path} says Status: done, but these Done-when checks are not "
                "ticked:\n" + "\n".join(f"- [ ] {t}" for t in unticked) +
                "\nTick each with its evidence (what ran, what it showed), or if a check was "
                "not met, set Status: open and say so in the hand-back.")
    checks = "\n".join(f"- [{'x' if ok else ' '}] {t}" for ok, t in task["checks"]) or "(none written)"
    log("stop_guard", "block_done_claim_task_open", data, phrase=claim.group(0),
        checks=len(task["checks"]), unticked=len(unticked))
    return (f'Your hand-back claims the work is finished ("{claim.group(0)}"), but the task file '
            f"{path} still says Status: open. Done-when checks:\n{checks}\n"
            "Before handing back: tick each check that passed with its evidence, set Status: done, "
            "and make the hand-back cite the same checks. If some check is not met, leave Status: open "
            "and say plainly what is not done.")


def main():
    data = json.load(sys.stdin)
    if data.get("stop_hook_active"):
        return
    message = data.get("last_assistant_message") or ""
    registry_known = "background_tasks" in data
    registry = data.get("background_tasks") or []
    statuses = status_counts(registry)
    all_tasks = live_tasks(registry)
    crons = data.get("session_crons") or []
    tasks, decidable = own_tasks(all_tasks, data)

    if len(all_tasks) < len(registry):
        log("stop_guard", "ignore_settled_tasks", data,
            registry=len(registry), live=len(all_tasks), statuses=statuses)

    if all_tasks and not decidable:
        # Parent's registry with no ownership info: a helper cannot account for these.
        keys = sorted({k for t in all_tasks if isinstance(t, dict) for k in t})
        log("stop_guard", "allow_subagent_unowned_tasks", data,
            count=len(all_tasks), task_keys=keys, statuses=statuses)

    # With no ownership info, the full registry decides whether a promise is backed
    # (original behaviour); only the unaccounted-tasks check below is skipped.
    promise_tasks = tasks if decidable else all_tasks
    promise = PROMISE.search(QUOTED.sub(" ", message))
    if promise and registry_known and not promise_tasks and not crons:
        log("stop_guard", "block_unbacked_promise", data, phrase=promise.group(0))
        block(f'Your final message promises "{promise.group(0)}", but nothing is registered '
              "to wake you: there are no background tasks and no scheduled wakeups. Either "
              "register a real completion mechanism (run_in_background, Monitor, or "
              "ScheduleWakeup) or rewrite the hand-back to say plainly what is still "
              "pending and how the user can check it. Do not promise to come back.")
        return

    if tasks and not MENTIONS_BACKGROUND.search(message) and not any(
            str(t.get("id", "")) and str(t.get("id")) in message for t in tasks):
        listing = "\n".join(
            f"- [{t.get('type', '?')}] {t.get('description') or t.get('command') or t.get('id')}"
            for t in tasks)
        log("stop_guard", "block_unaccounted_tasks", data, count=len(tasks), statuses=statuses)
        block("Background work is still in flight but your hand-back doesn't account for it:\n"
              f"{listing}\nFor each one, either stop it, or tell the user it is still running, "
              "what it is for, and that you'll be woken when it completes.")
        return

    reason = finish_line(data, message)
    if reason:
        block(reason)
        return
    log_allow("stop_guard", data, tasks=len(tasks), claim=bool(claims_done(message)))


if __name__ == "__main__":
    main()
