---
title: "Bug: macOS Test Suite Failures on darwin/arm64"
type: satellite-issue
epic: 000-epic-cross-machine-city
status: confirmed
component: tests, runtime, mail, config
current_state: broken
priority: high
author: trillium
date: 2026-03-21
labels: [bug, tests, macos, platform]
---

# Bug: macOS Test Suite Failures on darwin/arm64

## Summary

`go test ./...` produces **39 individual test failures across 9 packages** on
macOS (darwin/arm64). These are all pre-existing — they reproduce on a clean
checkout of `main` with no local modifications.

35 packages pass; 9 fail. All failures fall into five root causes, none of
which are related to application logic bugs.

## How to Observe

```bash
cd /path/to/gascity
go test ./...
```

**Environment where failures reproduce:**

| Property | Value |
|----------|-------|
| OS | macOS 25.3.0 (Darwin Kernel 25.3.0, arm64) |
| Go | go1.26.0 darwin/arm64 |
| Machine | Mac mini (T8103) |
| `sun_path` max | 104 bytes (kernel limit) |
| `flock` | **not installed** |
| `sed` | BSD sed (not GNU) |

## Root Causes

### 1. Unix Socket Path Too Long (18 tests)

**Packages:** `cmd/gc`, `internal/runtime/acp`, `internal/runtime/subprocess`

**Error pattern:**
```
listen unix /var/folders/95/hsqnjv0j74scn126v_5980ch0000gn/T/TestName.../001/socks/name.sock: bind: invalid argument
```

**Root cause:** macOS limits `sun_path` (the Unix domain socket path) to 104
bytes. Go's `t.TempDir()` returns paths under `/var/folders/...` which are
already ~70 bytes before the test adds subdirectories and socket filenames.
The resulting paths exceed 104 bytes and `bind(2)` returns `EINVAL`.

**Affected tests (18):**
- `cmd/gc`: `TestControllerShutdown`, `TestControllerPokeTriggersImmediate`,
  `TestTutorial01` (controller.txtar), `TestNewSessionProviderByNameSubprocessUsesCityScopedDir`,
  `TestNewSessionProviderByNameSubprocessAllowsSameSessionNameAcrossCities`
- `internal/runtime/acp`: `TestStart_HandshakeSuccess`, `TestStart_DuplicateReturnsError`,
  `TestStart_HandshakeTimeout`, `TestStop_MakesSessionNotRunning`,
  `TestNudge_SendsPrompt`, `TestPeek_ReturnsOutput`,
  `TestGetLastActivity_UpdatedOnOutput`, `TestClearScrollback_ClearsBuffer`,
  `TestMeta_RoundTrip`, `TestListRunning_FindsSessions`,
  `TestStderrCaptured_InHandshakeError`
- `internal/runtime/subprocess`: `TestStartDuplicateNameFails`,
  `TestIsRunningFalseAfterExit`, `TestEnvPassedToProcess`,
  `TestSocketRemovedAfterStop`, `TestSocketGoneAfterProcessDeath`,
  `TestCrossProcessStopBySocket`, `TestCrossProcessInterruptBySocket`,
  `TestListRunningViaSocket`

**Fix direction:** Use a short temp directory (e.g., `/tmp/gc-test-XXXX`) for
socket creation instead of `t.TempDir()`, or shorten the socket path within
the temp dir by hashing the test name.

### 2. macOS `/private` Symlink Mismatch (7 tests)

**Packages:** `cmd/gc`, `internal/convergence`, `internal/git`, `internal/supervisor`

**Error pattern:**
```
expected path /var/folders/.../my-city
got /private/var/folders/.../my-city
```

**Root cause:** On macOS, `/var` is a symlink to `/private/var`. `t.TempDir()`
returns the logical path (`/var/...`), but some operations resolve the symlink
and return the physical path (`/private/var/...`). Tests that compare these
paths with string equality fail.

**Affected tests (7):**
- `cmd/gc`: `TestDoRegister`, `TestRegisterCityWithSupervisorWaitsForConfiguredStartupTimeout`,
  `TestUnregisterCityFromSupervisorRestoresRegistrationOnReloadFailure`,
  `TestResolveCityFlag/flag_empty_fallback`
- `internal/convergence`: `TestResolveConditionPath` (both subtests)
- `internal/git`: `TestWorktreeList`
- `internal/supervisor`: `TestRegistryRegisterAndList`, `TestRegistryMultipleCities`

