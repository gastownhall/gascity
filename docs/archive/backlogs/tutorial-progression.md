---
title: "Capability Progression"
---

Internal reference — what each tutorial unlocks and what it requires.

| Tut | Problem | Config added | Prompts | Infrastructure used |
|-----|---------|--------------|---------|---------------------|
| 01 | Context loss kills progress | `[workspace]`, `[[agent]]` | one-shot | beads, session, reconciler |
| 02 | Named agents for different jobs | Multiple `[[agent]]` | mayor, worker | agent claim (assign to named agent) |
| 03 | Hand-feeding tasks one at a time | — | loop | claim (atomic self-claim via ready queue) |
| 04 | One agent too slow | More `[[agent]]` entries | — | — (just config + existing hooks) |
| 06 | Restart from scratch on multi-step work | `[formulas]` | — | formula parser, molecules (bead DAG) |
| 07 | Reusable formulas with specific context | `gc mol create --on` | — | attached molecules, Store.Update |
| 08 | Need more hands when work piles up | `[[pools]]`, `scale_check` | polecat | pool manager, scale_check shell eval |
| 09 | Agents stepping on each other's files | `dir` on `[[agent]]`, `GC_DIR` env | scoped-worker | resolveAgentDir, MkdirAll |
| 05b | Agents die silently | `[daemon]`, `[agent.health]` | — | health patrol, restart |
| 05c | Manual maintenance chores | `[orders]` | — | order gates, event bus |
| 05d | Multi-repo orchestration | `[projects.*]`, `scope` | — | project scoping, agent replication |
| 10 | Multiple projects need isolated task tracking | `[[rigs]]`, prefix, routes | — | InitRigBeads, deriveBeadsPrefix, routes.jsonl |
