"""session_checkpoint: /clear finds its predecessor by claude process, not by cwd.

Usage: python3 -I test_session_lineage.py   (from the staging directory)
"""
import json
import os
import subprocess
import sys
import tempfile
import time

HERE = os.path.dirname(os.path.abspath(__file__))
STATE = tempfile.mkdtemp(prefix="lnstate-")
PROJ = tempfile.mkdtemp(prefix="lnproj-")  # stands in for ~/.claude/projects/<launch dir>
ROOT = "/work/repo"
WORKTREE = ROOT + "/.claude/worktrees/feature"
ENV = {**os.environ, "CLAUDE_STATE_DIR": STATE, "CLAUDE_DECISIONS_LOG": os.path.join(STATE, "d.jsonl")}
failed = 0


def run(payload, pid="4242"):
    env = {**ENV, "CLAUDE_PROCESS_PID": pid}
    r = subprocess.run([sys.executable, "-I", os.path.join(HERE, "session_checkpoint.py")],
                       input=json.dumps({"hook_event_name": "SessionStart", **payload}),
                       capture_output=True, text=True, timeout=30, env=env)
    out = json.loads(r.stdout) if r.stdout.strip() else {}
    return r.returncode, (out.get("hookSpecificOutput") or {}).get("additionalContext", ""), r.stderr


def check(name, cond, detail=""):
    global failed
    failed += 0 if cond else 1
    print(("PASS" if cond else "FAIL"), "|", name, "" if cond else f"| {detail}")


def ckpt(sid, cwd, body, age=0, mechanical=False):
    d = os.path.join(STATE, "checkpoints")
    os.makedirs(d, exist_ok=True)
    p = os.path.join(d, f"{sid}{'.mechanical' if mechanical else ''}.md")
    with open(p, "w") as f:
        f.write(f"# Checkpoint\nSession: {sid}\nWritten: x\nCwd: {cwd}\n\n## Assignment\n{body}\n\n## Next step\n{body} next\n")
    t = time.time() - age
    os.utime(p, (t, t))
    return p


def task(sid, cwd, outcome, status="open", label=""):
    d = os.path.join(STATE, "tasks")
    os.makedirs(d, exist_ok=True)
    p = os.path.join(d, f"{sid}{label}.md")
    with open(p, "w") as f:
        f.write(f"# Task\nSession: {sid}\nStarted: x\nCwd: {cwd}\nStatus: {status}\n\n## Outcome\n{outcome}\n\n"
                "## Done when\n- [ ] t: $ true\n")
    return p


def reset():
    for sub in ("checkpoints", "tasks", "by-pid"):
        d = os.path.join(STATE, sub)
        if os.path.isdir(d):
            for n in os.listdir(d):
                os.remove(os.path.join(d, n))
    for n in os.listdir(PROJ):
        os.remove(os.path.join(PROJ, n))


def transcript(sid):
    p = os.path.join(PROJ, f"{sid}.jsonl")
    open(p, "w").close()
    return p


# --- today's failure: worktree checkpoint, older decoy whose Cwd equals the launch dir ---
reset()
run({"session_id": "A", "cwd": ROOT, "source": "startup", "transcript_path": transcript("A")})
ckpt("DECOY", ROOT, "launch completeness decisions", age=2 * 3600)
task("DECOY", ROOT, "Every open launch item is closed.", label=".launch")
ckpt("A", WORKTREE, "admin groups + owner names")
rc, c, err = run({"session_id": "B", "cwd": ROOT, "source": "clear", "transcript_path": transcript("B")})
check("clear: predecessor's worktree checkpoint injected", rc == 0 and "admin groups" in c and "this /clear replaced (A," in c, c[:300] + err)
check("clear: older cwd-matching decoy not injected", "launch completeness" not in c, c[:300])
check("clear: decoy's open task file not injected", "Every open launch item" not in c, c[:300])
check("clear: asks to name the checkpoint", "tell the user in one line" in c, c[:300])

