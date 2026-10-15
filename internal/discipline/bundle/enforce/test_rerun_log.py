#!/usr/bin/env python3
"""PreToolUse(Bash): log test runs, never block.

For every Bash command that looks like a test run, records whether the working
tree changed since the previous test run in this session (git fingerprint:
HEAD + diff + untracked files), whether the command is the same as last time,
and how many test runs the session has made. The log is the evidence for the
"run checks once" rule; enforcement comes later, if the measured rate earns it.

Env: CLAUDE_STATE_DIR (default ~/.claude/state).
"""
import hashlib
import json
import os
import re
import subprocess
import sys
import time

sys.path.insert(0, __file__.rsplit("/", 1)[0])
from common import STATE, log  # noqa: E402

TEST_CMD = re.compile(
    r"""(?:^|[\s;&|(]|bash\s+-c\s+['"])\s*(?:
        (?:python3?|py)\s+-m\s+(?:pytest|unittest)
      | pytest | py\.test | unittest
      | (?:npm|pnpm|yarn|bun)\s+(?:run\s+)?test(?:s|:\w+|-\w+)?\b
      | (?:npx|pnpx|bunx)\s+(?:vitest|jest|mocha|playwright\s+test|ava|tap|uvu)
      | vitest | jest | mocha
      | cargo\s+(?:test|nextest)
      | go\s+test
      | dotnet\s+test
      | mix\s+test
      | rspec | (?:bundle\s+exec\s+)?rake\s+test
      | make\s+(?:test|check)\b
      | node\s+--test
      | swift\s+test
      | ctest
      | tox\b
      | test_\w+\.py\b | \w+_test\.py\b
    )""", re.X)
# A command that only reads or filters the log is not a test run.
NOT_TEST = re.compile(r"^\s*(?:cat|grep|tail|head|less|ls|echo|sed|awk|rg)\b")


def sh(cwd, *args):
    try:
        r = subprocess.run(["git", "-C", cwd, "--no-optional-locks", *args],
                           capture_output=True, timeout=3)
        return r.stdout if r.returncode == 0 else None
    except (OSError, subprocess.SubprocessError):
        return None


def fingerprint(cwd):
    """Hash of HEAD, the tracked diff, and untracked files (name+mtime+size), or None if not git."""
    head = sh(cwd, "rev-parse", "HEAD")
    if head is None:
        return None
    h = hashlib.sha1(head)
    h.update(sh(cwd, "diff", "HEAD", "--no-color") or b"")
    untracked = sh(cwd, "ls-files", "--others", "--exclude-standard", "-z") or b""
    for name in untracked.split(b"\0")[:500]:
        if not name:
            continue
        p = os.path.join(cwd, name.decode(errors="replace"))
        try:
            st = os.stat(p)
            h.update(name + f":{st.st_mtime_ns}:{st.st_size}".encode())
        except OSError:
            h.update(name)
    return h.hexdigest()


def main():
    data = json.load(sys.stdin)
    cmd = ((data.get("tool_input") or {}).get("command") or "").strip()
    if not cmd or NOT_TEST.match(cmd) or not TEST_CMD.search(cmd):
        return
    sid = data.get("session_id") or "unknown"
    cwd = data.get("cwd") or os.getcwd()
    d = os.path.join(STATE, "testlog")
    os.makedirs(d, exist_ok=True)
    path = os.path.join(d, f"{sid}.json")
    prev = {}
    try:
        with open(path) as f:
            prev = json.load(f)
    except (OSError, ValueError):
        pass
    now = time.time()
    fp = fingerprint(cwd)
    norm = " ".join(cmd.split())
    first = not prev
    same_cmd = (not first) and prev.get("cmd") == norm
    if first:
        changed = "first"
    elif fp is None or prev.get("fp") is None:
        changed = "unknown"
    else:
        changed = "yes" if fp != prev.get("fp") else "no"
    count = int(prev.get("count") or 0) + 1
    with open(path, "w") as f:
        json.dump({"ts": now, "cmd": norm, "fp": fp, "count": count}, f)
    decision = {"first": "log_test_first", "yes": "log_test_after_change",
                "no": "log_test_rerun_no_change", "unknown": "log_test_rerun_unknown"}[changed]
    log("test_rerun", decision, data, command=norm[:200], same_cmd=same_cmd, changed=changed,
        since_last_s=(None if first else round(now - float(prev.get("ts") or now))),
        run_no=count, git=fp is not None)


if __name__ == "__main__":
    main()
