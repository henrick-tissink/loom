#!/usr/bin/env python3
"""PreCompact: preserve state before the prefix is summarised.

Always writes a mechanical snapshot (branch, git status, last prompts, last
reply, and the model-written checkpoint if one exists). On a manual /compact
with no fresh model checkpoint, blocks once per session with a message to the
user; the next /compact goes through. Never blocks auto compaction, because a
blocked auto compaction can surface a context-limit error and fail the turn.

Env: CLAUDE_STATE_DIR (default ~/.claude/state), CLAUDE_CHECKPOINT_FRESH_SECONDS (1800).
"""
import json
import os
import subprocess
import sys
import time

sys.path.insert(0, __file__.rsplit("/", 1)[0])
from common import load_task, log, read_text  # noqa: E402

STATE = os.environ.get("CLAUDE_STATE_DIR") or os.path.expanduser("~/.claude/state")
FRESH = int(os.environ.get("CLAUDE_CHECKPOINT_FRESH_SECONDS", "1800"))
TAIL_BYTES = 2_000_000


def tail_records(path):
    try:
        size = os.path.getsize(path)
        with open(path, "rb") as f:
            if size > TAIL_BYTES:
                f.seek(size - TAIL_BYTES)
            chunk = f.read()
    except OSError:
        return []
    lines = chunk.split(b"\n")
    if size > TAIL_BYTES:
        lines = lines[1:]
    out = []
    for line in lines:
        if not line.strip():
            continue
        try:
            out.append(json.loads(line))
        except ValueError:
            continue
    return out


def text_of(record):
    content = (record.get("message") or {}).get("content")
    if isinstance(content, str):
        return content.strip()
    if isinstance(content, list):
        return "\n".join(b.get("text", "") for b in content
                         if isinstance(b, dict) and b.get("type") == "text").strip()
    return ""


def git(cwd, *args):
    try:
        r = subprocess.run(["git", "-C", cwd, "--no-optional-locks", *args],
                           capture_output=True, text=True, timeout=5)
        return r.stdout.strip() if r.returncode == 0 else ""
    except (OSError, subprocess.SubprocessError):
        return ""


def main():
    data = json.load(sys.stdin)
    sid = data.get("session_id") or "unknown"
    cwd = data.get("cwd") or ""
    trigger = data.get("trigger") or "auto"
    ckpt_dir = os.path.join(STATE, "checkpoints")
    os.makedirs(ckpt_dir, exist_ok=True)
    ckpt = os.path.join(ckpt_dir, f"{sid}.md")
    mech = os.path.join(ckpt_dir, f"{sid}.mechanical.md")

    fresh = False
    try:
        fresh = (time.time() - os.path.getmtime(ckpt)) < FRESH
    except OSError:
        pass

    records = tail_records(data.get("transcript_path") or "")
    users = [t for t in (text_of(r) for r in records if r.get("type") == "user" and not r.get("isSidechain"))
             if t and not t.startswith("[") and not t.startswith("<")][-3:]
    assistant = ""
    for r in reversed(records):
        if r.get("type") == "assistant" and not r.get("isSidechain"):
            assistant = text_of(r)
            if assistant:
                break

    lines = [f"# Mechanical snapshot ({trigger} compaction)",
             f"Session: {sid}", f"Written: {time.strftime('%Y-%m-%d %H:%M')}", f"Cwd: {cwd}", ""]
    if cwd and os.path.isdir(cwd):
        branch = git(cwd, "branch", "--show-current")
        if branch:
            status = git(cwd, "status", "--short").splitlines()
            lines += [f"## Git", f"Branch: {branch}", f"Uncommitted files: {len(status)}"]
            lines += status[:40] + ([f"... {len(status) - 40} more"] if len(status) > 40 else []) + [""]
    lines += ["## Last user prompts"] + [f"- {u[:300]}" for u in users] + [""]
    lines += ["## Last assistant message", assistant[:1500], ""]
    task_file, task, _ = load_task(sid)
    if task:
        lines += [f"## Task file ({task_file})", read_text(task_file).strip(), ""]
    if os.path.exists(ckpt):
        try:
            with open(ckpt) as f:
                lines += ["## Model-written checkpoint" + ("" if fresh else " (stale)"), f.read().strip(), ""]
        except OSError:
            pass
    with open(mech, "w") as f:
        f.write("\n".join(lines) + "\n")

    if trigger == "manual" and not fresh:
        marker_dir = os.path.join(STATE, "ctxbudget")
        os.makedirs(marker_dir, exist_ok=True)
        marker = os.path.join(marker_dir, f"{sid}.precompact-nudged")
        if not os.path.exists(marker):
            with open(marker, "w") as f:
                f.write(time.strftime("%Y-%m-%dT%H:%M:%S"))
            log("precompact", "block_manual_compact_no_checkpoint", data)
            sys.stderr.write(
                f"No fresh checkpoint for this session (expected {ckpt}, newer than {FRESH // 60} min). "
                f"A mechanical snapshot was saved to {mech}. Tell Claude to write the checkpoint, then run "
                "/compact again; the second /compact is not blocked.\n")
            sys.exit(2)

    log("precompact", "allow_compact", data, trigger=trigger, fresh_checkpoint=fresh)


if __name__ == "__main__":
    main()