# --- predecessor's own open task file comes with it (labelled file name) ---
task("A", WORKTREE, "Operators save without a group.", label=".admin")
run({"session_id": "A", "cwd": ROOT, "source": "compact", "transcript_path": transcript("A")})  # re-own pid
rc, c, err = run({"session_id": "C", "cwd": ROOT, "source": "clear", "transcript_path": transcript("C")})
check("clear: predecessor's open task injected", "Operators save without a group" in c and "admin groups" in c, c[:400])

# --- a subagent's SessionStart does not displace the session in the record ---
run({"session_id": "A", "cwd": ROOT, "source": "compact"})
run({"session_id": "SUB", "cwd": ROOT, "source": "startup", "agent_type": "Explore"})
rc, c, err = run({"session_id": "D", "cwd": ROOT, "source": "clear"})
check("subagent start ignored: predecessor still A", "this /clear replaced (A," in c, c[:300])

# --- another claude process: no lineage, falls back to matching ---
run({"session_id": "A", "cwd": ROOT, "source": "compact"})
rc, c, err = run({"session_id": "E", "cwd": ROOT, "source": "clear"}, pid="9999")
check("other process: no lineage claimed", "this /clear replaced" not in c, c[:300])
check("other process: two matches listed, none resumed",
      "Several recent checkpoints" in c and "- A (" in c and "- DECOY (" in c and "admin groups next" not in c, c[:600])
check("list: open task files named per row", "DECOY.launch.md" in c and "A.admin.md" in c, c[:800])

# --- stale process record is not a predecessor ---
reset()
os.makedirs(os.path.join(STATE, "by-pid"), exist_ok=True)
with open(os.path.join(STATE, "by-pid", "4242"), "w") as f:
    json.dump({"sid": "OLD", "ts": time.time() - 25 * 3600}, f)
ckpt("OLD", ROOT, "old work", age=60)
rc, c, err = run({"session_id": "F", "cwd": ROOT, "source": "clear"})
check("stale pid record: no lineage", "this /clear replaced" not in c and "previous session in this directory" in c, c[:300])

# --- predecessor without a checkpoint: falls back, no lineage claim ---
reset()
run({"session_id": "G", "cwd": ROOT, "source": "startup"})
ckpt("H", WORKTREE, "worktree work", age=600)
rc, c, err = run({"session_id": "I", "cwd": ROOT, "source": "clear"})
check("predecessor has no checkpoint: single match under cwd injected, labelled as older",
      "Checkpoint from the session this /clear replaced" not in c and "worktree work" in c
      and "(G) left no checkpoint" in c and "tell the user in one line" in c, c[:400])

# --- predecessor without a checkpoint keeps its own open task file ---
task("I", ROOT, "Short session's own finish line.")
task("H", WORKTREE, "Earlier session's finish line.")
rc, c, err = run({"session_id": "J2", "cwd": ROOT, "source": "clear"})
check("predecessor has no checkpoint: its open task file injected, not the earlier one's",
      "Short session's own finish line" in c and "Earlier session's finish line" not in c, c[:400])

# --- startup: launch-dir match through the transcript, even with an unrelated Cwd ---
reset()
transcript("J")
ckpt("J", "/somewhere/else", "launched here, drifted away", age=60)
rc, c, err = run({"session_id": "K", "cwd": "/other", "source": "startup", "transcript_path": transcript("K")})
check("startup: same project dir matches despite drifted Cwd", "launched here, drifted away" in c, c[:300])
rc, c, err = run({"session_id": "L", "cwd": "/other", "source": "startup"})
check("startup: no project dir, unrelated Cwd -> silent", c == "", c[:300])

# --- a sibling directory with a shared prefix is not 'under' ---
reset()
ckpt("M", ROOT + "-other", "sibling repo", age=60)
rc, c, err = run({"session_id": "N", "cwd": ROOT, "source": "startup"})
check("startup: /work/repo-other is not under /work/repo", c == "", c[:300])

print(f"\n{'ALL PASS' if not failed else f'{failed} FAILED'}  (state: {STATE})")
sys.exit(1 if failed else 0)
