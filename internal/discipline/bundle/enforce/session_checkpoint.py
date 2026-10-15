#!/usr/bin/env python3
"""SessionStart: re-inject the checkpoint.

After compaction or a resume, injects this session's model-written checkpoint
and the mechanical snapshot.

After /clear, injects the checkpoint of the session the /clear replaced. That
session is known exactly: /clear keeps the same `claude` process, and every
SessionStart records which session that process is running (state/by-pid/).
The working directory is no key for this: it drifts with every cd and worktree,
and a checkpoint written from a worktree never matched the directory the
session was launched in.

At startup, or after a /clear whose predecessor is unknown, matches checkpoints
from the last 24 hours by launch directory (the transcript's project dir) or a
Cwd at or under this one. One match is injected; several are listed for the
user to choose from, never picked between. The matching task file comes with
it (this session's, the predecessor's, or the newest open one for this
directory).

Env: CLAUDE_STATE_DIR (default ~/.claude/state); CLAUDE_PROCESS_PID overrides
the claude process lookup (tests).
"""
import glob
import json
import os
import subprocess
import sys
import time

sys.path.insert(0, __file__.rsplit("/", 1)[0])
from common import TASK_OPEN, load_task, log, parse_task  # noqa: E402

STATE = os.environ.get("CLAUDE_STATE_DIR") or os.path.expanduser("~/.claude/state")
MAX_CHARS = 8000
MAX_AGE = 24 * 3600
NAME_IT = ("Before acting on it, tell the user in one line which checkpoint you are resuming "
           "(its session id and age).")


def read(path):
    try:
        with open(path) as f:
            return f.read().strip()
    except OSError:
        return ""


def header_cwd(text):
    for line in text.splitlines()[:8]:
        if line.startswith("Cwd:"):
            return line[4:].strip()
    return ""


def age_text(seconds):
    return f"{int(seconds // 60)} min" if seconds < 3600 else f"{seconds / 3600:.1f} h"


def sid_of(path):
    # <sid>.md, <sid>.mechanical.md, and task files named <sid>.<label>.md
    return os.path.basename(path).split(".", 1)[0]


def claude_pid():
    """PID of the `claude` process this hook runs under: it survives /clear, the session id does not."""
    forced = os.environ.get("CLAUDE_PROCESS_PID")
    if forced is not None:
        return forced
    pid = os.getppid()
    for _ in range(8):
        if pid <= 1:
            return ""
        try:
            out = subprocess.run(["ps", "-o", "ppid=,comm=", "-p", str(pid)], capture_output=True,
                                 text=True, timeout=2).stdout.strip()
        except (OSError, subprocess.SubprocessError):
            return ""
        ppid, _, comm = out.partition(" ")
        if os.path.basename(comm.strip()) == "claude":
            return str(pid)
        try:
            pid = int(ppid)
        except ValueError:
            return ""
    return ""


def swap_process_session(pid, sid):
    """Record that `pid` now runs `sid`; return the session it ran before, if recent."""
    if not pid:
        return ""
    d = os.path.join(STATE, "by-pid")
    path = os.path.join(d, pid)
    previous = ""
    try:
        with open(path) as f:
            rec = json.load(f)
        if rec.get("sid") != sid and time.time() - float(rec.get("ts", 0)) <= MAX_AGE:
            previous = rec.get("sid") or ""
    except (OSError, ValueError, TypeError, AttributeError):
        pass
    try:
        os.makedirs(d, exist_ok=True)
        with open(path, "w") as f:
            json.dump({"sid": sid, "ts": time.time()}, f)
    except OSError:
        pass
    return previous


def in_dir(path, cwd):
    return bool(path and cwd) and (path == cwd or path.startswith(cwd.rstrip("/") + "/"))


def matches_here(sid, text_cwd, cwd, project_dir):
    """Launched in this project (its transcript sits beside ours), or written at or under this cwd."""
    if project_dir and os.path.exists(os.path.join(project_dir, f"{sid}.jsonl")):
        return True
    return in_dir(text_cwd, cwd)


def summary_line(text):
    """First content line of the Assignment (or Outcome) section, for the list."""
    lines = text.splitlines()
    for i, line in enumerate(lines):
        if line.strip().lower() in ("## assignment", "## outcome"):
            for nxt in lines[i + 1:]:
                if nxt.strip():
                    return nxt.strip()[:200]
    for line in lines:
        if line.strip().lower().startswith("outcome:"):
            return line.strip()[:200]
    return ""


def checkpoint_for(sid, now):
    """(age, path, text) of a session's checkpoint, model-written before mechanical, or None."""
    for name in (f"{sid}.md", f"{sid}.mechanical.md"):
        path = os.path.join(STATE, "checkpoints", name)
        try:
            age = now - os.path.getmtime(path)
        except OSError:
            continue
        text = read(path)
        if text and age <= MAX_AGE:
            return age, path, text
    return None


def open_task_of(sid, now):
    """(age, path) of the newest open task file belonging to `sid`, or None."""
    best = None
    for path in glob.glob(os.path.join(STATE, "tasks", f"{sid}*.md")):
        if sid_of(path) != sid:
            continue
        try:
            age = now - os.path.getmtime(path)
        except OSError:
            continue
        task = parse_task(read(path))
        if age <= MAX_AGE and task and task["status"] == TASK_OPEN and (best is None or age < best[0]):
            best = (age, path)
    return best


