"""Executable Done-when checks (`name: $ command`) run by stop_guard. Usage: python3 -I test_execchecks.py"""
import json
import os
import subprocess
import sys
import tempfile
import time

HERE = os.path.dirname(os.path.abspath(__file__))
STATE = tempfile.mkdtemp(prefix="ecstate-")
WORK = tempfile.mkdtemp(prefix="ecwork-")
ENV = {**os.environ, "CLAUDE_STATE_DIR": STATE, "CLAUDE_DECISIONS_LOG": os.path.join(STATE, "d.jsonl"),
       "CLAUDE_ALLOWS_LOG": "/dev/null", "CLAUDE_CHECK_TIMEOUT": "3", "CLAUDE_CHECKS_TIMEOUT": "5"}
os.makedirs(os.path.join(STATE, "tasks"))
failed = 0


def check(name, cond, detail=""):
    global failed
    failed += 0 if cond else 1
    print(("PASS" if cond else "FAIL"), "|", name, "" if cond else f"| {detail}")


def stop(sid, message="Done.", event="Stop"):
    r = subprocess.run([sys.executable, "-I", os.path.join(HERE, "stop_guard.py")], input=json.dumps(
        {"session_id": sid, "cwd": WORK, "hook_event_name": event, "last_assistant_message": message,
         "background_tasks": []}), capture_output=True, text=True, timeout=60, env=ENV)
    out = json.loads(r.stdout) if r.stdout.strip() else {}
    return r.returncode, out, r.stderr


def task(sid, checks, status="done", cwd=WORK, bump=0):
    body = "\n".join(f"- [x] {c}" for c in checks)
    p = os.path.join(STATE, "tasks", f"{sid}.md")
    with open(p, "w") as f:
        f.write(f"# Task\nSession: {sid}\nStarted: now\nCwd: {cwd}\nStatus: {status}\n\n## Outcome\nx\n\n"
                f"## Scope\ny\n\n## Done when\n{body}\n\n## Notes\n")
    if bump:
        t = time.time() + bump
        os.utime(p, (t, t))
    return p


def decisions():
    with open(ENV["CLAUDE_DECISIONS_LOG"]) as f:
        return [json.loads(l)["decision"] for l in f if l.strip()]


# parser
sys.path.insert(0, HERE)
from common import parse_task  # noqa: E402
t = parse_task("# Task\nStatus: done\n\n## Done when\n- [x] tests: $ pytest -q\n- [x] seen: the page renders\n"
               "- [ ] <check name>: $ <command that exits 0 when the check holds; the stop guard runs it>\n")
check("parse: one command, placeholders skipped", t["commands"] == [("tests", "pytest -q")] and len(t["checks"]) == 2,
      str(t["commands"]))
t = parse_task("# Task\nStatus: done\n\n## Done when\n- [x] url ok: $ curl -s http://x:1/$PATH | grep -q ok\n")
check("parse: colon inside command kept", t["commands"] == [("url ok", "curl -s http://x:1/$PATH | grep -q ok")],
      str(t["commands"]))

# 1 passing command -> allow, verified marker
p = task("e1", ["lint: $ true", "seen: the page renders"])
rc, out, err = stop("e1")
check("done + passing command -> allow", rc == 0 and not out, f"out={out} err={err[:120]}")
check("verified marker written", os.path.exists(p + ".verified"))
check("decision logged allow_done_checks_passed", "allow_done_checks_passed" in decisions())

# 2 failing command -> block once with output, then allow
p = task("e2", ["unit: $ echo boom >&2; exit 3"])
rc, out, err = stop("e2")
check("done + failing command -> block", out.get("decision") == "block", f"out={out} err={err[:120]}")
check("block names check, exit code and output",
      all(s in out.get("reason", "") for s in ("unit", "exited 3", "boom")), out.get("reason", "")[:200])
rc, out2, _ = stop("e2")
check("second stop, same file -> allow (nudged once)", rc == 0 and not out2, f"out={out2}")
check("decisions: block then already-nudged",
      "block_done_check_failed" in decisions() and "allow_done_checks_failed_already_nudged" in decisions(), str(decisions()))

cnt2 = os.path.join(WORK, "count2")
task("e2b", [f"fail: $ echo y >> {cnt2}; exit 1"])
stop("e2b"); stop("e2b"); stop("e2b")
check("failing command runs once per file version, not on every later stop", open(cnt2).read().count("y") == 1,
      open(cnt2).read())

# 3 file rewritten, still failing -> block again; then fixed -> allow and verified
task("e2", ["unit: $ exit 3"], bump=2)
rc, out, _ = stop("e2")
check("rewritten file, still failing -> block again", out.get("decision") == "block", f"out={out}")
p = task("e2", ["unit: $ true"], bump=4)
rc, out, _ = stop("e2")
check("rewritten file, now passing -> allow", rc == 0 and not out, f"out={out}")
check("verified marker now at new mtime", float(open(p + ".verified").read()) >= os.path.getmtime(p) - 0.01)

# 4 passing commands run once per file version (counter file)
cnt = os.path.join(WORK, "count")
task("e4", [f"count: $ echo x >> {cnt}"])
stop("e4"); stop("e4"); stop("e4", message="Anything else?")
check("passing command not re-run on later stops", open(cnt).read().count("x") == 1, open(cnt).read())

# 5 cwd from the task file
d = tempfile.mkdtemp(prefix="eccwd-")
task("e5", [f'cwd: $ test "$(pwd)" = "{os.path.realpath(d)}"'], cwd=d)
rc, out, _ = stop("e5")
check("command runs in the task's Cwd", rc == 0 and not out, f"out={out}")
task("e5b", ["cwd: $ test -d ."], cwd="/nonexistent/dir", bump=1)
rc, out, _ = stop("e5b")
check("missing Cwd falls back to the session cwd", rc == 0 and not out, f"out={out}")

# 6 timeout -> rc 124 reported; SubagentStop ignored; status open ignores commands
task("e6", ["slow: $ sleep 10"])
t0 = time.time(); rc, out, _ = stop("e6")
check("timed-out command -> block with 124", out.get("decision") == "block" and "exited 124" in out.get("reason", "")
      and time.time() - t0 < 9, f"out={out} dt={time.time()-t0:.1f}")
task("e6b", [f"pipe: $ sleep 30 | cat; echo done >> {os.path.join(WORK, 'pipe')}"])
rc, out, _ = stop("e6b")
for _ in range(20):  # the killed children are reaped asynchronously; allow up to 2s
    ps = subprocess.run(["pgrep", "-x", "-f", "sleep 30"], capture_output=True, text=True).stdout.strip()  # exact cmdline: the sleep itself
    if not ps:
        break
    time.sleep(0.1)
check("timed-out pipeline leaves no child running", out.get("decision") == "block" and not ps, f"pgrep={ps!r} out={out}")

task("e7", ["unit: $ exit 1"])
rc, out, _ = stop("e7", event="SubagentStop")
check("SubagentStop never runs commands", rc == 0 and not out, f"out={out}")
task("e8", ["unit: $ exit 1"], status="open")
rc, out, _ = stop("e8", message="Still working on it.")
check("Status: open, no done claim -> commands not run, allow", rc == 0 and not out, f"out={out}")
rc, out, _ = stop("e8", message="Done.")
check("Status: open + done claim -> the existing open-file block, not a command run",
      out.get("decision") == "block" and "exited" not in out.get("reason", ""), out.get("reason", "")[:120])

print("ALL PASS" if not failed else f"{failed} FAILED")
sys.exit(1 if failed else 0)
