#!/usr/bin/env python3
"""UserPromptSubmit guard: context budget.

Measures the context prefix from the transcript, using the same formula as
context-bar.sh (input + cache_read + cache_creation of the last non-sidechain
usage record). When it is over the budget, injects an instruction to write the
checkpoint and hand the session over. Nudges once per STEP tokens above the
budget, so it cannot nag on every prompt. Also injects the checkpoint path and
shape when the prompt itself asks for a checkpoint.

Finish line: on the first non-slash prompt of a session with no task file (or
with a closed one), injects the task-file path and shape once per task-file
version, so the model pins Outcome / Scope / Done-when as a durable object. The
checkpoint shape's Assignment is prefilled from the task file when it exists.

Env: CLAUDE_CONTEXT_BUDGET (default 200000), CLAUDE_CONTEXT_BUDGET_STEP (50000),
CLAUDE_STATE_DIR (default ~/.claude/state).
"""
import json
import os
import re
import sys
import time

sys.path.insert(0, __file__.rsplit("/", 1)[0])
from common import TASK_OPEN, TASK_SHAPE, load_task, log, log_allow  # noqa: E402

BUDGET = int(os.environ.get("CLAUDE_CONTEXT_BUDGET", "200000"))
STEP = int(os.environ.get("CLAUDE_CONTEXT_BUDGET_STEP", "50000"))
STATE = os.environ.get("CLAUDE_STATE_DIR") or os.path.expanduser("~/.claude/state")
TAIL_BYTES = 2_000_000
# Only a prompt that is itself a checkpoint request, not any sentence containing the word.
ASKS_CHECKPOINT = re.compile(r"^\s*(?:please\s+)?(?:write|save|take|make|do)?\s*(?:a|the)?\s*checkpoint\b", re.I)
# A bare acknowledgement is not the start of a task.
TRIVIAL_PROMPT = re.compile(r"^\s*(?:ok(?:ay)?|yes|no|thanks?|thank you|ty|k|sure|go|go ahead|continue|proceed|start|y|n)[.!]?\s*$", re.I)

CHECKPOINT_SHAPE = """# Checkpoint
Session: {sid}
Written: {ts}
Cwd: {cwd}

## Assignment
Outcome: {outcome}
Scope: {scope}
Done when:
{done_when}

## State
(what is done and verified, with the evidence; what is not done)

## Next step
(the single next action)

## Open blockers / questions

## Files touched

## Background jobs
(none, or each one with its status)
"""


def context_tokens(path):
    """Context size at the last assistant turn, read from the transcript tail."""
    try:
        size = os.path.getsize(path)
        with open(path, "rb") as f:
            if size > TAIL_BYTES:
                f.seek(size - TAIL_BYTES)
            chunk = f.read()
    except OSError:
        return 0
    lines = chunk.split(b"\n")
    if size > TAIL_BYTES:
        lines = lines[1:]  # first line may be partial
    for line in reversed(lines):
        if not line.strip():
            continue
        try:
            d = json.loads(line)
        except ValueError:
            continue
        if d.get("isSidechain") or d.get("isApiErrorMessage"):
            continue
        u = (d.get("message") or {}).get("usage")
        if not isinstance(u, dict):
            continue
        return (int(u.get("input_tokens") or 0) + int(u.get("cache_read_input_tokens") or 0)
                + int(u.get("cache_creation_input_tokens") or 0))
    return 0


def one_line(text):
    return " ".join(text.split()) if text and not text.lstrip().startswith("(") else ""


def bullets(text):
    """Keep Done-when checks one per line; drop the template placeholder."""
    lines = [l.rstrip() for l in (text or "").splitlines() if l.strip() and not l.lstrip().startswith("(")]
    return "\n".join(lines) if lines else "- [ ] "


