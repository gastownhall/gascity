#!/usr/bin/env python3
"""Critical-path report and CI budget gate.

THE OPTIMIZATION LOOP
=====================

  gap = elapsed - critical_path   (per run, from bazel --profile)

  Decision rule (formalized):
    gap < 15% of elapsed  → scheduling is NOT the bottleneck.
                            Attack the longest critical-path component:
                              - a test action  → shard it further or optimize
                                its slowest individual tests
                              - input upload   → warm CAS/worker fast stores
                              - a build action → slim inputs, split the target
    gap >= 15% of elapsed → actions waited for capacity.
                            Attack the farm/scheduling:
                              - autoscaler target divisor (workers per demand)
                              - --jobs on the client
                              - worker fast-store hit rate

  MEASUREMENT TIERS (bazel caching makes single numbers lie):
    T1 "steady"  = CI run with warm remote CAS/AC + warm runner disk cache.
                   The PR experience. GOAL metric lives here.
    T2 "cold-client" = CI run on a fresh runner (empty disk cache), warm
                   remote CAS. What CI pays without runner caching.
    T3 "cold-farm" = after worker recycle / CAS eviction. Worst case;
                   track but never gate on it.

  GOAL: T1 bazel test elapsed ≤ 60s.

Usage:
  critpath.py PROFILE [--budget SECONDS] [--tier NAME]
Exit code 1 iff --budget given and elapsed exceeds it.
"""
import json, sys, re, argparse

def main():
    ap = argparse.ArgumentParser(add_help=False)
    ap.add_argument("profile")
    ap.add_argument("--budget", type=float, default=None)
    ap.add_argument("--tier", default="T1")
    args = ap.parse_args()

    prof = json.load(open(args.profile))
    ev = prof.get("traceEvents", [])
    elapsed = max((e.get("dur", 0) for e in ev if e.get("name") == "buildTargets"), default=0) / 1e6
    cps = sorted((e for e in ev if e.get("cat") == "critical path component" and e.get("dur", 0) > 1e6),
                 key=lambda e: e["dur"], reverse=True)
    cp = cps[0]["dur"] / 1e6 if cps else 0.0
    gap = elapsed - cp
    pct = (gap / elapsed * 100) if elapsed else 0.0

    print(f"[{args.tier}] elapsed        {elapsed:7.1f}s")
    print(f"[{args.tier}] critical path  {cp:7.1f}s   ({', '.join(e['name'].split('action ')[-1][:44] for e in cps[:2])})")
    print(f"[{args.tier}] gap            {gap:7.1f}s   ({pct:.0f}% of elapsed)")
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
            print(f"NEXT: attack {name[:64]} ({top['dur']/1e6:.0f}s)")
    else:
        print("NEXT: scheduling gap — grow the elastic pool, raise --jobs,")
        print("      or add a runner disk cache so inputs are not re-hashed.")

    if args.budget is not None:
        if elapsed > args.budget:
            print(f"\nBUDGET: FAIL — {elapsed:.1f}s > {args.budget:.0f}s ({args.tier})")
            return 1
        print(f"\nBUDGET: pass — {elapsed:.1f}s ≤ {args.budget:.0f}s ({args.tier})")
    return 0

if __name__ == "__main__":
    sys.exit(main())
