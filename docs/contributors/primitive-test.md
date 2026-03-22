---
title: "The Primitive Test"
---

Decision framework for whether a capability belongs in Gas City's SDK
primitive layer or in the consumer layer (agent prompts, `bd` CLI, user
config, external binaries).

## The three necessary conditions

A capability belongs in the SDK **only if all three hold.** If any
condition fails, it belongs in the consumer layer.

### 1. Atomicity — can agents do it safely without races?

If two agents calling CLI commands concurrently can corrupt state or
violate invariants, the SDK must provide the atomic version. If it's a
single-agent operation, naturally idempotent, or the underlying tool
already handles concurrency (e.g., SQL transactions, INSERT IGNORE),
agents can call it directly.

**Questions to ask:**
- Can two agents hit this operation simultaneously?
- Does the underlying tool (bd, git, tmux) already provide atomicity?
- Is there a read-check-write pattern that could race?

**Examples:**
- `bd label add` → INSERT IGNORE deduplicates → safe → consumer layer
- `bd slot clear` → idempotent set-to-empty → safe → consumer layer
- `bd slot set hook` → read-check-write race → fix in beads, not Gas City
- Two agents hooking the same bead → needs atomic CAS → Gas City's
  `beads.Store.Claim()` provides this when the underlying store doesn't

### 2. Bitter Lesson — does it become MORE useful as models improve?

If a smarter model would do it better from the prompt, it fails the
Bitter Lesson test and belongs in the consumer layer. If it's pure
plumbing that models will always delegate to (and never improve upon),
it's a primitive.

**The test:** Imagine a model 10x more capable. Does this capability
become less necessary (→ consumer layer) or exactly as necessary
(→ primitive)?

**Examples:**
- "Decide whether to unhook a stale bead" → judgment → consumer layer
  (smarter model decides better)
- "Atomically transition bead status" → plumbing → primitive
  (smarter model still needs this)
- "Detect which agent should handle a task" → judgment → consumer layer
- "Create a git worktree" → plumbing → primitive
- "`gc done` command" → encodes judgment about when/how to finish →
  consumer layer (the agent decides its own done flow)

### 3. ZFC — is it transport or cognition?

If implementing it in Go requires a judgment call (`if stuck then X`),
it's cognition and belongs in the prompt. If it's pure data movement,
process management, or filesystem operations, it's transport and belongs
in the SDK.

**The test:** Does any line of Go contain a judgment call? If yes, the
decision belongs in the prompt, not the code.

**Examples:**
- "Query all beads where status=hooked" → transport → primitive
- "Decide recovery strategy for crashed agent" → cognition → consumer
- "Remove a git worktree" → transport → primitive
- "Detect stale hooks and decide what to do" → cognition → consumer
- "Send SIGTERM to a process" → transport → primitive

## Applying the framework

### Decision table template

| Capability | Atomicity needed? | Bitter Lesson pass? | ZFC pass? | Verdict |
|---|---|---|---|---|
| ... | Does the underlying tool race? | Does a smarter model still need this? | Is it pure transport? | All three → primitive |

### Common verdicts

**Primitive (all three pass):**
- Bead CRUD, hook with conflict detection
- Git worktree create/remove/list
- Session start/stop/attach
- Event append
- Config parse/validate

**Consumer layer (at least one fails):**
- Done flow orchestration (fails Bitter Lesson — model decides)
- Stale hook recovery strategy (fails ZFC — judgment)
- Bidirectional hook tracking (fails Atomicity — two `bd` calls suffice)
- Agent bead creation (fails Atomicity — `bd create` works)
- Label management (fails Atomicity — `bd label` is safe)

**Fix upstream (Atomicity problem in dependency):**
- `bd slot set hook` race → fix in beads, not Gas City

## The corollary: when to fix upstream vs wrap

If a capability fails the Primitive Test only because the underlying tool
has a concurrency bug, the right fix is in the tool — not a wrapper in
Gas City. Gas City wraps tools for ergonomics (consistent API), not to
paper over bugs.

**Fix upstream when:** The tool's own semantics should be atomic but aren't.
(Example: `bd slot set hook` should be atomic but has a read-check-write race.)

**Wrap in Gas City when:** The tool is correct but the SDK needs to compose
multiple tool calls atomically. (Example: create worktree + setup redirect
needs rollback if redirect fails.)
