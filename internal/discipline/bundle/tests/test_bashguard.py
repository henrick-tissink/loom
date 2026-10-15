"""Synthetic + historical-replay tests for bash_guard.py. Usage: python3 -I test_bashguard.py"""
import json
import os
import subprocess
import sys

HERE = os.path.dirname(os.path.abspath(__file__))
ENV = {**os.environ, "CLAUDE_DECISIONS_LOG": "/dev/null"}
failed = 0


def run(guard, cmd, bg=False):
    r = subprocess.run([sys.executable, "-I", os.path.join(HERE, guard)], input=json.dumps(
        {"session_id": "t", "cwd": "/tmp", "hook_event_name": "PreToolUse", "tool_name": "Bash",
         "tool_input": {"command": cmd, "run_in_background": bg}}),
        capture_output=True, text=True, timeout=20, env=ENV)
    if r.returncode != 0:
        return f"error:{r.stderr[:100]}"
    return "deny" if '"deny"' in r.stdout else "allow"


def check(name, cond, detail=""):
    global failed
    failed += 0 if cond else 1
    print(("PASS" if cond else "FAIL"), "|", name, "" if cond else f"| {detail}")


N = "no" + "hup"
ALLOW = [
    (f'grep -rn "{N}" ~/.claude/hooks/', False),
    (f"echo 'never use {N} or disown'", False),
    ('git commit -m "remove setsid wrapper"', False),
    (f"cat > notes.md <<'EOF'\nUse {N} sparingly; setsid too.\nEOF", False),
    (f"ls # {N} would be bad here", False),
    ("echo 'a & b'", False),
    ("cmd 2>&1", False),
    ("make &&\n  echo ok", False),
    (f"python3 -c \"print('{N}')\"", False),
    (f'sed -i "s/{N} //" run.sh', False),
    (f"python3 - <<'PY'\nimport os\nprint('{N} is a word')\nPY", False),
    ("cd ~/proj && deadline 20m npm test", True),
    ("export PATH=/x:$PATH; deadline 15m npm run dev", True),
    ("D=/tmp/x && deadline 40m bash -c 'for i in 1 2; do echo $i; done'", True),
    ("(deadline 10m uv sync; echo $?)", True),
    ("cat > /tmp/w.sh <<'EOF'\n#!/bin/bash\nsleep 1\nEOF\ndeadline 5m bash /tmp/w.sh", True),
    ("sleep 20; deadline 25m gh pr checks 1 --watch", True),
    ("/usr/local/bin/deadline 5m ./x", True),
    ("if true; then deadline 5m ./x; fi", True),
    ("bash -c 'deadline 5m sleep 1'", True),
    ('ssh host "deadline 10m ./job"', True),
    ("cd x && bash -lc 'deadline 5m npm test'", True),
]
DENY = [
    (f"{N} ./server &", False),
    (f"bash -c '{N} ./server &'", False),
    (f'ssh host "{N} ./job > /dev/null 2>&1 &"', False),
    ("./server &", False),
    ("setsid ./daemon", False),
    (f"eval \"{N} x\"", False),
    (f"bash <<'EOF'\n{N} ./x &\nEOF", False),
    (f"sudo bash -c '{N} ./x'", False),
    (f"xargs -I{{}} sh -c '{N} {{}}'", False),
    (f"timeout 5 {N} ./x", False),
    ("npm run dev", True),
    ("echo hi", True),
    ("cd x && npm test", True),
    ("echo 'deadline 5m' ; npm test", True),
]
for cmd, bg in ALLOW:
    v = run("bash_guard.py", cmd, bg)
    check(f"allow: {cmd[:60]!r} bg={bg}", v == "allow", v)
for cmd, bg in DENY:
    v = run("bash_guard.py", cmd, bg)
    check(f"deny:  {cmd[:60]!r} bg={bg}", v == "deny", v)

print("ALL PASS" if not failed else f"{failed} FAILED")
sys.exit(1 if failed else 0)