def task_block(age, path, why):
    return (f"## Open task file from {why} ({age_text(age)} ago)\n"
            f"Path: {path}\nIf the user continues this work, copy it to this session's task "
            "file path (the next prompt hook gives it) and carry on; otherwise ignore it.\n" + read(path))


def main():
    data = json.load(sys.stdin)
    sid = data.get("session_id") or "unknown"
    cwd = data.get("cwd") or ""
    source = data.get("source") or "startup"
    transcript = data.get("transcript_path") or ""
    project_dir = os.path.dirname(transcript) if transcript else ""
    ckpt_dir = os.path.join(STATE, "checkpoints")
    parts = []
    found = ""
    mode = ""
    now = time.time()

    # A subagent shares the claude process; it must not displace the session in the record.
    previous = "" if data.get("agent_type") else swap_process_session(claude_pid(), sid)
    task_text = ""
    note = ""

    if source in ("compact", "resume"):
        mode = "own"
        task_path_, task, _ = load_task(sid)
        if task:
            task_text = f"## Task file for this session ({task_path_})\n{read(task_path_)}"
        for name, label in ((f"{sid}.md", "Checkpoint written before this compaction or handover"),
                            (f"{sid}.mechanical.md", "Mechanical snapshot taken at compaction")):
            text = read(os.path.join(ckpt_dir, name))
            if text:
                parts.append(f"## {label}\n{text}")
                found = found or name

    if not parts and source == "clear" and previous:
        hit = checkpoint_for(previous, now)
        if hit:
            age, path, text = hit
            parts.append(
                f"## Checkpoint from the session this /clear replaced ({previous}, {age_text(age)} ago)\n"
                "Use it only if the user continues that work; otherwise ignore it. " + NAME_IT + "\n" + text)
            found = os.path.basename(path)
            mode = "lineage"
        # The replaced session's open task file is its finish line, checkpoint or not.
        best = open_task_of(previous, now)
        if best:
            task_text = task_block(best[0], best[1], "a previous session (the one this /clear replaced)")
        if not hit:
            # Anything matched below is OLDER than the session the user just left.
            note = (f"The session this /clear replaced ({previous}) left no checkpoint; what follows "
                    "comes from an earlier session and may not be what the user means.\n")

    chosen_sid = ""
    if not parts:
        by_sid = {}
        for path in glob.glob(os.path.join(ckpt_dir, "*.md")):
            csid = sid_of(path)
            if csid == sid or csid in by_sid:
                continue
            hit = checkpoint_for(csid, now)
            if hit and matches_here(csid, header_cwd(hit[2]), cwd, project_dir):
                by_sid[csid] = hit
        candidates = sorted(by_sid.items(), key=lambda kv: kv[1][0])  # newest first
        if len(candidates) == 1:
            csid, (age, path, text) = candidates[0]
            parts.append(
                f"## Checkpoint from a previous session in this directory ({age_text(age)} ago)\n" + note +
                "Use it only if the user continues that work; otherwise ignore it. " + NAME_IT + "\n" + text)
            found = os.path.basename(path)
            chosen_sid = csid
            mode = "single"
        elif candidates:
            rows = []
            for csid, (age, path, text) in candidates:
                task = open_task_of(csid, now)
                rows.append(f"- {csid} ({age_text(age)} ago) Cwd: {header_cwd(text) or '?'}\n"
                            f"  {summary_line(text) or '(no assignment line)'}\n"
                            f"  Checkpoint: {path}" + (f"\n  Open task file: {task[1]}" if task else ""))
            parts.append(
                "## Several recent checkpoints match this directory\n" + note +
                "Which work the user means is not known. Resume none of them until the user names one: "
                "if the first message is just \"continue\", show this list and ask. Then read the "
                "chosen checkpoint (and its task file) before acting.\n" + "\n".join(rows))
            found = ",".join(c[0] for c in candidates)
            mode = "list"

    if not task_text and mode == "single":
        best = open_task_of(chosen_sid, now)
        if best:
            task_text = task_block(best[0], best[1], "a previous session (the same one as the checkpoint)")

    if not task_text and not mode:
        # No checkpoint at all: the newest open task file for this directory, the finish
        # line of the work the user is most likely continuing after /clear.
        best = None
        for path in glob.glob(os.path.join(STATE, "tasks", "*.md")):
            tsid = sid_of(path)
            if tsid == sid:
                continue
            try:
                age = now - os.path.getmtime(path)
            except OSError:
                continue
            if age > MAX_AGE:
                continue
            task = parse_task(read(path))
            if (task and task["status"] == TASK_OPEN and matches_here(tsid, task["cwd"], cwd, project_dir)
                    and (best is None or age < best[0])):
                best = (age, path)
        if best:
            task_text = task_block(best[0], best[1], "a previous session in this directory")
    if task_text:
        parts.insert(0, task_text)

    if not parts:
        return
    text = "\n\n".join(parts)
    if len(text) > MAX_CHARS:
        text = text[:MAX_CHARS] + "\n... (truncated)"
    log("session_checkpoint", "inject_checkpoint", data, source=source, file=found, chars=len(text),
        mode=mode or "task", previous=previous)
    print(json.dumps({"hookSpecificOutput": {"hookEventName": "SessionStart",
                                             "additionalContext": text}}))


if __name__ == "__main__":
    main()
