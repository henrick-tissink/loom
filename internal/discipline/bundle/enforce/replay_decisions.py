#!/usr/bin/env python3
"""Replay logged bash_guard denials through a (new) bash_guard and report what changed.

Usage: python3 -I replay_decisions.py [--days 30] [--guard /path/to/bash_guard.py] [--strict]
Reads decisions.jsonl (CLAUDE_DECISIONS_LOG or the default beside this file). For each
bash_guard denial that logged its command, re-runs the guard and prints the ones that would
now be allowed. Exit 0 unless --strict and at least one decision flipped.
"""
import json
import os
import subprocess
import sys
import time

HERE = os.path.dirname(os.path.abspath(__file__))
args = sys.argv[1:]


def opt(name, default):
    if name in args:
        i = args.index(name)
        return args[i + 1]
    return default


days = float(opt("--days", "30"))
guard = opt("--guard", os.path.join(HERE, "bash_guard.py"))
strict = "--strict" in args
_cfg = os.environ.get("CLAUDE_CONFIG_DIR") or os.path.expanduser("~/.claude")
_logdir = os.path.join(os.environ.get("CLAUDE_STATE_DIR") or os.path.join(_cfg, "state"), "log")
log_path = os.environ.get("CLAUDE_DECISIONS_LOG") or os.path.join(_logdir, "decisions.jsonl")
cutoff = time.time() - days * 86400
env = {**os.environ, "CLAUDE_DECISIONS_LOG": "/dev/null", "CLAUDE_ALLOWS_LOG": "/dev/null"}

entries = []
try:
    with open(log_path) as f:
        for line in f:
            try:
                e = json.loads(line)
            except ValueError:
                continue
            if e.get("hook") != "bash_guard" or "command" not in e:
                continue
            try:
                ts = time.mktime(time.strptime(e.get("ts", ""), "%Y-%m-%dT%H:%M:%S"))
            except ValueError:
                continue
            if ts >= cutoff:
                entries.append(e)
except OSError:
    print(f"no decisions log at {log_path}")
    sys.exit(0)

flips = 0
for e in entries:
    bg = e["decision"] == "deny_no_deadline"
    payload = {"session_id": "replay", "cwd": e.get("cwd") or "/tmp", "hook_event_name": "PreToolUse",
               "tool_name": "Bash", "tool_input": {"command": e["command"], "run_in_background": bg}}
    r = subprocess.run([sys.executable, "-I", guard], input=json.dumps(payload), capture_output=True,
                       text=True, timeout=20, env=env)
    now = "error" if r.returncode != 0 else ("deny" if '"deny"' in r.stdout else "allow")
    if now != "deny":
        flips += 1
        print(f"  {e['ts']} {e['decision']:18s} -> {now:5s} {e['command'][:90]!r}")
print(f"replay: {len(entries)} logged denials in the last {days:g} days; {flips} would now be allowed"
      + ("" if entries else " (nothing to replay)"))
sys.exit(1 if strict and flips else 0)
