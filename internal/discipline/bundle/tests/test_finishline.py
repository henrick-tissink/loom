"""Synthetic-payload tests for the finish-line task file across the four hooks.

Usage: python3 -I test_finishline.py   (from the staging directory)
"""
import json
import os
import subprocess
import sys
import tempfile
import time

HERE = os.path.dirname(os.path.abspath(__file__))
STATE = tempfile.mkdtemp(prefix="flstate-")
WORK = tempfile.mkdtemp(prefix="flwork-")
LOG = os.path.join(STATE, "decisions.jsonl")
ENV = {**os.environ, "CLAUDE_STATE_DIR": STATE, "CLAUDE_DECISIONS_LOG": LOG,
       "CLAUDE_CONTEXT_BUDGET": "200000", "CLAUDE_CONTEXT_BUDGET_STEP": "50000"}
failed = 0


def run(hook, payload):
    r = subprocess.run([sys.executable, "-I", os.path.join(HERE, hook)], input=json.dumps(payload),
                       capture_output=True, text=True, timeout=30, env=ENV)
    out = {}
    if r.stdout.strip():
        try:
            out = json.loads(r.stdout)
        except ValueError:
            out = {"raw": r.stdout}
    return r.returncode, out, r.stderr


def ctx_of(out):
    return (out.get("hookSpecificOutput") or {}).get("additionalContext", "")


def check(name, cond, detail=""):
    global failed
    failed += 0 if cond else 1
    print(("PASS" if cond else "FAIL"), "|", name, "" if cond else f"| {detail}")


def last_decision():
    try:
        with open(LOG) as f:
            lines = [l for l in f if l.strip()]
        return json.loads(lines[-1])["decision"] if lines else ""
    except OSError:
        return ""


def task_file(sid, status="open", checks=(("tests: pytest", False), ("live probe: curl /health", False)),
              outcome="Users can reset their password from the login page.", cwd=WORK, bump=0):
    d = os.path.join(STATE, "tasks")
    os.makedirs(d, exist_ok=True)
    p = os.path.join(d, f"{sid}.md")
    body = [f"# Task", f"Session: {sid}", "Started: 2026-10-13 13:00", f"Cwd: {cwd}", f"Status: {status}", "",
            "## Outcome", outcome, "", "## Scope", "auth module only", "", "## Done when"]
    body += [f"- [{'x' if ok else ' '}] {t}" for t, ok in checks]
    body += ["", "## Notes", ""]
    with open(p, "w") as f:
        f.write("\n".join(body) + "\n")
    if bump:
        t = time.time() + bump
        os.utime(p, (t, t))
    return p


def transcript(tokens):
    path = os.path.join(WORK, f"t{tokens}.jsonl")
    recs = [{"type": "user", "message": {"role": "user", "content": "hi"}},
            {"type": "assistant", "message": {"content": [{"type": "text", "text": "ok"}],
                                              "usage": {"input_tokens": 100, "cache_read_input_tokens": tokens - 100}}}]
    with open(path, "w") as f:
        f.write("\n".join(json.dumps(r) for r in recs) + "\n")
    return path


def stop(sid, message, event="Stop", **extra):
    return run("stop_guard.py", {"session_id": sid, "cwd": WORK, "hook_event_name": event,
                                 "last_assistant_message": message, "background_tasks": [], **extra})


def blocked(out):
    return out.get("decision") == "block"


print("--- stop_guard: finish line ---")
rc, out, err = stop("s0", "Done. The container runs abc123 and both hosts answer.")
check("no task file + done claim -> allow, logged", rc == 0 and not out and last_decision() == "allow_done_no_task_file",
      f"out={out} dec={last_decision()} err={err[:200]}")

p = task_file("s1")
rc, out, err = stop("s1", "Done. Tests pass and the fix is deployed.")
check("task open + 'Done.' -> block naming path and checks", blocked(out) and p in out.get("reason", "")
      and "tests: pytest" in out.get("reason", ""), f"out={out} err={err[:200]}")
rc, out, err = stop("s1", "All done, everything is in place.")
check("same task version again -> allow (nudged once)", rc == 0 and not out and last_decision() == "allow_finish_line_already_nudged",
      f"out={out} dec={last_decision()}")
