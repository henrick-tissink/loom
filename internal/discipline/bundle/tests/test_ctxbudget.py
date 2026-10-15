"""Synthetic-payload tests for the three context-budget hooks.

Usage: python3 -I test_ctxbudget.py   (run from a directory holding the hooks and common.py)
"""
import json
import os
import shutil
import subprocess
import sys
import tempfile
import time

HERE = os.path.dirname(os.path.abspath(__file__))
STATE = tempfile.mkdtemp(prefix="ctxstate-")
WORK = tempfile.mkdtemp(prefix="ctxwork-")
ENV = {**os.environ, "CLAUDE_DECISIONS_LOG": "/dev/null", "CLAUDE_STATE_DIR": STATE, "CLAUDE_CONTEXT_BUDGET": "200000",
       "CLAUDE_CONTEXT_BUDGET_STEP": "50000", "CLAUDE_CHECKPOINT_FRESH_SECONDS": "1800"}
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


def transcript(tokens, user_text="Fix the login bug", assistant_text="Done, tests pass."):
    path = os.path.join(WORK, f"t{tokens}.jsonl")
    recs = [
        {"type": "user", "message": {"role": "user", "content": user_text}},
        {"type": "assistant", "isSidechain": True,
         "message": {"content": [{"type": "text", "text": "side"}],
                     "usage": {"input_tokens": 10, "cache_read_input_tokens": 999999}}},
        {"type": "assistant", "message": {"content": [{"type": "text", "text": assistant_text}],
                                          "usage": {"input_tokens": 100, "cache_read_input_tokens": tokens - 600,
                                                    "cache_creation_input_tokens": 500}}},
    ]
    with open(path, "w") as f:
        f.write("\n".join(json.dumps(r) for r in recs) + "\n")
    return path


SID = "sess-1"
base = {"session_id": SID, "cwd": WORK, "hook_event_name": "UserPromptSubmit"}
ckpt = os.path.join(STATE, "checkpoints", f"{SID}.md")
mech = os.path.join(STATE, "checkpoints", f"{SID}.mechanical.md")

# --- ctx_budget ---
rc, out, err = run("ctx_budget.py", {**base, "transcript_path": transcript(150_000), "prompt": "continue"})
check("budget: 150k under budget -> silent", rc == 0 and not out, f"rc={rc} out={out} err={err[:120]}")

rc, out, err = run("ctx_budget.py", {**base, "transcript_path": transcript(250_000), "prompt": "continue"})
check("budget: 250k over budget -> nudge with path", rc == 0 and "CONTEXT BUDGET" in ctx_of(out) and ckpt in ctx_of(out),
      f"rc={rc} ctx={ctx_of(out)[:100]} err={err[:120]}")

rc, out, err = run("ctx_budget.py", {**base, "transcript_path": transcript(260_000), "prompt": "continue"})
check("budget: 260k same level -> silent (nudged once)", rc == 0 and not out, f"out={out}")

rc, out, err = run("ctx_budget.py", {**base, "transcript_path": transcript(310_000), "prompt": "continue"})
check("budget: 310k next step -> nudge again", rc == 0 and "310,000" in ctx_of(out), f"ctx={ctx_of(out)[:100]}")

rc, out, err = run("ctx_budget.py", {**base, "transcript_path": transcript(400_000), "prompt": "/compact"})
check("budget: slash command -> silent", rc == 0 and not out, f"out={out}")

rc, out, err = run("ctx_budget.py", {**base, "transcript_path": transcript(50_000), "prompt": "write a checkpoint"})
check("budget: explicit checkpoint request -> path and shape", rc == 0 and "CHECKPOINT:" in ctx_of(out)
      and "## Next step" in ctx_of(out), f"ctx={ctx_of(out)[:100]}")

rc, out, err = run("ctx_budget.py", {**base, "transcript_path": transcript(50_000), "prompt": "add that checkpoint rule to the doc"})
check("budget: word 'checkpoint' mid-sentence -> silent", rc == 0 and not out, f"out={out}")

rc, out, err = run("ctx_budget.py", {**base, "transcript_path": os.path.join(WORK, "missing.jsonl"), "prompt": "hi"})
check("budget: missing transcript -> silent, exit 0", rc == 0 and not out, f"rc={rc} err={err[:120]}")

# --- precompact ---
pre = {"session_id": SID, "cwd": WORK, "hook_event_name": "PreCompact", "custom_instructions": None,
       "transcript_path": transcript(300_000, "Please refactor the parser", "Parser refactored; 12 tests green.")}
rc, out, err = run("precompact_checkpoint.py", {**pre, "trigger": "manual"})
check("precompact: manual, no checkpoint -> block once (exit 2, stderr names path)",
      rc == 2 and ckpt in err and os.path.exists(mech), f"rc={rc} err={err[:160]}")
m = open(mech).read()
check("precompact: mechanical snapshot has last prompt and reply",
      "Please refactor the parser" in m and "Parser refactored" in m and f"Cwd: {WORK}" in m, m[:200])

rc, out, err = run("precompact_checkpoint.py", {**pre, "trigger": "manual"})
check("precompact: manual second time -> allowed", rc == 0, f"rc={rc} err={err[:120]}")

shutil.rmtree(os.path.join(STATE, "ctxbudget"))
rc, out, err = run("precompact_checkpoint.py", {**pre, "trigger": "auto"})
check("precompact: auto, no checkpoint -> never blocked", rc == 0, f"rc={rc} err={err[:120]}")

with open(ckpt, "w") as f:
    f.write(f"# Checkpoint\nSession: {SID}\nWritten: now\nCwd: {WORK}\n\n## Next step\nRun the migration\n")
rc, out, err = run("precompact_checkpoint.py", {**pre, "trigger": "manual"})
check("precompact: manual with fresh checkpoint -> allowed, snapshot embeds it",
      rc == 0 and "Run the migration" in open(mech).read(), f"rc={rc} err={err[:120]}")

# --- session start ---
ss = {"session_id": SID, "cwd": WORK, "hook_event_name": "SessionStart"}
rc, out, err = run("session_checkpoint.py", {**ss, "source": "compact"})
c = ctx_of(out)
check("session: compact -> model checkpoint first, then snapshot",
      rc == 0 and c.find("Run the migration") < c.find("Mechanical snapshot") and "Run the migration" in c, c[:200])

rc, out, err = run("session_checkpoint.py", {"session_id": "sess-NEW", "cwd": WORK, "hook_event_name": "SessionStart", "source": "startup"})
check("session: startup in same cwd -> previous checkpoint injected with caution",
      rc == 0 and "previous session" in ctx_of(out) and "Run the migration" in ctx_of(out), ctx_of(out)[:200])

rc, out, err = run("session_checkpoint.py", {"session_id": "sess-NEW", "cwd": "/elsewhere", "hook_event_name": "SessionStart", "source": "startup"})
check("session: startup in other cwd -> silent", rc == 0 and not out, f"out={out}")

old = time.time() - 25 * 3600
os.utime(ckpt, (old, old)); os.utime(mech, (old, old))
rc, out, err = run("session_checkpoint.py", {"session_id": "sess-NEW", "cwd": WORK, "hook_event_name": "SessionStart", "source": "startup"})
check("session: startup with only stale checkpoints -> silent", rc == 0 and not out, f"out={out}")

print(f"{'all' if not failed else failed} {'passed' if not failed else 'FAILED'}; state dir {STATE}")
sys.exit(1 if failed else 0)
