#!/usr/bin/env python3
"""Summarise test-run logging: how often tests are re-run with no tree change.

Usage: python3 -I test_rerun_report.py [decisions.jsonl] [--days N]
"""
import collections
import json
import os
import sys
import time

args = sys.argv[1:]
days = int(args[args.index("--days") + 1]) if "--days" in args else 30
positional = [a for i, a in enumerate(args) if not a.startswith("--") and not (i > 0 and args[i - 1] == "--days")]
_cfg = os.environ.get("CLAUDE_CONFIG_DIR") or os.path.expanduser("~/.claude")
_logdir = os.path.join(os.environ.get("CLAUDE_STATE_DIR") or os.path.join(_cfg, "state"), "log")
path = positional[0] if positional else (os.environ.get("CLAUDE_DECISIONS_LOG")
                                         or os.path.join(_logdir, "decisions.jsonl"))
cut = time.strftime("%Y-%m-%dT%H:%M:%S", time.localtime(time.time() - days * 86400))
tot = collections.Counter(); per = collections.defaultdict(collections.Counter); same = collections.Counter(); gaps = []
with open(path) as f:
    for line in f:
        try:
            d = json.loads(line)
        except ValueError:
            continue
        if d.get("hook") != "test_rerun" or d.get("ts", "") < cut:
            continue
        c = d["changed"]; proj = (d.get("cwd") or "?").replace(os.path.expanduser("~"), "~")
        tot[c] += 1; per[proj][c] += 1
        if c == "no":
            same[bool(d.get("same_cmd"))] += 1
            if d.get("since_last_s") is not None:
                gaps.append(d["since_last_s"])
runs = sum(tot.values())
if not runs:
    print(f"no test runs logged in the last {days} days"); sys.exit()
rer = tot["no"]
print(f"test runs: {runs}  | first-in-session: {tot['first']}  after a change: {tot['yes']}  "
      f"re-run with NO change: {rer} ({100*rer/runs:.0f}%)  unknown (not git): {tot['unknown']}")
if rer:
    print(f"  of the no-change re-runs: same command {same[True]}, different command {same[False]}; "
          f"median gap {sorted(gaps)[len(gaps)//2] if gaps else '?'}s")
print("per project (runs / no-change re-runs):")
for proj, c in sorted(per.items(), key=lambda kv: -sum(kv[1].values())):
    n = sum(c.values()); print(f"  {n:>4} / {c['no']:>3}  {proj}")