task_file("s1", bump=5)
rc, out, err = stop("s1", "It's live.")
check("task file rewritten + \"It's live.\" -> block again", blocked(out), f"out={out} err={err[:200]}")

task_file("s2")
for msg in ["Still working on it; the migration is not done yet, two checks remain.",
            "Once it's live I will need you to check the DNS. For now, the branch is pushed.",
            "Here is the plan: three steps, starting with the schema.",
            "The rule fires on \"all done\" claims, so quoting one is safe.",
            "Found the bug in auth.py:42; the fix is a one-liner. Want me to apply it?"]:
    rc, out, err = stop("s2", msg)
    check(f"task open + no claim -> allow: {msg[:45]!r}", rc == 0 and not out, f"out={out} err={err[:200]}")

task_file("s3", status="done", checks=(("tests: pytest 12 passed", True), ("live probe: 200 OK", True)))
rc, out, err = stop("s3", "Done. 12 tests pass and the live probe returns 200.")
check("task done, all ticked + done claim -> allow", rc == 0 and not out, f"out={out} err={err[:200]}")

task_file("s4", status="done", checks=(("tests: pytest 12 passed", True), ("live probe: curl /health", False)))
rc, out, err = stop("s4", "Everything is done.")
check("task done but a check unticked -> block naming it", blocked(out) and "live probe" in out.get("reason", "")
      and "Status: done" in out.get("reason", ""), f"out={out}")
rc, out, err = stop("s4", "Thanks, that's all.")
check("done+unticked, second stop -> allow (nudged once)", rc == 0 and not out, f"out={out}")

task_file("s5", status="consultation", checks=())
rc, out, err = stop("s5", "Done, that answers it: the hook reads the file.")
check("consultation + done claim -> allow", rc == 0 and not out, f"out={out}")

task_file("s6")
rc, out, err = stop("s6", "Done.", event="SubagentStop", agent_id="helper-1")
check("SubagentStop ignores finish line", rc == 0 and not out, f"out={out}")
rc, out, err = stop("s6", "Done.", stop_hook_active=True)
check("stop_hook_active -> allow", rc == 0 and not out, f"out={out}")

task_file("s7")
rc, out, err = stop("s7", "Done. I'll keep an eye on the deploy and report back.")
check("unbacked promise still checked first", blocked(out) and "promises" in out.get("reason", ""), f"out={out}")

for msg in ["All deployed and verified. Nothing left open on my side.",
            "Verified live at 3e5d8b9, working tree clean, nothing unpushed.",
            "**Done.** The container is running bfceb2b.",
            "All seven workstreams are built, merged, and verified."]:
    i = abs(hash(msg)) % 10000
    task_file(f"s8-{i}")
    rc, out, err = stop(f"s8-{i}", msg)
    check(f"real hand-back shape blocks: {msg[:40]!r}", blocked(out), f"out={out} err={err[:200]}")

print("--- ctx_budget: task-file nudge ---")
base = {"cwd": WORK, "hook_event_name": "UserPromptSubmit", "transcript_path": transcript(50_000)}
tp = os.path.join(STATE, "tasks", "c1.md")
rc, out, err = run("ctx_budget.py", {**base, "session_id": "c1", "prompt": "ok"})
check("trivial first prompt -> silent", rc == 0 and not out, f"out={out} err={err[:200]}")
rc, out, err = run("ctx_budget.py", {**base, "session_id": "c1", "prompt": "/clear"})
check("slash prompt -> silent", rc == 0 and not out, f"out={out}")
rc, out, err = run("ctx_budget.py", {**base, "session_id": "c1", "prompt": "add password reset to the login page"})
check("first real prompt, no task file -> FINISH LINE with path and shape, no checkpoint request",
      "FINISH LINE" in ctx_of(out) and tp in ctx_of(out) and "## Done when" in ctx_of(out) and "Status: open" in ctx_of(out)
      and "CHECKPOINT" not in ctx_of(out),
      f"ctx={ctx_of(out)[:200]} err={err[:200]}")
