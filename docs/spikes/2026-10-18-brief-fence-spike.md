# Spike: the fence for a brief's "Ask about this" session

Date: 2026-10-18
Environment: macOS (Darwin 25.3.0), `claude --version` → **2.1.302 (Claude Code)**.

A brief's study session (`session.Recipe{Fenced: true}`) is an interactive
Claude session seeded with a task from the brief. It should be able to read the
project's code and the brief, and nothing else: no shell, no edits, no web, and
in particular **no MCP tools**. This account has claude.ai connectors (Claude
Docs, Gmail, Calendar, Drive), and those can publish or send content.

## Question

Does `--tools Read,Grep,Glob` by itself remove MCP tools? If not, what does?

## Method

Two arms of `claude -p` with haiku. Each asks for "the exact names of every tool
available to you, one per line, … Include any mcp__ tools", run with
`--no-session-persistence` and the parent's `CLAUDECODE` / `CLAUDE_CODE_*`
variables unset:

| arm | extra flags |
|---|---|
| control | `--tools "Read,Grep,Glob"` |
| fenced | `--tools "Read,Grep,Glob" --strict-mcp-config --mcp-config '{"mcpServers":{}}' --setting-sources ""` |

## Result

| arm | tools listed | `mcp__*` | `mcp__claude_ai_*` (connectors) |
|---|---|---|---|
| control | 46 | 43 | 38 |
| fenced | **3** (`Glob`, `Grep`, `Read`) | **0** | **0** |

**`--tools` only limits the built-in tools.** MCP tools are not affected: the
claude.ai connectors and the user-scope MCP servers (lava, volcano) all stay
available. `--strict-mcp-config` with an empty `mcpServers` removes every MCP
tool, connectors included. `--setting-sources ""` also stops user, project and
local settings from loading, so no hooks and no settings-defined MCP servers
come with them.

Dropping the settings also drops the user's own deny rules. The fence therefore
supplies its own rules through `--settings`. A second check confirmed that flag
settings still apply under `--setting-sources ""`:

```
--settings '{"permissions":{"deny":["Read(**/.env*)"]}}'
→ readme.txt read; .env.local: "File is in a directory that is denied by your permission settings."
```

## Decision

`fenceArgs()` in `internal/session/recipe.go` is the fenced arm verbatim. The
fence's deny list for secrets (`.env*`, keys, `~/.ssh`, `~/.aws`, `~/.claude/*.json`,
the gh/glab credential dirs, …) is merged into `--settings`. The session row is
tagged `fenced`, and a resume rebuilds the same argv (`IsFenced`). Without the
tag, a resumed study session would come back with every tool.

Side observation: with settings loaded, the CLI warns that the user's
`Write(~/.claude/hooks/**)` deny rule is not matched by file permission checks
and should be `Edit(...)`.
