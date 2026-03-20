---
title: "Worktree Isolation Roadmap"
---

## Current state (what's implemented)

Gas City has basic worktree isolation:

- `isolation = "worktree"` on agents creates per-agent git worktrees
- Pool agents each get separate worktrees (worker-1, worker-2, etc.)
- Worktrees live under `.gc/worktrees/<rig>/<agent>/`
- Branch naming: `gc/<agent>-<base36-nanos>`
- `.beads/redirect` files point worktrees to shared bead stores
- `createAgentWorktree` is idempotent (reuses existing worktrees)
- `gc stop` preserves worktrees by default (like `gt down`)
- `gc stop --clean` removes worktrees with safety checks (like `gt shutdown`)
- `HasUncommittedWork()` safety check skips dirty worktrees during `--clean`

## The problem: worktrees must persist across restarts

Gas Town distinguishes **pause** vs **permanent shutdown**:

| Gas Town command | Sessions | Worktrees | Beads | Gas City equivalent |
|------------------|----------|-----------|-------|---------------------|
| `gt down` (pause) | Killed | **Preserved** | Preserved | `gc stop` |
| `gt up` (resume) | Re-created | **Auto-discovered** | Read | `gc start` |
| `gt shutdown` | Killed | **Removed** | Preserved | `gc stop --clean` (future) |

**Fixed:** `gc stop` now preserves worktrees (pause). `gc stop --clean`
removes them with safety checks (permanent shutdown).

### Why worktrees must persist

1. **Crash recovery.** Agent crashes mid-task. Its worktree has uncommitted
   changes representing hours of work. `gc stop && gc start` must not
   destroy that work.

2. **Context continuity.** A restarted agent picks up where it left off.
   Its worktree has the right branch checked out, local changes present,
   bead hook pointing to the current task.

3. **Pool scaling.** Pool scales down from 5 → 2 agents. The 3 scaled-down
   worktrees may have in-flight work. Destroying them immediately loses
   that work. (Gas Town uses drain timeouts for this.)

4. **NDI (Nondeterministic Idempotence).** Sessions come and go; the work
   survives. Worktrees + beads are the persistent substrate. Multiple
   independent observers can reconcile the same state.

### How Gas Town handles restart discovery

Gas Town uses **filesystem discovery** (ZFC: no status files):

1. On `gt up`, scan `polecats/<name>/` directories for existing worktrees
2. Scan tmux for existing sessions
3. Cross-reference with beads agent registry (`hook_bead` field)
4. If worktree exists + bead has work assigned → reuse worktree, start session
5. If worktree exists + no work assigned → available for new assignment
6. If session exists but worktree gone → kill orphan session

Key code: `ReconcilePool()` in `gastown/internal/polecat/manager.go`

## What's already fixed

- [x] `gc stop` preserves worktrees by default
- [x] `gc stop --clean` flag for explicit cleanup
- [x] `HasUncommittedWork()` safety check in `internal/git`
- [x] `cleanupWorktrees` skips dirty worktrees with stderr warning
- [x] `createAgentWorktree` idempotent (reuses on `gc start`)

## Roadmap: what to build when

### Tutorial 03 (Ralph Loop)

- [ ] Auto-respawn on crash (tmux pane-died hook)
- [ ] Worktree persists across agent restarts within a running city
- [ ] Agent discovers its worktree path from `GC_DIR` env var

### Tutorial 04 (Agent Team)

- [ ] Cross-rig worktrees (crew pattern)
- [ ] Identity preservation across rigs (env vars + git config)
- [ ] `gc worktree add/remove/list` commands for crew management

### Tutorial 05a (Formulas)

- [ ] Merge-back flow: formula step that merges worktree branch → main
- [ ] Post-merge worktree cleanup (only after confirmed merge)

### Tutorial 05b (Health Patrol)

- [ ] Zombie detection: tmux session alive but agent process dead
- [ ] Stale hook scanning: dead agent + hooked bead → unhook
- [ ] **Worktree safety check before unhook** (check for partial work)
- [ ] PID tracking for orphan process cleanup
- [ ] Mass death detection (multiple agents dying rapidly)

