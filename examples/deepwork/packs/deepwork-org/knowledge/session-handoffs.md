# Session Handoffs — Context Preservation

Sessions end due to credit limits, compaction, crashes, or intentional cycling.
The goal: the next session resumes work with minimal context loss.

## The Handoff Chain

```
Session N (context filling up)
  -> commits code, updates beads
  -> writes handoff mail (gt handoff)
  -> session ends

Session N+1 (fresh context)
  -> gt prime (loads role context)
  -> gt hook (checks for hooked work)
  -> gt mail inbox (finds handoff mail)
  -> reads handoff, resumes work
```

## What to Include in Handoff Mail

Keep it SHORT. The next session has full codebase access — it only needs:

1. **What you were doing** (1 sentence)
2. **What's done** (commits pushed, beads closed)
3. **What remains** (next steps, blockers)
4. **Gotchas** (things that aren't obvious from code alone)

Bad: 500-word essay about your journey
Good: "Fixed vaa-4sc (pushed). Still need to run tests on vaa-bwa. Anar polecat has uncommitted work in /tmp."

## Compaction vs. Handoff

| Event | Trigger | You Control It? | Action |
|-------|---------|-----------------|--------|
| Handoff | You decide | Yes | `gt handoff` |
| Compaction | Context window full | No | Auto-summarizes, loses detail |
| Credit limit | API quota exhausted | No | Session dies mid-work |
| Crash | Bug/infra failure | No | Whatever was pushed survives |

**Best practice:** Handoff BEFORE compaction forces it. If you notice context getting large, proactively cycle.

## Hook vs. Mail

- **Hook** (`gt hook`): Your current assignment. Survives across sessions. Check first.
- **Handoff mail** (`gt mail inbox`): Context notes. Supplements the hook.

The hook tells you WHAT to work on. The mail tells you WHERE you left off.

## Polecat Handoffs

Polecats are transient — their handoff is different:
1. Commit and push to polecat branch
2. Call `gt done` (submits to refinery)
3. Polecat is garbage collected

If a polecat dies mid-work, the manager rescues uncommitted files and closes the bead.
