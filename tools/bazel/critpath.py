#!/usr/bin/env python3
"""Report the critical-path gap for a bazel profile and name the next lever.

Usage: bazel test //... --profile=/tmp/p.json ...; then tools/bazel/critpath.py /tmp/p.json

Decision rule:
  gap = elapsed - critical_path
  gap < 15%  -> scheduling is fine; attack the longest critical-path action
                (shard it if it's a test; parallelize inputs if it's an upload)
  gap > 15%  -> actions waited for resources; grow the farm or raise --jobs
"""
import json, sys, re

prof = json.load(open(sys.argv[1]))
ev = prof.get("traceEvents", [])
elapsed = max((e.get("dur", 0) for e in ev if e.get("name") == "buildTargets"), default=0) / 1e6
cps = sorted((e for e in ev if e.get("cat") == "critical path component" and e.get("dur", 0) > 1e6),
             key=lambda e: e["dur"], reverse=True)
cp = max((e["dur"] for e in cps), default=0) / 1e6
gap = elapsed - cp
pct = (gap / elapsed * 100) if elapsed else 0

print(f"elapsed        {elapsed:7.1f}s")
print(f"critical path  {cp:7.1f}s   ({', '.join(e['name'].split('action ')[-1][:40] for e in cps[:3])})")
print(f"gap            {gap:7.1f}s   ({pct:.0f}% of elapsed)")
print()
if pct < 15:
    top = cps[0]
    name = top["name"]
    m = re.search(r"Testing (\S+)", name)
    if m:
        print(f"NEXT: shard or speed up {m.group(1)} ({top['dur']/1e6:.0f}s)")
    elif "upload" in name.lower():
        print("NEXT: input upload dominates — warm the CAS/worker fast stores or slim inputs")
    else:
        print(f"NEXT: attack {name[:60]} ({top['dur']/1e6:.0f}s)")
else:
    print("NEXT: scheduling gap — grow the elastic pool (autoscaler target divisor),")
    print("      raise --jobs, or check worker fast-store hit rates")