### Tutorial 05d (Full Orchestration)

- [ ] Bare repo pattern (`.repo.git` + worktrees) for efficiency
- [ ] Full worktree lifecycle: create → work → merge → cleanup
- [ ] Name pool recycling (release polecat names after cleanup)

## Key design patterns from Gas Town

### 1. Filesystem as source of truth (ZFC)

No worktree registry files. Discovery by scanning directories +
`git worktree list`. Consistent with CLAUDE.md's "No status files"
rule.

### 2. Beads track work assignments, not worktree state

The bead's `hook` field says "agent X is working on task Y."
The worktree is an implementation detail. If the worktree is gone
but the hook exists, something is wrong → flag for recovery.

### 3. Two-phase cleanup

1. **Soft stop** (`gt down` / `gc stop`): kill sessions, keep worktrees
2. **Hard stop** (`gt shutdown` / `gc stop --clean`): kill sessions,
   verify no partial work, remove worktrees, prune git

### 4. Stale hook = dead session + hooked bead

Before unhooking:
1. Check if agent session is alive (tmux + process check)
2. Check if worktree has uncommitted changes
3. If dead + clean → unhook bead (status → open, reassignable)
4. If dead + dirty → warn, don't unhook (manual recovery needed)

### 5. Startup beacon for context restoration

When a session restarts in an existing worktree, the startup command
includes a "beacon" — context about what work was assigned. This lets
the agent resume without losing track of its current task.

In Gas City, this maps to prompt templates reading from beads:
the agent's prompt includes its current hook status.

## Polecat lifecycle (Gas Town's ephemeral agent pattern)

The polecat is Gas Town's ephemeral agent — a pool worker (min 0) that
spawns on demand, does work, self-destructs. This is the pattern Gas City
agents with `pool.min = 0` will follow. It's also the most bug-prone
lifecycle in Gas Town.

### The 7-step lifecycle

#### Step 1: Work creation & assignment (`gt sling`)

`SpawnPolecatForSling()` does an atomic multi-step spawn:

1. `AllocateName()` — picks a name from the name pool (filesystem-derived,
   never persisted). Writes `.pending` marker, creates directory, removes
   marker.
2. `AddWithOptions()` — creates the git worktree (`git worktree add -b`)
   at `polecats/<name>/`. Sets up environment.
