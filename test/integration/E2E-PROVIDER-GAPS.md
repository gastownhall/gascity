# E2E Test Suite: Docker & K8s Provider Gap Analysis

**Date:** 2026-02-28
**Baseline:** 52 E2E tests, all passing on subprocess provider
**Commit:** 5a2b7807 (Add E2E test suite)

## Executive Summary

One root cause — **agent scripts reference host paths not accessible
inside containers** — accounts for ~90% of all Docker and K8s failures.
The remaining failures are second-order effects of the same problem
(tmux server dies when the agent command fails, disabling metadata and
observation operations).

**Fix priority:** Solve the script path problem and nearly all tests
will pass on both providers.

---

## Test Results

### Subprocess (baseline): 49 PASS / 3 SKIP / 0 FAIL

### Docker: 18 PASS / 20 FAIL / 1 TIMEOUT / ~13 never ran

| Result | Count | Tests |
|--------|-------|-------|
| PASS | 18 | Drain_Ack, RequestRestart, Nudge, EventEmit, EventsQuery, EventsSince, Hook_NoWork, Hook_WithWork, Hook_Inject, Kill, StopGraceful, Handoff_Remote, MailSend, MailCheck, MailCheckInject, MailInbox, MailRead, MailArchive |
| FAIL | 20 | See root cause analysis below |
| TIMEOUT | 1 | Pool_SharedDir (panic, killed entire run) |
| Never ran | ~13 | EnvVars_*, Dir_*, Overlay, Hooks_*, PreStart, Suspended |

### K8s: 1 PASS / 3 FAIL / 1 TIMEOUT / ~48 never ran

| Result | Count | Tests |
|--------|-------|-------|
| PASS | 1 | RequestRestart |
| FAIL | 3 | Drain_SetAndCheck, Drain_Ack, Undrain |
| TIMEOUT | 1 | Nudge (panic at 10min, killed entire run) |
| Never ran | ~48 | Everything after Nudge |

---

## Root Cause Analysis

### RC-1: Agent script host paths not in container (CRITICAL)

**Impact:** 17+ Docker failures, nearly all K8s failures

`e2eReportScript()` and `e2eSleepScript()` resolve to absolute host
paths like `/data/projects/gascity/test/agents/e2e-report.sh`. The
Docker provider mounts `work_dir` (cityDir) and `GC_CITY` (cityDir)
into the container, but NOT the gascity source tree. The K8s provider
has no access to the host filesystem at all.

**Cascade effect:** When the agent command fails inside Docker:
1. `bash /data/projects/gascity/test/agents/stuck-agent.sh` exits
   immediately (file not found)
2. tmux session has no program to run, exits
3. tmux server shuts down (no remaining sessions)
4. Container stays alive via `tini -- sleep infinity` (PID 1 keepalive)
5. `IsRunning` returns true (checks `docker ps`, container IS running)
6. ALL tmux-dependent operations fail silently:
   - `SetMeta` / `GetMeta` → tmux not running → returns empty
   - `Peek` → no tmux pane to capture
   - `Nudge` → no tmux pane to send keys

**Evidence:**
```
$ docker exec gc-test-container ps aux
PID  COMMAND
  1  /sbin/docker-init -- sleep infinity
  8  sleep infinity
     (no tmux, no agent)

$ docker exec -e TMUX_TMPDIR=/run/gc-tmux gc-test-container tmux -u ls
no server running on /run/gc-tmux/tmux-0/default
```

**Tests affected (report-based):**
ConfigDrift, WorkspaceDefaults, AgentOverridesWorkspace, CustomProvider,
ProviderEnvMerge, SessionTemplate, Restart, SuspendResume_Agent,
SuspendResume_City, StartIdempotent, MultiAgent_Independent,
MultiAgent_PoolAndFixed, MultiAgent_CityAndRig, Pool_InstanceNaming,
Pool_SingletonNaming, Pool_WithDir, Pool_SharedDir

**Tests affected (metadata/observation cascade):**
Drain_SetAndCheck, Undrain, Peek

