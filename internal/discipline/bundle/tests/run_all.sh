#!/bin/bash
# Runs the suites against a hooks dir: $HOOKS_DIR, else $1, else the installed
# ~/.claude/hooks/enforce. Loom's gate points it at a staged copy.
# Each suite runs from a temp dir holding a copy of the hooks plus the test, so
# imports of common.py resolve and nothing writes to your real logs or state.
HERE="$(cd "$(dirname "$0")" && pwd)"
SRC="${HOOKS_DIR:-${1:-$HOME/.claude/hooks/enforce}}"
[ -d "$SRC" ] || { echo "no hooks at $SRC"; exit 2; }
export CLAUDE_DECISIONS_LOG=/dev/null CLAUDE_ALLOWS_LOG=/dev/null
fail=0
for t in test_finishline test_stop_guard test_ctxbudget test_bashguard test_testlog test_execchecks test_session_lineage; do
  W="$(mktemp -d)"
  cp "$SRC"/*.py "$W"/
  cp "$HERE/$t.py" "$W"/
  export CLAUDE_STATE_DIR="$W/state"; mkdir -p "$CLAUDE_STATE_DIR"
  if [ "$t" = test_stop_guard ]; then args="$W/stop_guard.py"; else args=""; fi
  out="$(cd "$W" && python3 -I "$W/$t.py" $args 2>&1)"; rc=$?
  if [ $rc -eq 0 ]; then echo "PASS  $t  ($(printf '%s\n' "$out" | grep -c '^PASS') checks)"; else echo "FAIL  $t (exit $rc)"; printf '%s\n' "$out" | grep -v '^PASS' | tail -15; fail=1; fi
  rm -rf "$W"
done
[ $fail -eq 0 ] && echo "OVERALL: PASS" || echo "OVERALL: FAIL"
exit $fail