def task_nudge(data, sid, cwd, prompt):
    """Inject the task-file shape once per task-file version when none is open."""
    path, task, mtime = load_task(sid)
    if task and task["status"] == TASK_OPEN:
        return None
    marker_dir = os.path.join(STATE, "ctxbudget")
    os.makedirs(marker_dir, exist_ok=True)
    marker = os.path.join(marker_dir, f"{sid}.taskfile-nudged")
    try:
        with open(marker) as f:
            if float(f.read().strip() or -1) >= mtime:
                return None
    except (OSError, ValueError):
        pass
    with open(marker, "w") as f:
        f.write(repr(mtime))
    os.makedirs(os.path.dirname(path), exist_ok=True)
    shape = TASK_SHAPE.format(sid=sid, ts=time.strftime("%Y-%m-%d %H:%M"), cwd=cwd)
    if task is None:
        log("ctx_budget", "nudge_task_file", data)
        intro = (f"FINISH LINE: no task file exists for this session. Per the finish-line rule, a "
                 f"delivery task that starts with this prompt has its task file at {path}, in the shape "
                 "below, written before the first edit: Outcome, Scope and named Done-when checks. A "
                 "question or consultation has Status: consultation and one line under Outcome. Helpers "
                 "receive the file verbatim as their assignment. The stop guard accepts a hand-back that "
                 "claims the work is finished only when every check is ticked and Status is done; a "
                 "check written as `name: $ command` is executed by the stop guard and holds when the "
                 "command exits 0.")
    else:
        log("ctx_budget", "nudge_task_file_closed", data, status=task["status"])
        intro = (f"FINISH LINE: the task file {path} is closed (Status: {task['status']}). A new "
                 "delivery task that starts with this prompt replaces it with the shape below "
                 "(Status: open); a question or consultation leaves it closed.")
    return intro + "\n\n" + shape


def main():
    data = json.load(sys.stdin)
    prompt = data.get("prompt") or ""
    sid = data.get("session_id") or "unknown"
    cwd = data.get("cwd") or ""
    ckpt_dir = os.path.join(STATE, "checkpoints")
    os.makedirs(ckpt_dir, exist_ok=True)
    ckpt = os.path.join(ckpt_dir, f"{sid}.md")
    tokens = context_tokens(data.get("transcript_path") or "")
    slash = prompt.lstrip().startswith("/")

    explicit = bool(ASKS_CHECKPOINT.search(prompt)) and not slash
    nudge = False
    if tokens >= BUDGET and not slash:
        marker_dir = os.path.join(STATE, "ctxbudget")
        os.makedirs(marker_dir, exist_ok=True)
        marker = os.path.join(marker_dir, sid)
        last = 0
        try:
            with open(marker) as f:
                last = int(f.read().strip() or 0)
        except (OSError, ValueError):
            pass
        level = BUDGET + ((tokens - BUDGET) // STEP) * STEP
        if level > last:
            nudge = True
            with open(marker, "w") as f:
                f.write(str(level))

    task_part = None
    if not slash and not TRIVIAL_PROMPT.match(prompt):
        task_part = task_nudge(data, sid, cwd, prompt)

    if not (nudge or explicit or task_part):
        log_allow("ctx_budget", data, tokens=tokens)
        return

    parts = []
    if nudge:
        parts.append(
            f"CONTEXT BUDGET: this session's context is about {tokens:,} tokens, over the budget "
            f"of {BUDGET:,}; every turn re-reads that whole prefix. Per the context-budget rule, the "
            f"checkpoint for this session is {ckpt}, in the shape below, and it is written before the "
            "task continues. The reply then states the context size, that the checkpoint is written, "
            "and recommends /clear at a task boundary (the checkpoint is re-injected at startup) or "
            "/compact mid-task; the prompt itself is answered only if it needs an answer now.")
        log("ctx_budget", "nudge_context_budget", data, tokens=tokens, budget=BUDGET)
    elif explicit:
        parts.append(
            f"CHECKPOINT: the user asked for a checkpoint. Its path is {ckpt} and its shape is below; "
            f"the reply confirms the path and the context size (about {tokens:,} tokens).")
        log("ctx_budget", "checkpoint_requested", data, tokens=tokens)
    if nudge or explicit:
        _, task, _ = load_task(sid)
        sec = (task or {}).get("sections") or {}
        parts.append(CHECKPOINT_SHAPE.format(
            sid=sid, ts=time.strftime("%Y-%m-%d %H:%M"), cwd=cwd,
            outcome=one_line(sec.get("outcome", "")), scope=one_line(sec.get("scope", "")),
            done_when=bullets(sec.get("done when", ""))))
    if task_part:
        parts.append(task_part)
    print(json.dumps({"hookSpecificOutput": {"hookEventName": "UserPromptSubmit",
                                             "additionalContext": "\n\n".join(parts)}}))


if __name__ == "__main__":
    main()
