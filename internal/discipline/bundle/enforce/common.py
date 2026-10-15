"""Shared helpers for the enforcement hooks."""
import json
import os
import re
import time

CONFIG_DIR = os.environ.get("CLAUDE_CONFIG_DIR") or os.path.expanduser("~/.claude")
STATE = os.environ.get("CLAUDE_STATE_DIR") or os.path.join(CONFIG_DIR, "state")
# Logs live under the state dir, not beside the hooks: the hook dir is an immutable,
# versioned install that gets swapped on upgrade, the logs must outlive it.
LOG_PATH = os.environ.get("CLAUDE_DECISIONS_LOG") or os.path.join(STATE, "log", "decisions.jsonl")
ALLOW_PATH = os.environ.get("CLAUDE_ALLOWS_LOG") or os.path.join(STATE, "log", "allows.jsonl")


def _rev():
    """The bundle revision this hook dir came from (REV file one level up), else "local"."""
    try:
        with open(os.path.join(os.path.dirname(os.path.abspath(__file__)), "..", "REV")) as f:
            return f.read().strip() or "local"
    except OSError:
        return "local"


REV = _rev()


def _append(path, record):
    try:
        d = os.path.dirname(path)
        if d and d != "/dev":
            os.makedirs(d, exist_ok=True)
        with open(path, "a") as f:
            f.write(json.dumps(record) + "\n")
    except OSError:
        pass

# The finish line as a durable object: one file per session, written by the model
# when a delivery task starts, read by the stop guard, the checkpoint hooks, and
# (verbatim) by helpers as their assignment.
TASK_SHAPE = """# Task
Session: {sid}
Started: {ts}
Cwd: {cwd}
Status: open

## Outcome
(the behaviour or deliverable, one sentence a reader could test)

## Scope
(what this covers; what it explicitly leaves out)

## Done when
- [ ] <check name>: $ <command that exits 0 when the check holds>  # <evidence, once ticked>
- [ ] <check name>: <the observation that proves it, when no command can>

## Notes
(assumptions taken, decisions made, out-of-scope findings)
"""
TASK_OPEN, TASK_DONE, TASK_CONSULTATION = "open", "done", "consultation"
_CHECK = re.compile(r"^\s*[-*]\s*\[([ xX])\]\s*(.*?)\s*$")
# `name: $ command` marks an executable check: the stop guard runs the command (shell, in the
# task's Cwd) before it accepts Status: done. Exit 0 means the check holds.
_CHECK_CMD = re.compile(r"^(?P<name>[^:$]+?)\s*:\s*\$\s+(?P<cmd>\S.*)$")
CHECK_TIMEOUT = int(os.environ.get("CLAUDE_CHECK_TIMEOUT", "120"))
CHECKS_TIMEOUT = int(os.environ.get("CLAUDE_CHECKS_TIMEOUT", "240"))


def log(hook, decision, data, **extra):
    """Append one line per intervention, so we can measure how often each rule fires."""
    record = {
        "ts": time.strftime("%Y-%m-%dT%H:%M:%S"),
        "hook": hook,
        "decision": decision,
        "event": data.get("hook_event_name"),
        "session": data.get("session_id"),
        "agent_type": data.get("agent_type"),
        "cwd": data.get("cwd"),
        "rev": REV,
        **extra,
    }
    _append(LOG_PATH, record)


def log_allow(hook, data, **extra):
    """One compact line per silent pass, so interventions have a denominator."""
    record = {"ts": time.strftime("%Y-%m-%dT%H:%M:%S"), "hook": hook,
              "event": data.get("hook_event_name"), "session": data.get("session_id"),
              "rev": REV, **extra}
    _append(ALLOW_PATH, record)


def task_path(sid):
    return os.path.join(STATE, "tasks", f"{sid}.md")


def read_text(path):
    try:
        with open(path) as f:
            return f.read()
    except OSError:
        return ""


def parse_task(text):
    """Return {"status", "cwd", "sections": {name: body}, "checks": [(ticked, text)]} or None."""
    if not text.strip():
        return None
    head = {}
    for line in text.splitlines()[:12]:
        m = re.match(r"^(Session|Started|Cwd|Status):\s*(.*)$", line)
        if m:
            head[m.group(1).lower()] = m.group(2).strip()
    sections, name, buf = {}, None, []
    for line in text.splitlines():
        m = re.match(r"^##\s+(.+?)\s*$", line)
        if m:
            if name is not None:
                sections[name] = "\n".join(buf).strip()
            name, buf = m.group(1).strip().lower(), []
        elif name is not None:
            buf.append(line)
    if name is not None:
        sections[name] = "\n".join(buf).strip()
    checks = []
    for line in sections.get("done when", "").splitlines():
        m = _CHECK.match(line)
        if m and not m.group(2).startswith("<"):  # skip the template placeholders
            checks.append((m.group(1).strip().lower() == "x", m.group(2)))
    commands = []
    for _, text in checks:
        m = _CHECK_CMD.match(text)
        if m and not m.group("cmd").startswith("<"):
            commands.append((m.group("name").strip(), m.group("cmd").strip()))
    status = (head.get("status") or "").lower()
    if status not in (TASK_OPEN, TASK_DONE, TASK_CONSULTATION):
        status = TASK_OPEN if sections else ""
    return {"status": status, "cwd": head.get("cwd", ""), "sections": sections, "checks": checks,
            "commands": commands}


def run_checks(task, cwd=None):
    """Run the task's executable checks; return [(name, cmd, rc, output_tail)] for the failures.
    rc 124 = timed out; rc 125 = skipped because the overall time budget ran out."""
    import signal
    import subprocess
    workdir = task.get("cwd") or cwd or None
    if workdir and not os.path.isdir(workdir):
        workdir = cwd if cwd and os.path.isdir(cwd) else None
    failures, deadline = [], time.time() + CHECKS_TIMEOUT
    for name, cmd in task.get("commands") or []:
        left = deadline - time.time()
        if left <= 0:
            failures.append((name, cmd, 125, "skipped: the checks' overall time budget ran out"))
            continue
        budget = min(CHECK_TIMEOUT, left)
        try:
            proc = subprocess.Popen(cmd, shell=True, cwd=workdir, stdout=subprocess.PIPE,
                                    stderr=subprocess.STDOUT, stdin=subprocess.DEVNULL, text=True,
                                    start_new_session=True)
        except OSError as e:
            failures.append((name, cmd, 127, str(e)))
            continue
        try:
            out, _ = proc.communicate(timeout=budget)
            rc = proc.returncode
        except subprocess.TimeoutExpired:
            # Kill the whole process group, not just the shell: a pipeline or `a && b` chain
            # would otherwise keep running after the hook returns.
            try:
                os.killpg(proc.pid, signal.SIGKILL)
            except OSError:
                pass
            try:
                out, _ = proc.communicate(timeout=5)
            except (subprocess.TimeoutExpired, OSError):
                out = ""
            rc, out = 124, f"timed out after {int(budget)}s\n" + (out or "")
        if rc != 0:
            failures.append((name, cmd, rc, out.strip()[-600:]))
    return failures


def load_task(sid):
    """(path, parsed task or None, mtime or 0) for this session's task file."""
    path = task_path(sid)
    try:
        mtime = os.path.getmtime(path)
    except OSError:
        return path, None, 0
    return path, parse_task(read_text(path)), mtime
