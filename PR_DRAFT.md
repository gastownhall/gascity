## Summary

Replaces the unbounded `syscall.Flock(LOCK_EX)` in `events.FileRecorder.Record` with a non-blocking retry loop bounded by a 250 ms budget at a 5 ms cadence. Prevents the deadlock class where a dead writer that held the advisory file lock causes every subsequent `gc event emit` to block forever, piling up hundreds of stuck processes.

## What the bug looks like in production

`FileRecorder.Record` opens `<city>/.gc/events.jsonl` with `O_APPEND`, takes a `LOCK_EX`, writes one JSON line, and unlocks. The lock is correctly paired with a deferred `LOCK_UN`. If the holder is killed (`SIGKILL`, panic mid-syscall, `gc magi doctor` SIGTERM during reconciliation, etc.), the kernel reaps the holder asynchronously — until it does, every subsequent `Record` blocks indefinitely. The function's docstring states *"Recording is best-effort: errors are logged to stderr but never returned to callers"* — blocking forever violates that contract.

Observed symptoms on macOS Darwin 25.4.0 (M-series host, 512 GB RAM):

- 200+ stuck `gc event emit bead.<verb> --subject te-<id>` processes (each on a single CPU thread but waiting on flock).
- System CPU pegged at 88 % `system`, 12 % `user`, 0 % `idle` — scheduler/lock-contention thrash, not user computation.
- `gc supervisor` stuck in a restart loop: `starting_bead_store took 1m36.31s` → `exec beads init: signal: killed (skipping)` → `init failure #1, next retry in 10s`, repeating.
- Recovery required manual `pkill -9 -f "gc event emit"`.

The chain of causation:

1. A bd-heavy workflow (multi-step pipeline, `bd magi doctor`, `gastown.dog` order sweeps, etc.) fires many `gc event emit` calls in rapid succession.
2. The first emit acquires the flock and starts to write.
3. Something kills that process between `LOCK_EX` and `LOCK_UN` (SIGKILL from `KILL_CITY.sh`, supervisor restart, OOM, etc.).
4. The kernel keeps the lock on the open-file-description while the dying process is reaped.
5. Every subsequent `gc event emit` blocks at `LOCK_EX` waiting for the dead holder to be fully torn down.
6. bd-hook-driven emits keep firing, each spawning a new blocked process.
7. The supervisor's own bd operations are also blocked, so it concludes `bd init` is hung and SIGKILLs it — adding more dead lock holders.

## What the patch does

In `internal/events/recorder.go` only:

1. Adds two unexported `time.Duration` constants in a single `const (...)` block: `recordFlockTimeout = 250 * time.Millisecond` and `recordFlockRetryInterval = 5 * time.Millisecond`. Both carry inline rationale comments.
2. Adds `errors` to the stdlib import group.
3. Updates the `FileRecorder` type doc-comment to state that cross-process serialization is enforced by a *bounded-wait* advisory file lock (previously the comment said only "advisory file lock").
4. Replaces the single blocking `syscall.Flock(int(r.file.Fd()), syscall.LOCK_EX)` call in `Record()` with a bounded retry loop:

```go
fd := int(r.file.Fd())
deadline := time.Now().Add(recordFlockTimeout)
for {
    err := syscall.Flock(fd, syscall.LOCK_EX|syscall.LOCK_NB)
    if err == nil {
        break
    }
    if !errors.Is(err, syscall.EWOULDBLOCK) && !errors.Is(err, syscall.EAGAIN) {
        fmt.Fprintf(r.stderr, "events: lock: %v\n", err)
        return
    }
    if time.Now().After(deadline) {
        fmt.Fprintf(r.stderr, "events: lock: timed out after %dms waiting on flock at %s\n", recordFlockTimeout.Milliseconds(), r.path)
        return
    }
    time.Sleep(recordFlockRetryInterval)
}
```

5. The deferred `LOCK_UN` block is untouched. It runs only after `break` exits the loop with `err == nil`, which is the only path that leaves a held lock.

Design notes:

- **Fixed 5 ms cadence over exponential backoff.** Sub-millisecond release latency means a healthy holder is observed free within one cadence; uniform timing also simplifies test assertions. Exponential backoff adds complexity with no contention-scale benefit.
- **In-process callers serialize on `r.mu` first**, so the flock loop never spins for an in-process peer. The bounded wait targets cross-process contention only. A one-line comment in the code states this so future readers do not optimize prematurely.
- **`errors.Is` against both `EWOULDBLOCK` and `EAGAIN`.** On Linux they share a numeric value; on macOS/BSD they are distinct. Both name checks are portable and idiomatic.
- **Stderr message text is grep-stable.** The new timeout line keeps the `events: lock:` prefix shared with the existing error line so log scrapers and ops dashboards continue to match.