**Fix options (choose one):**

**Option A: Copy scripts into cityDir during test setup (recommended)**
```go
func copyAgentScripts(t *testing.T, cityDir string) {
    srcDir := filepath.Join(findModuleRoot(), "test", "agents")
    dstDir := filepath.Join(cityDir, ".gc", "scripts")
    os.MkdirAll(dstDir, 0o755)
    // copy e2e-report.sh, stuck-agent.sh
}

func e2eReportScript() string {
    return "bash .gc/scripts/e2e-report.sh"  // relative to workdir
}
```
Pro: No provider changes needed. Scripts live inside the mounted cityDir.
Con: Requires relative path support in commands.

**Option B: Mount source tree into Docker containers**
Add gascity source tree as a read-only mount in gc-session-docker:
```bash
vol_args+=(-v "/data/projects/gascity:/data/projects/gascity:ro")
```
Pro: Zero test changes.
Con: Hard-codes host path; doesn't fix K8s; fragile for CI.

**Option C: Inline simple commands instead of script paths**
```go
func e2eSleepScript() string { return "sleep 3600" }
func e2eReportScript() string {
    return `bash -c 'SAFE=${GC_AGENT//\//__}; mkdir -p ${GC_DIR}/.gc-reports; { echo STATUS=started; env | grep ^GC_ | sort; echo STATUS=complete; } > ${GC_DIR}/.gc-reports/${SAFE}.report; sleep 3600'`
}
```
Pro: No file dependencies at all.
Con: Inline scripts are hard to maintain; may exceed command length limits.

**Recommendation:** Option A. Copy scripts into the city's `.gc/scripts/`
directory during `setupE2ECity`. This mirrors how real deployments would
work — the city directory is the self-contained unit.

---

### RC-2: K8s startup timeout (~120s per pod) (HIGH)

**Impact:** Only 5 K8s tests ran before the 10-minute timeout killed the suite

K8s pod creation involves image pull, scheduling, readiness probes, and
tmux session setup. Each pod takes ~120 seconds (the exec provider's
start timeout). With 52 sequential tests, the suite would need ~100
minutes — far beyond the 10-minute test timeout.

**Fix:**
1. Increase test timeout for K8s: `-timeout 120m`
2. Pre-pull the image: `kubectl create daemonset` or `docker pull` first
3. Consider test parallelization for K8s (each test creates its own city)
4. K8s-specific subset: run fewer tests (e.g., one from each category)

---

### RC-3: Session lifecycle events not emitted with exec provider (MEDIUM)

**Impact:** 1 Docker failure (TestE2E_AgentLifecycleEvents)

`gc events --type session.woke` returns empty output after `gc start`.
The events may only be emitted by the controller loop (not one-shot
start), or the exec provider path in `doStart` may skip event recording.

**Investigation needed:** Check if `doStart` records `session.woke` events
for exec session providers. The one-shot path may return before events
are flushed.

---

### RC-4: Docker `gc start` timeout for report-based tests (LOW)

**Impact:** Timeout cascade for Pool_SharedDir (caused test suite panic)

Even with the script path fix, Docker container startup is slower than
subprocess (~8s vs ~5s). The `e2eDefaultTimeout` of 15 seconds may be
too tight for Docker when waiting for reports. Pool tests that start
multiple containers compound this.

**Fix:** Use a provider-aware timeout:
```go
func e2eTimeout() time.Duration {
    if usingSubprocess() { return 15 * time.Second }
    return 60 * time.Second
}
```

---

## Test-by-Test Docker Results

