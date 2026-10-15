#!/usr/bin/env python3
"""PreToolUse(Bash) guard.

1. A background command must carry a deadline: `deadline <duration> ...` as a
   command word somewhere in the pipeline (after `cd x &&`, an export, a
   subshell open, inside `bash -c '...'`, `ssh host '...'` or `eval` all count).
2. Commands must not detach themselves (nohup / disown / setsid / trailing &):
   detached processes escape the task registry, so nobody is accountable for them.

Both checks run on the command with its *text* removed first: heredoc bodies,
quoted strings and comments are data, not shell. Quoted strings that are
themselves shell are kept (quote marks dropped): the argument after
sh/bash/zsh/dash/ksh -c or -lc, eval, exec, ssh [opts] host, su [opts] -c,
sudo [opts] sh|bash -c, xargs [opts], any -exec, timeout <arg>, deadline <arg>.
Heredoc bodies fed to sh/bash/zsh/dash/ksh, ssh or eval are kept too.

Denies with the corrected command in the reason rather than rewriting the input,
because a rewrite (updatedInput) would also auto-approve the call and skip the
normal permission checks.
"""
import json
import re
import shlex
import sys

sys.path.insert(0, __file__.rsplit("/", 1)[0])
from common import log, log_allow  # noqa: E402

DEFAULT_DEADLINE = "60m"
DETACH = re.compile(r"(?:^|[\s;&|(`])(nohup|disown|setsid)\b")
TRAILING_AMPERSAND = re.compile(r"(?<![&|>])&\s*$")
# `deadline` as a command word: at the start, after a separator, inside a subshell, or
# as the first word of a `bash -c '...'` string.
HAS_DEADLINE = re.compile(
    r"(?:^|[;&|(\n]|\bthen\b|\bdo\b|\belse\b|\s-l?c\s|\bssh\s+(?:-\S+\s+)*\S+\s|\beval\s)\s*(?:\S*/)?deadline\s")

# A quoted string right after one of these is code, not text: keep it for the checks.
CODE_QUOTE_BEFORE = re.compile(
    r"(?:(?:ba|z|da|k)?sh\s+(?:-\w+\s+)*-l?c|eval|exec|ssh\s+(?:-\S+\s+)*\S+|su\s+(?:-\S+\s+)*-c"
    r"|sudo(?:\s+-\S+)*\s+(?:sh|bash)\s+-l?c|xargs(?:\s+-\S+)*|-exec|timeout\s+\S+|deadline\s+\S+)\s*$")
HEREDOC_OPEN = re.compile(r"<<-?\s*(['\"]?)([A-Za-z_][A-Za-z0-9_]*)\1[^\n]*\n")
# The heredoc's target command is a shell (or an interpreter reading it), so its body is code.
SHELL_TARGET = re.compile(r"(?:^|[\s;&|(])(?:(?:ba|z|da|k)?sh|ssh|eval)\b[^\n<|>]*$")


def strip_heredocs(cmd):
    out, pos = [], 0
    while True:
        m = HEREDOC_OPEN.search(cmd, pos)
        if not m:
            out.append(cmd[pos:])
            break
        tag = m.group(2)
        body_start = m.end()
        end = re.compile(rf"^\s*{re.escape(tag)}\s*$", re.M).search(cmd, body_start)
        body_end = end.start() if end else len(cmd)
        line_start = cmd.rfind("\n", 0, m.start()) + 1
        head = cmd[line_start:m.start()]
        keep = bool(SHELL_TARGET.search(head))
        out.append(cmd[pos:m.end()])
        out.append(cmd[body_start:body_end] if keep else " ")
        pos = body_end
    return "".join(out)


def strip_quotes(cmd):
    """Blank quoted strings and comments, unless the quote is an argument to a shell-like command."""
    out, i, n = [], 0, len(cmd)
    while i < n:
        ch = cmd[i]
        if ch == "\\":
            out.append(cmd[i:i + 2])
            i += 2
            continue
        if ch in "'\"":
            j = i + 1
            while j < n:
                if cmd[j] == "\\" and ch == '"':
                    j += 2
                    continue
                if cmd[j] == ch:
                    break
                j += 1
            inner = cmd[i + 1:j]
            keep = bool(CODE_QUOTE_BEFORE.search("".join(out)[-80:]))
            # Kept code loses its quote marks so the checks see it as plain shell.
            out.append(" " + strip_quotes(inner) + " " if keep else ch + " " + ch)
            i = j + 1
            continue
        if ch == "#" and (i == 0 or cmd[i - 1] in " \t\n;("):
            j = cmd.find("\n", i)
            i = n if j < 0 else j
            continue
        out.append(ch)
        i += 1
    return "".join(out)


def shell_only(cmd):
    return strip_quotes(strip_heredocs(cmd))


def deny(reason):
    print(json.dumps({"hookSpecificOutput": {
        "hookEventName": "PreToolUse",
        "permissionDecision": "deny",
        "permissionDecisionReason": reason,
    }}))


def main():
    data = json.load(sys.stdin)
    tool_input = data.get("tool_input") or {}
    command = tool_input.get("command") or ""
    code = shell_only(command)

    if DETACH.search(code) or TRAILING_AMPERSAND.search(code.strip()):
        log("bash_guard", "deny_detach", data, command=command[:800])
        deny("Blocked: this command detaches a process (nohup/disown/setsid/trailing &), "
             "which escapes the task registry and leaves nobody accountable for it. "
             "Run it with run_in_background: true instead, wrapped in "
             f"`deadline <duration>` (e.g. `deadline {DEFAULT_DEADLINE} <cmd>`).")
        return

    if tool_input.get("run_in_background") and not HAS_DEADLINE.search(code):
        fixed = f"deadline {DEFAULT_DEADLINE} bash -c {shlex.quote(command)}"
        log("bash_guard", "deny_no_deadline", data, command=command[:800])
        deny("Blocked: background commands need a deadline. Re-run with the command wrapped "
             "in `deadline <duration>` (seconds, or 90s/20m/2h); pick a duration that fits "
             "the job. It kills the whole process group on expiry (exit 124) and reaps "
             f"leftovers on exit. With the default {DEFAULT_DEADLINE}:\n{fixed}")
        return

    log_allow("bash_guard", data, bg=bool(tool_input.get("run_in_background")))


if __name__ == "__main__":
    main()