**Fix direction:** Canonicalize paths with `filepath.EvalSymlinks()` before
comparison, or use `t.TempDir()` through `filepath.EvalSymlinks()` at test
setup.

### 3. Missing `flock` Binary (1 test)

**Package:** `cmd/gc`

**Error pattern:**
```
flock is required but not installed. Install: brew install flock (macOS) or apt install util-linux (Linux)
```

**Root cause:** The `gc-beads-bd` shell script requires `flock` for lock
management. macOS does not ship `flock` — it requires `brew install flock`.
The test `TestGcBeadsBdStartUsesRootBeadsDataDir` exercises this script and
fails when `flock` is absent.

**Affected test:** `TestGcBeadsBdStartUsesRootBeadsDataDir`

**Fix direction:** Either skip the test when `flock` is not available
(`exec.LookPath("flock")`), or remove the `flock` dependency from the
script (use `mkdir`-based locking or Go-level `syscall.Flock`).

### 4. BSD `sed` Incompatibility (13+ tests)

**Package:** `internal/mail/exec`

**Error pattern:**
```
sed: 1: "/var/folders/95/hsqnjv0 ...": invalid command code f
```

**Root cause:** The mail provider shell scripts use GNU `sed` in-place
syntax (`sed -i "..."`) which is incompatible with BSD `sed` on macOS.
BSD `sed -i` requires an explicit backup extension argument (`sed -i '' "..."`).
The path itself is being interpreted as a sed command.

**Affected tests:** All `TestExecConformance` and `TestMCPMailConformance`
subtests (13+ individual failures across the conformance suite).

**Fix direction:** Use `sed -i '' ...` (portable to both BSD and GNU with the
empty-string backup extension), or replace `sed` with a pure-shell or Go
implementation.

### 5. Shell Script Bash Array Syntax (13+ tests)

**Package:** `internal/mail/exec`

**Error pattern:**
```
gc-mail-mcp-agent-mail: line 143: auth_header[@]: unbound variable
```

**Root cause:** The MCP mail agent script uses Bash array syntax
(`${auth_header[@]}`) under `set -u` (nounset). When the array is
uninitialized, Bash treats it as an unbound variable and aborts. This
affects all `TestMCPMailConformance` subtests.

**Affected tests:** All `TestMCPMailConformance` subtests (13+ failures).

**Fix direction:** Initialize the array before use (`auth_header=()`), or
use `${auth_header[@]+"${auth_header[@]}"}` for safe expansion under
`set -u`.

### 6. Test Count Drift (1 test)

**Package:** `examples/gastown`

**Error pattern:**
```
found 11 formula files, want 7
```

**Root cause:** `TestAllFormulasExist` hard-codes an expected formula count
(7) that no longer matches reality (11 formula files exist). New formulas
were added without updating the test assertion.

**Affected test:** `TestAllFormulasExist`

**Fix direction:** Update the expected count, or change the test to validate
formula existence without a hard-coded count.

### 7. Tool Detection False Positive (2 tests)

**Package:** `internal/api`

**Error pattern:**
```
claude status = "needs_auth", want "not_installed"
github_cli status = "needs_auth", want "not_installed"
```

**Root cause:** The readiness handler tests expect `not_installed` status
when the binary is absent, but the detection logic finds the installed
binary on this machine and returns `needs_auth` instead. The tests assume
the tools are not installed system-wide.

**Affected tests:** `TestHandleProviderReadinessReturnsNotInstalledWhenBinaryMissing`,
`TestHandleReadinessReturnsNotInstalledForGitHubCLIWithoutBinary`

**Fix direction:** Override `PATH` in these tests to ensure the binary is
genuinely not found, or mock the binary lookup.

## Full Failing Package List

| Package | Failures | Root Causes |
|---------|----------|-------------|
| `cmd/gc` | 10 | Socket path (#1), `/private` symlink (#2), missing flock (#3) |
| `internal/runtime/acp` | 11 | Socket path (#1) |
| `internal/runtime/subprocess` | 9 | Socket path (#1) |
| `internal/mail/exec` | 2 suites | BSD sed (#4), bash arrays (#5) |
| `internal/supervisor` | 2 | `/private` symlink (#2) |
| `internal/convergence` | 1 | `/private` symlink (#2) |
| `internal/git` | 1 | `/private` symlink (#2) |
| `examples/gastown` | 1 | Count drift (#6) |
| `internal/api` | 2 | Tool detection (#7) |

**Total: 39 individual test failures across 9 packages.**