| Test | Result | Root Cause |
|------|--------|------------|
| Drain_SetAndCheck | FAIL | RC-1 (tmux dead → GetMeta empty) |
| Drain_Ack | PASS | SetMeta returns success even if tmux dead |
| Undrain | FAIL | RC-1 (tmux dead → GetMeta empty) |
| RequestRestart | PASS | Uses file-based metadata, no tmux needed |
| Nudge | PASS | Nudge is best-effort, returns nil on failure |
| Peek | FAIL | RC-1 (tmux dead → no pane to capture) |
| ConfigDrift | FAIL | RC-1 (e2e-report.sh not in container) |
| WorkspaceDefaults | FAIL | RC-1 |
| AgentOverridesWorkspace | FAIL | RC-1 |
| CustomProvider | FAIL | RC-1 |
| ProviderEnvMerge | FAIL | RC-1 |
| SessionTemplate | FAIL | RC-1 |
| EventEmit | PASS | CLI-only, no agent needed |
| EventsQuery | PASS | CLI-only |
| EventsSince | PASS | CLI-only |
| AgentLifecycleEvents | FAIL | RC-3 (events not emitted) |
| Hook_NoWork | PASS | CLI-only |
| Hook_WithWork | PASS | CLI-only |
| Hook_Inject | PASS | CLI-only |
| Kill | PASS | Container is running, kill works |
| StopGraceful | PASS | Stop/cleanup works on containers |
| Restart | FAIL | RC-1 (report needed after restart) |
| SuspendResume_Agent | FAIL | RC-1 (report needed after resume) |
| SuspendResume_City | FAIL | RC-1 (report needed after resume) |
| StartIdempotent | FAIL | RC-1 (report needed) |
| Handoff_Remote | PASS | CLI-only (mail + metadata) |
| MailSend | PASS | CLI-only |
| MailCheck | PASS | CLI-only |
| MailCheckInject | PASS | CLI-only |
| MailInbox | PASS | CLI-only |
| MailRead | PASS | CLI-only |
| MailArchive | PASS | CLI-only |
| MultiAgent_Independent | FAIL | RC-1 |
| MultiAgent_PoolAndFixed | FAIL | RC-1 |
| MultiAgent_CityAndRig | FAIL | RC-1 |
| Pool_InstanceNaming | FAIL | RC-1 |
| Pool_SingletonNaming | FAIL | RC-1 |
| Pool_WithDir | FAIL | RC-1 |
| Pool_SharedDir | TIMEOUT | RC-1 + RC-4 |
| Pool_EnvPerInstance | NOT RUN | Suite killed by timeout |
| EnvVars_CityScoped | NOT RUN | |
| EnvVars_Custom | NOT RUN | |
| Dir_Default | NOT RUN | |
| Dir_Relative | NOT RUN | |
| Dir_GC_DIR | NOT RUN | |
| Overlay | NOT RUN | |
| Hooks_Gemini | NOT RUN | |
| Hooks_Claude | NOT RUN | |
| Hooks_WorkspaceDefault | NOT RUN | |
| Hooks_AgentOverride | NOT RUN | |
| PreStart | NOT RUN | |
| Suspended | NOT RUN | |

---

## Predicted Results After Fixing RC-1

If agent scripts are made available inside containers (Option A), the
predicted Docker results would be:

| Category | Before | After (predicted) |
|----------|--------|-------------------|
| PASS | 18 | ~46 |
| FAIL | 20 | ~3 (RC-2, RC-3) |
| SKIP | 0 | 0 |
| TIMEOUT | 1 | 0 |
| NOT RUN | 13 | 0 |

Remaining failures would be:
- AgentLifecycleEvents (RC-3: investigate event emission for exec providers)
- Drain_SetAndCheck, Undrain (if tmux stays alive with working scripts)

---

## Action Items

1. **[P0] Fix agent script paths for container providers**
   Copy test scripts into cityDir during setupE2ECity. Use relative
   paths in start_command. This unblocks ~35 tests across Docker and K8s.

2. **[P1] Add provider-aware timeouts**
   Docker and K8s are 2-10x slower than subprocess. Scale timeouts
   by provider type.

3. **[P1] Increase K8s test timeout**
   Use `-timeout 120m` for K8s runs, or create a K8s-specific test
   subset.

4. **[P2] Investigate `session.woke` event emission for exec providers**
   May need to record events in the one-shot `doStart` path.

5. **[P2] Consider test parallelization**
   Independent cities allow `t.Parallel()` for non-interfering tests.
