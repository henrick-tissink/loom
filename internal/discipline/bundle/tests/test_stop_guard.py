"""Run a guard script against synthetic Stop / SubagentStop payloads.

Usage: python3 -I test_stop_guard.py <path-to-stop_guard.py>
Prints one line per case and exits 1 if any expectation fails.
"""
import json
import os
import subprocess
import sys

GUARD = sys.argv[1]
HERE = os.path.dirname(os.path.abspath(__file__))
PARENT_TASKS = [{"id": "t1", "type": "teammate", "description": "Audit hooks"},
                {"id": "t2", "type": "teammate", "description": "Measure burn"}]
OWNED_TASKS = [{"id": "t1", "type": "bash", "description": "parent job", "agent_id": "parent"},
               {"id": "t2", "type": "bash", "description": "my job", "agent_id": "helper-1"}]

IDLE_TASKS = [{"id": "t1", "type": "teammate", "description": "Audit hooks", "status": "idle"},
              {"id": "t2", "type": "teammate", "description": "Measure burn", "status": "completed"}]
MIXED_TASKS = IDLE_TASKS + [{"id": "t3", "type": "bash", "description": "deploy", "status": "running"}]

CASES = [
    ("Stop: only settled tasks, message silent -> allow",
     {"hook_event_name": "Stop", "background_tasks": IDLE_TASKS,
      "last_assistant_message": "All done."}, False),
    ("Stop: only settled tasks, promise to monitor -> block (nothing live backs it)",
     {"hook_event_name": "Stop", "background_tasks": IDLE_TASKS,
      "last_assistant_message": "I'll monitor it and report back."}, True),
    ("Stop: settled plus one running, message silent -> block",
     {"hook_event_name": "Stop", "background_tasks": MIXED_TASKS,
      "last_assistant_message": "All done."}, True),
    ("Stop: settled plus one running, message names it -> allow",
     {"hook_event_name": "Stop", "background_tasks": MIXED_TASKS,
      "last_assistant_message": "The deploy is still running; I'll be woken when it ends."}, False),
    ("Stop: task with no status counts as live -> block",
     {"hook_event_name": "Stop", "background_tasks": [{"id": "t9", "type": "bash", "description": "x"}],
      "last_assistant_message": "All done."}, True),
    ("SubagentStop: only settled parent tasks, helper silent -> allow",
     {"hook_event_name": "SubagentStop", "agent_id": "helper-1", "background_tasks": IDLE_TASKS,
      "last_assistant_message": "Report: 3 findings."}, False),
    # name, payload, expect_block
    ("Stop: in-flight tasks, message silent -> block",
     {"hook_event_name": "Stop", "background_tasks": PARENT_TASKS,
      "last_assistant_message": "All done."}, True),
    ("Stop: in-flight tasks, message mentions them -> allow",
     {"hook_event_name": "Stop", "background_tasks": PARENT_TASKS,
      "last_assistant_message": "Two jobs are still running; I'll be woken on completion."}, False),
    ("Stop: no tasks, promise to monitor -> block",
     {"hook_event_name": "Stop", "background_tasks": [],
      "last_assistant_message": "I'll monitor the deploy and report back."}, True),
    ("Stop: no tasks, no promise -> allow",
     {"hook_event_name": "Stop", "background_tasks": [],
      "last_assistant_message": "Done. Tests pass."}, False),
    ("Stop: second attempt (stop_hook_active) -> allow",
     {"hook_event_name": "Stop", "background_tasks": PARENT_TASKS, "stop_hook_active": True,
      "last_assistant_message": "All done."}, False),
    ("SubagentStop: parent's unowned tasks, helper silent -> allow (the bug)",
     {"hook_event_name": "SubagentStop", "agent_id": "helper-1", "background_tasks": PARENT_TASKS,
      "last_assistant_message": "Report: 3 findings."}, False),
    ("SubagentStop: parent's unowned tasks, helper promises -> allow (cannot judge)",
     {"hook_event_name": "SubagentStop", "agent_id": "helper-1", "background_tasks": PARENT_TASKS,
      "last_assistant_message": "I'll keep watching it."}, False),
    ("SubagentStop: no tasks at all, helper promises -> block",
     {"hook_event_name": "SubagentStop", "agent_id": "helper-1", "background_tasks": [],
      "last_assistant_message": "I'll keep watching it."}, True),
    ("SubagentStop: owned task present, helper silent -> block",
     {"hook_event_name": "SubagentStop", "agent_id": "helper-1", "background_tasks": OWNED_TASKS,
      "last_assistant_message": "Report: 3 findings."}, True),
    ("SubagentStop: owned task present, helper mentions it -> allow",
     {"hook_event_name": "SubagentStop", "agent_id": "helper-1", "background_tasks": OWNED_TASKS,
      "last_assistant_message": "My job t2 is still running; wake me when it ends."}, False),
    ("SubagentStop: only parent-owned tasks, helper silent -> allow",
     {"hook_event_name": "SubagentStop", "agent_id": "helper-1",
      "background_tasks": [OWNED_TASKS[0]], "last_assistant_message": "Report: 3 findings."}, False),
    ("SubagentStop: registry absent, helper promises -> allow (unknown registry)",
     {"hook_event_name": "SubagentStop", "agent_id": "helper-1",
      "last_assistant_message": "I'll keep watching it."}, False),
]

failed = 0
for name, payload, expect_block in CASES:
    payload.setdefault("session_id", "s-test")
    payload.setdefault("cwd", HERE)
    out = subprocess.run([sys.executable, "-I", GUARD], input=json.dumps(payload), env={**os.environ, "CLAUDE_DECISIONS_LOG": "/dev/null"},
                         capture_output=True, text=True, timeout=20)
    blocked = '"decision": "block"' in out.stdout
    ok = blocked == expect_block and out.returncode == 0
    failed += 0 if ok else 1
    print(("PASS" if ok else "FAIL"), "|", name, "" if ok else f"| got block={blocked} rc={out.returncode} err={out.stderr.strip()[:200]}")
print(f"{len(CASES) - failed}/{len(CASES)} passed")
sys.exit(1 if failed else 0)