3. `hookBead` — **atomically** sets two fields:
   - On the **agent bead**: `hook_bead: "<issue-id>"` (what work I'm doing)
   - On the **work bead**: `status: hooked`, `assignee: "<rig>/polecats/<name>"`
     (who's doing me)

The hook is set at spawn time, not after. The agent is born knowing its
assignment. This is critical for crash recovery: if the agent dies, the
hook on the work bead tells the system who was working on it.

**Gas City equivalent:** `createAgentWorktree` + `setupBeadsRedirect` +
a future hook mechanism on beads. The atomic hook-at-spawn pattern must
be preserved.

#### Step 2: Agent spawning (`SessionManager.Start()`)

1. Create tmux session with startup command
2. Startup command includes a **beacon** — structured context about the
   assigned work (hook bead ID, molecule ID, rig path, worktree path)
3. Environment variables set: `GT_POLECAT_PATH`, `GT_POLECAT_NAME`,
   `GT_RIG_PATH`, etc.
4. Wait for Claude process to actually start (poll tmux pane PID)
5. Return only after agent is confirmed running

**Gas City equivalent:** Already partially done — `GC_DIR`, `GC_AGENT`,
`GC_RIG`, `GC_BRANCH` env vars. Startup beacon maps to prompt templates
that read from beads.

#### Step 3: Agent doing work

Agent reads its hook via `gt prime --hook` and implements the work in its
isolated worktree. The agent has full autonomy (GUPP: "If you find work
on your hook, YOU RUN IT").

**Gas City equivalent:** Prompt templates + bead queries. No Go code needed.

#### Step 4: Done flow (`gt done`) — the critical path

This is where most bugs live. The done flow has **6 sub-steps**:

1. **Validate state** — branch pushed? No uncommitted work? On correct
   branch? If validation fails, abort with error.

2. **Auto-detect cleanup mode:**
   - `push` — just push the branch (no MR)
   - `mr` — create merge request
   - `nuke` — destroy without preserving work

3. **Write done-intent label** — Written EARLY in the flow as a crash
   detection breadcrumb. If the agent dies mid-done, the label's presence
   tells the witness "this agent was trying to finish." Without it, a dead
   agent with hooked work looks like a crash-mid-work (different recovery).

4. **Push branch → create MR** — If MR mode, push to remote and create
   the merge request via git forge API.

5. **Notify witness** — Signal the witness (polecat manager) that this
   agent is done. The witness handles post-merge verification.

6. **Self-nuke worktree + self-kill session** — The polecat:
   a. Verifies branch is pushed to remote (`git ls-remote`)
   b. `cd /` (escape the worktree CWD — learned from the CWD bug)
   c. `git worktree remove --force <path>`
   d. `git worktree prune`
   e. Kills own tmux session

**Key insight: the polecat self-cleans.** The witness does NOT normally
clean up worktrees. The witness only handles: (a) pending MRs that need
post-merge verification, and (b) zombie/crash recovery.

**Gas City equivalent:** This maps to a formula (Tutorial 05a). The done
flow is a multi-step sequence that the SDK must support but NOT hardcode.
The self-nuke pattern is agent behavior (prompt-driven), not framework
behavior.

#### Step 5: Witness cleanup (exceptional path only)

The witness handles cleanup in these cases:

- **Pending MR:** MR created but not yet merged. Witness polls for merge,
  then does post-merge verification (run tests on merged result), then
  cleans up worktree.
- **Crash during done:** `done-intent` label present but agent died.
  Witness detects this and completes the cleanup.
- **Zombie recovery:** Session alive but agent process dead. Health patrol
  detects, kills session, witness unhooks bead.

**Gas City equivalent:** Health patrol (Tutorial 05b) + formula steps
(Tutorial 05a). The witness pattern maps to a "supervisor agent" that
watches pool agents.

#### Step 6: Name recycling

`namePool.Release(name)` — but InUse is **derived from filesystem
scanning**, never persisted. On restart, the name pool scans `polecats/`
directories to determine which names are in use. Pure ZFC.

**Gas City equivalent:** When we add pool name management, derive state
from `.gc/worktrees/<rig>/` directory contents.

#### Step 7: Error cases

| Failure mode | Detection | Recovery |
|---|---|---|
| Agent crash mid-work | Session dead + hook exists + no done-intent | Restart agent in same worktree, resume from hook |
| Agent crash mid-done | Session dead + done-intent label present | Witness completes cleanup |
| Zombie session | Session alive + process dead | Health patrol kills session, unhooks bead |
| Stuck agent | Hook age > threshold | Health patrol nudges, then escalates |
| Orphaned bead | Hook exists + no matching agent/worktree | Stale hook scan unhooks (if worktree clean) |
| Uncommitted work + dead agent | Dirty worktree + session dead | **Manual recovery required** — warn operator |
| Name stuck in .pending | .pending file age > 5 min | Reconciliation deletes stale .pending |

### SDK implications for Gas City

1. **Self-nuke is agent behavior, not framework behavior.** The SDK
   provides the primitives (`git worktree remove`, session kill) but the
   agent's prompt drives the done flow. The framework NEVER initiates
   worktree removal of a live agent.

2. **Done-intent labels are essential.** Any "agent finishing" flow needs
   a breadcrumb written early so crash recovery can distinguish "crashed
   mid-work" from "crashed mid-cleanup."

3. **Hook atomicity matters.** The work bead and agent bead must be
   updated together. If only one is updated, the system enters an
   inconsistent state that's hard to detect.

4. **The witness is a derived pattern.** It composes from: health patrol
   (detect death) + bead queries (find stale hooks) + git ops (cleanup
   worktrees). The SDK provides these primitives; the witness role is
   configuration.

5. **Recovery strategy depends on crash timing:**
   - Crash mid-work → restart in same worktree (preserve everything)
   - Crash mid-done → complete the cleanup (done-intent present)
   - Clean death → recycle name + worktree

## Gas Town cleanup bugs: lessons for Gas City

Gas Town's cleanup sequence has been a major source of bugs. These are
the hard-won patterns Gas City must adopt to avoid repeating them.

### Shutdown ordering (dependency order, not alphabetical)

Gas Town shuts down in strict dependency order:

1. Stop refineries (output consumers)
2. Stop witnesses (polecat managers)
3. Stop orchestration (mayor, deacon)
4. Stop daemon (background Go process)
5. Stop infrastructure (dolt server)
6. Kill tracked PIDs
7. Graceful orphan cleanup (SIGTERM → 60s → SIGKILL)
8. Verify shutdown (rescan for respawned processes)

**Lesson:** Consumers stop before providers. If a witness dies while a
refinery is still writing, the refinery reads stale state. Gas City's
`gc stop` must stop agents in reverse dependency order when we add
`depends_on` relationships.

### PID reuse kills unrelated processes

**Bug:** Track PID 12345 for agent. Agent crashes, kernel reuses PID
12345 for systemd. Cleanup sends SIGTERM to systemd.

**Fix:** Store process start time alongside PID. Before killing,
verify current start time matches recorded time. If mismatch, the PID
was reused — skip the kill.

```go
// Gas Town: pidtrack.go
// Store: "12345|Wed Feb 24 10:30:00 2026"
// Before SIGTERM: read /proc/PID/stat start time, compare
```

**Gas City:** When we add PID tracking (Tutorial 05b), store start
time. Never send signals based on PID alone.

### Permission-denied on worktree removal

**Bug:** `os.RemoveAll` fails on worktrees with read-only files
(e.g., `.git/objects/pack/` files, node_modules with 0444 perms).

**Fix:** `forceRemoveDir()` — walk tree, chmod everything writable,
retry `RemoveAll`.

```go
// Gas Town: manager.go
func forceRemoveDir(dir string) error {
    if err := os.RemoveAll(dir); err == nil { return nil }
    filepath.WalkDir(dir, func(path string, d os.DirEntry, _ error) error {
        if d.IsDir() { os.Chmod(path, 0755) } else { os.Chmod(path, 0644) }
        return nil
    })
    return os.RemoveAll(dir) // retry
}
```

**Gas City:** Our `removeAgentWorktree` should adopt this pattern.
Currently it falls back to `os.RemoveAll` but doesn't chmod first.

### Verify removal actually completed

**Bug:** `os.RemoveAll` returns nil but directory still exists (NFS,
FUSE, race with process writing to directory).

**Fix:** `verifyRemovalComplete()` — after remove, `os.Stat` the
path. If still exists, retry with `forceRemoveDir`. Return error
if still present after retry.

**Gas City:** Add verification to `removeAgentWorktree`.

### Stale .pending markers from crashed allocations

**Bug:** `AllocateName` writes `.pending` marker file, then crashes
before creating directory. Next allocation doesn't see the directory
but `.pending` blocks the name. Name is stuck forever.

**Fix:** Reconciliation treats `.pending` files older than 5 minutes
as orphaned and deletes them.

**Gas City:** When we add pool name allocation, use age-based cleanup
for any intermediate state markers.

### TOCTOU in orphan detection

**Bug:** Process scan finds orphan at T0. By T1 (signal time), the
process has been adopted by a tmux session. SIGTERM kills a
now-legitimate agent.

**Fix:** Re-verify orphan status immediately before sending signal.
Check TTY, check tmux session PIDs, confirm still orphaned.

```go
// Gas Town: orphan.go
func isProcessStillOrphaned(pid int) bool {
    tty := getProcessTTY(pid)
    if tty != "?" { return false }  // acquired a TTY
    protectedPIDs := getTmuxSessionPIDs()
    return !protectedPIDs[pid]
}
```

**Gas City:** Any "find X then kill X" pattern must re-verify X
between find and kill.

### Session alive but agent process dead

**Bug:** tmux session exists (HasSession returns true) but the Claude
process inside the pane has exited. Session is a zombie — appears
running but doing nothing. Hook stays attached forever.

**Fix:** `isSessionProcessDead()` — get pane PID from tmux, check if
process is actually alive via `Signal(0)`. If process dead, kill the
stale session and allow re-spawn.

**Gas City:** `session.IsRunning()` currently only checks tmux session
existence. Needs a deeper "is the agent process alive" check for
health patrol.

### Rollback pattern for failed multi-step operations

**Bug:** Creating a polecat requires: allocate name → create directory →
create worktree → create agent bead → start session. If step 4 fails,
steps 1-3 are orphaned.

**Fix:** Track resources created, define cleanup closure, call on error:

```go
var worktreeCreated bool
cleanupOnError := func() {
    _ = beads.ResetAgentBead(aid, "rollback")
    if worktreeCreated { _ = git.WorktreeRemove(path, true) }
    _ = os.RemoveAll(polecatDir)
    namePool.Release(name)
}
// ... each creation step ...
if err != nil { cleanupOnError(); return err }
```

**Gas City:** `createAgentWorktree` + `setupBeadsRedirect` is a
two-step operation. If redirect fails, the worktree is orphaned.
Should add rollback.

### Never delete lock files

**Bug:** Deleting a lock file creates a race: process A holds flock
on inode X. Process B deletes the file. Process C creates new file
(inode Y), acquires "lock" on inode Y. Now A and C both think they
hold the lock — different inodes, same path.

**Fix:** Never delete lock files. Flock is released on close. The
file stays on disk as a harmless empty sentinel.

**Gas City:** If we add file-based locking (controller.lock exists
already), never delete the lock file.

### Minimum orphan age prevents killing during startup

**Bug:** Agent process spawns, briefly has no TTY (before tmux
associates it). Orphan scan runs, sees process with TTY="?", kills
the brand-new agent.

**Fix:** `minOrphanAge = 60s`. Don't consider a process orphaned
unless it's been running for at least 60 seconds.

**Gas City:** Any orphan detection must have an age threshold.

### Don't assume CWD exists during cleanup

**Bug:** `gt done` merges and deletes worktree. Agent's shell session
still has CWD set to the deleted path. Shell breaks, subsequent
commands fail.

**Fix:** Store worktree path in tmux environment (`GT_POLECAT_PATH`)
at session start. `gt done` reads from env var, not from `os.Getwd()`.

**Gas City:** Always store paths in env vars at startup time. Never
rely on CWD surviving worktree operations.

## Gas Town reference files

| Concept | Gas Town file | Key function |
|---------|---------------|--------------|
| Worktree preserved on down | internal/cmd/down.go | runDown() |
| Auto-discovery on up | internal/polecat/manager.go:1289 | ReconcilePool() |
| Session stale check | internal/polecat/session_manager.go:448 | Start() |
| Agent bead registry | internal/polecat/manager.go:1621 | loadPolecatState() |
| Crew worktree (permanent) | internal/cmd/worktree.go:96 | runWorktree() |
| Zombie detection | internal/doctor/zombie_check.go | isSessionProcessDead() |
| Stale hook scan | internal/deacon/stale_hooks.go:75 | scanStaleHooks() |
| Worktree safety check | internal/deacon/stale_hooks.go:191 | checkUncommittedWork() |
| PID tracking | internal/session/pidtrack.go | WritePID() / CheckPID() |
| Startup beacon | internal/session/startup.go | FormatStartupBeacon() |
| Polecat spawn (full flow) | internal/polecat/manager.go | SpawnPolecatForSling() |
| Name allocation | internal/polecat/name_pool.go | AllocateName() / Release() |
| Hook bead (atomic assign) | internal/polecat/manager.go | hookBead() |
| Done flow | internal/cmd/done.go | runDone() |
| Self-nuke | internal/cmd/done.go | selfNukePolecat() |
| Done-intent label | internal/cmd/done.go | writeDoneIntent() |
| Witness post-merge | internal/polecat/witness.go | handleMergedMR() |
| Session wait-for-start | internal/polecat/session_manager.go | waitForProcess() |
