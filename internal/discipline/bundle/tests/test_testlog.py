"""Synthetic tests for test_rerun_log.py. Usage: python3 -I test_testlog.py"""
import json, os, subprocess, sys, tempfile, time
HERE = os.path.dirname(os.path.abspath(__file__))
STATE = tempfile.mkdtemp(prefix="tlstate-"); LOG = os.path.join(STATE, "d.jsonl")
ENV = {**os.environ, "CLAUDE_STATE_DIR": STATE, "CLAUDE_DECISIONS_LOG": LOG}
failed = 0
def run(cmd, sid="t1", cwd=None, bg=False):
    t0 = time.time()
    r = subprocess.run([sys.executable, "-I", os.path.join(HERE, "test_rerun_log.py")], input=json.dumps(
        {"session_id": sid, "cwd": cwd or STATE, "hook_event_name": "PreToolUse", "tool_name": "Bash",
         "tool_input": {"command": cmd, "run_in_background": bg}}), capture_output=True, text=True, timeout=30, env=ENV)
    return r, time.time() - t0
def last():
    try:
        lines = [l for l in open(LOG) if l.strip()]; return json.loads(lines[-1]) if lines else {}
    except OSError: return {}
def nlog():
    try: return sum(1 for l in open(LOG) if l.strip())
    except OSError: return 0
def check(name, cond, detail=""):
    global failed; failed += 0 if cond else 1; print(("PASS" if cond else "FAIL"), "|", name, "" if cond else f"| {detail}")

repo = tempfile.mkdtemp(prefix="tlrepo-")
subprocess.run(["git", "init", "-q", repo], check=True)
subprocess.run(["git", "-C", repo, "-c", "user.email=t@t", "-c", "user.name=t", "commit", "-q", "--allow-empty", "-m", "init"], check=True)
open(os.path.join(repo, "a.py"), "w").write("x = 1\n")

for c in ["ls -la", "cat decisions.jsonl | grep test", "git status", "echo pytest is great", "python3 script.py", "npm run build"]:
    n = nlog(); r, _ = run(c, cwd=repo)
    check(f"non-test command -> silent: {c!r}", r.returncode == 0 and not r.stdout.strip() and nlog() == n, f"rc={r.returncode} out={r.stdout[:80]} err={r.stderr[:120]}")

r, dt = run("python3 -m pytest tests/ -q", cwd=repo)
d = last(); check("first test run -> log_test_first", d.get("decision") == "log_test_first" and d.get("run_no") == 1 and d.get("git") is True, f"d={d} err={r.stderr[:200]}")
check("hook is fast (<0.5s)", dt < 0.5, f"{dt:.2f}s")
r, _ = run("python3 -m pytest tests/ -q", cwd=repo)
d = last(); check("identical re-run, no change -> log_test_rerun_no_change same_cmd", d.get("decision") == "log_test_rerun_no_change" and d.get("same_cmd") is True and d.get("run_no") == 2, f"d={d}")
r, _ = run("pytest tests/test_x.py -k foo", cwd=repo)
d = last(); check("different test cmd, no change -> no_change, same_cmd False", d.get("decision") == "log_test_rerun_no_change" and d.get("same_cmd") is False, f"d={d}")
open(os.path.join(repo, "a.py"), "a").write("y = 2\n")
r, _ = run("pytest tests/test_x.py -k foo", cwd=repo)
d = last(); check("untracked file edited -> log_test_after_change", d.get("decision") == "log_test_after_change", f"d={d}")
subprocess.run(["git", "-C", repo, "add", "a.py"], check=True)
subprocess.run(["git", "-C", repo, "-c", "user.email=t@t", "-c", "user.name=t", "commit", "-q", "-m", "a"], check=True)
r, _ = run("pytest", cwd=repo); d = last(); check("commit -> after_change", d.get("decision") == "log_test_after_change", f"d={d}")
open(os.path.join(repo, "a.py"), "a").write("z = 3\n")
r, _ = run("pytest", cwd=repo); d = last(); check("tracked file edited -> after_change", d.get("decision") == "log_test_after_change", f"d={d}")
r, _ = run("pytest", cwd=repo); d = last(); check("nothing touched -> no_change", d.get("decision") == "log_test_rerun_no_change" and d.get("since_last_s") is not None, f"d={d}")

nogit = tempfile.mkdtemp(prefix="tlnogit-")
r, _ = run("npm test", sid="t2", cwd=nogit); d = last(); check("non-git dir first run -> first, git False", d.get("decision") == "log_test_first" and d.get("git") is False, f"d={d}")
r, _ = run("npm test", sid="t2", cwd=nogit); d = last(); check("non-git dir re-run -> unknown", d.get("decision") == "log_test_rerun_unknown", f"d={d}")

for c in ["cargo test --workspace", "go test ./...", "dotnet test Foo.sln", "npx vitest run", "pnpm test:unit", "deadline 20m bash -c 'npm run test'", "cd app && yarn test --watch=false", "make test", "bun test", "node --test"]:
    n = nlog(); r, _ = run(c, sid="t3", cwd=repo)
    check(f"recognised as test: {c!r}", nlog() == n + 1, f"out={r.stdout[:80]} err={r.stderr[:120]}")

rep = subprocess.run([sys.executable, "-I", os.path.join(HERE, "test_rerun_report.py"), LOG], capture_output=True, text=True, env=ENV)
check("report runs and shows the no-change rate", rep.returncode == 0 and "re-run with NO change" in rep.stdout and "per project" in rep.stdout, rep.stdout[:300] + rep.stderr[:200])
print(rep.stdout)
print("ALL PASS" if not failed else f"{failed} FAILED"); sys.exit(1 if failed else 0)