rc, out, err = run("ctx_budget.py", {**base, "session_id": "c1", "prompt": "and make it pretty"})
check("second prompt, still no file -> silent", rc == 0 and not out, f"out={out}")
task_file("c1")
rc, out, err = run("ctx_budget.py", {**base, "session_id": "c1", "prompt": "continue with the tests"})
check("task file open -> silent", rc == 0 and not out, f"out={out}")
task_file("c1", status="done", bump=5)
rc, out, err = run("ctx_budget.py", {**base, "session_id": "c1", "prompt": "now also fix the signup form"})
check("task file closed -> nudge once, says closed, no checkpoint request", "closed" in ctx_of(out) and "Status: done" in ctx_of(out)
      and "CHECKPOINT" not in ctx_of(out), f"ctx={ctx_of(out)[:200]}")
rc, out, err = run("ctx_budget.py", {**base, "session_id": "c1", "prompt": "go on"})
check("closed file, second prompt -> silent", rc == 0 and not out, f"out={out}")

task_file("c2")
rc, out, err = run("ctx_budget.py", {**base, "session_id": "c2", "prompt": "checkpoint"})
check("checkpoint request prefills Assignment from task file",
      "Outcome: Users can reset their password" in ctx_of(out) and "Scope: auth module only" in ctx_of(out)
      and "Done when:\n- [ ] tests: pytest\n- [ ] live probe" in ctx_of(out), f"ctx={ctx_of(out)[:400]}")
rc, out, err = run("ctx_budget.py", {**base, "session_id": "c3", "prompt": "checkpoint"})
check("checkpoint request without task file leaves Assignment blank and nudges task file",
      "Outcome: \n" in ctx_of(out) and "FINISH LINE" in ctx_of(out), f"ctx={ctx_of(out)[:400]}")
task_file("c4")
rc, out, err = run("ctx_budget.py", {**base, "session_id": "c4", "transcript_path": transcript(250_000), "prompt": "continue"})
check("budget nudge still fires with prefilled Assignment", "CONTEXT BUDGET" in ctx_of(out) and "Outcome: Users can" in ctx_of(out),
      f"ctx={ctx_of(out)[:300]}")

print("--- precompact / session_checkpoint ---")
task_file("p1")
rc, out, err = run("precompact_checkpoint.py", {"session_id": "p1", "cwd": WORK, "hook_event_name": "PreCompact",
                                                "trigger": "auto", "transcript_path": transcript(100)})
mech = open(os.path.join(STATE, "checkpoints", "p1.mechanical.md")).read()
check("mechanical snapshot includes the task file", rc == 0 and "## Task file" in mech and "tests: pytest" in mech, f"rc={rc} err={err[:200]}")

rc, out, err = run("session_checkpoint.py", {"session_id": "p1", "cwd": WORK, "hook_event_name": "SessionStart", "source": "compact"})
check("compact: task file injected", "Task file for this session" in ctx_of(out) and "tests: pytest" in ctx_of(out), f"ctx={ctx_of(out)[:200]}")

rc, out, err = run("session_checkpoint.py", {"session_id": "new-1", "cwd": WORK, "hook_event_name": "SessionStart", "source": "startup"})
check("startup: newest open task file for cwd injected", "Open task file from a previous session" in ctx_of(out)
      and "Users can reset" in ctx_of(out), f"ctx={ctx_of(out)[:300]}")
for sid in ("p1", "s1", "s2", "s6", "s7", "c1", "c2", "c4") + tuple(f"s8-{abs(hash(m)) % 10000}" for m in [
        "All deployed and verified. Nothing left open on my side.",
        "Verified live at 3e5d8b9, working tree clean, nothing unpushed.",
        "**Done.** The container is running bfceb2b.",
        "All seven workstreams are built, merged, and verified."]):
    task_file(sid, status="done")
rc, out, err = run("session_checkpoint.py", {"session_id": "new-2", "cwd": WORK, "hook_event_name": "SessionStart", "source": "startup"})
check("startup: only closed task files -> nothing injected", rc == 0 and "Open task file from a previous session" not in ctx_of(out), f"ctx={ctx_of(out)[:200]}")
task_file("p2", cwd="/elsewhere")
rc, out, err = run("session_checkpoint.py", {"session_id": "new-3", "cwd": WORK, "hook_event_name": "SessionStart", "source": "startup"})
check("startup: open task file for another cwd -> not injected", rc == 0 and "Open task file from a previous session" not in ctx_of(out), f"ctx={ctx_of(out)[:200]}")

print(f"\n{'ALL PASS' if not failed else f'{failed} FAILED'}  (state: {STATE})")
sys.exit(1 if failed else 0)
