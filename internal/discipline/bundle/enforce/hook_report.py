#!/usr/bin/env python3
"""Intervention rate per hook: silent passes (allows.jsonl) vs interventions (decisions.jsonl).

Usage: python3 -I hook_report.py [--days N]
"""
import collections, json, os, sys, time
days = int(sys.argv[sys.argv.index("--days") + 1]) if "--days" in sys.argv else 14
cut = time.strftime("%Y-%m-%dT%H:%M:%S", time.localtime(time.time() - days * 86400))
_cfg = os.environ.get("CLAUDE_CONFIG_DIR") or os.path.expanduser("~/.claude")
base = os.path.join(os.environ.get("CLAUDE_STATE_DIR") or os.path.join(_cfg, "state"), "log")
ALLOWS = os.environ.get("CLAUDE_ALLOWS_LOG") or os.path.join(base, "allows.jsonl")
DECISIONS = os.environ.get("CLAUDE_DECISIONS_LOG") or os.path.join(base, "decisions.jsonl")
allow, inter = collections.Counter(), collections.defaultdict(collections.Counter)
for name, sink in ((ALLOWS, None), (DECISIONS, inter)):
    try:
        for line in open(name):
            try:
                d = json.loads(line)
            except ValueError:
                continue
            if d.get("ts", "") < cut:
                continue
            if sink is None:
                allow[d["hook"]] += 1
            else:
                sink[d["hook"]][d["decision"]] += 1
    except OSError:
        pass
print(f"last {days} days  (allows logged since the allow counter was installed)")
for hook in sorted(set(allow) | set(inter)):
    n_int = sum(inter[hook].values()); n_all = allow[hook]; tot = n_int + n_all
    blocks = sum(v for k, v in inter[hook].items() if k.startswith(("block", "deny", "nudge")))
    print(f"{hook:18s} runs={tot:>6}  silent={n_all:>6}  logged={n_int:>5}  blocking={blocks:>4} ({100*blocks/tot if tot else 0:.1f}%)")
    for dec, v in inter[hook].most_common(6):
        print(f"{'':18s}   {v:>5}  {dec}")