## Test coverage

`internal/events/events_test.go` (existing file, white-box `package events`) gains three new tests and one helper, appended at the end:

- `mustOpenSiblingLock(t, path)` — opens a separate `*os.File` against the same path with `O_WRONLY|O_APPEND` flags and takes a non-blocking exclusive flock on it. The separate open file description is what lets the kernel-level flock contend with the recorder's own FD.
- `TestFileRecorderFlockTimeoutFiresWithinBudget` — sibling holds the lock for the entire test; asserts elapsed ∈ `[recordFlockTimeout, recordFlockTimeout + 100ms]`, stderr contains `events: lock: timed out`, `waiting on flock at`, and the recorder path.
- `TestFileRecorderFlockSucceedsAfterShortContention` — sibling releases via `time.AfterFunc(50*time.Millisecond, ...)`; asserts elapsed ∈ `[40ms, recordFlockTimeout)` and stderr is empty.
- `TestFileRecorderFlockSucceedsWithoutContention` — no sibling lock; asserts elapsed `< 50 ms` and stderr is empty.

Existing tests in the events package — including `TestFileRecorderConcurrentSafe`, which spawns 10 × 10 in-process `Record` calls on a single recorder — pass unchanged because in-process callers serialize on `r.mu` BEFORE reaching the flock and therefore never observe contention at the kernel layer.

## How to try it out

```bash
git clone -b fix/events-flock-bounded-wait https://github.com/gastownhall/gascity.git
cd gascity

# Build + format + vet
go build ./...
gofmt -l internal/events/recorder.go internal/events/events_test.go      # empty (no drift)
go vet ./...                                                              # clean

# Run the three new tests in isolation
go test -race -v -run TestFileRecorderFlock ./internal/events/...

# Run them five times under the race detector to surface latent flakes
go test -race -count=5 -run TestFileRecorderFlock ./internal/events/...

# Run the full events package under the race detector
go test -race ./internal/events/...
```

To exercise the fix end-to-end on a live machine:

```bash
go install ./cmd/gc
# `which gc` must now resolve to $GOBIN/gc (or $GOPATH/bin/gc)

# In one terminal — hold the flock manually
python3 - <<'PY'
import fcntl, os, time, pathlib
p = pathlib.Path.home() / ".gc" / "events.jsonl"
p.parent.mkdir(parents=True, exist_ok=True)
p.touch()
f = open(p, "ab")
fcntl.flock(f.fileno(), fcntl.LOCK_EX)
print("holding lock; press ctrl-c to release")
time.sleep(60)
PY

# In a second terminal — start a wall clock and call gc event emit
time gc event emit test --subject lock-probe
# Old binary: hangs indefinitely.
# New binary: returns within ~250 ms with stderr "events: lock: timed out after 250ms..."
```

## Test plan

- [x] `gofmt -l internal/events/recorder.go internal/events/events_test.go` — empty
- [x] `go vet ./...` (module-wide) — zero output
- [x] `go build ./...` — zero errors
- [x] `go test -race -v -run TestFileRecorderFlock ./internal/events/...` — 3/3 PASS
- [x] `go test -race -count=5 -run TestFileRecorderFlock ./internal/events/...` — 5×3 PASS
- [x] `go test -race ./internal/events/...` — full package green, no regressions
- [x] `go install ./cmd/gc`; `which gc` resolves to `$GOPATH/bin/gc`; `gc version` prints `1.1.1`; live `gc magi doctor` end-to-end returns rc=0 with zero WARNINGs
- [x] Live test: a manually held `LOCK_EX` on `~/.gc/events.jsonl` causes the new `gc event emit` to return within `recordFlockTimeout`, while the old binary blocks indefinitely

## Notes for reviewers

- The fix is a pure defensive timeout. The contract of `Record` is unchanged: errors still log to stderr, the function still never returns. Callers cannot tell the difference between "wrote successfully" and "lock timed out" by return value — that is intentional and consistent with the best-effort contract documented at the top of `events.go`.
- No API surface widened. `recordFlockTimeout` and `recordFlockRetryInterval` are unexported. No new constructor argument, no new public function, no new interface method.
- No new dependencies. All additions are stdlib (`errors` already widely used in the file; was not previously imported here because `Record` only reported errors verbatim via `%v`).
- The 250 ms budget is a single trade-off knob. It is well above expected sub-millisecond release latency under healthy contention and well below user-perceptible stall. Changing it is a one-line edit at the constant.
