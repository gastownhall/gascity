# Release Gate — ga-dnf3 (reconciler deadline_exceeded masking fix)

**Bead:** ga-rhw8 (fix) via review bead ga-dnf3
**Branch:** `release/ga-dnf3`
**Source commit:** 7e71fd4c (builder branch) → cherry-picked onto `origin/main` as b961769d (issues.jsonl stripped per deployer EXCLUDES discipline)
**Evaluator:** gascity/deployer-1 on 2026-04-23

## Gate criteria

| # | Criterion | Verdict | Evidence |
|---|-----------|---------|----------|
| 1 | Review PASS present | PASS | ga-dnf3 notes: `review_verdict: pass` from gascity/reviewer. Single-pass (gemini second-pass disabled). |
| 2 | Acceptance criteria met | PASS | See matrix below. |
| 3 | Tests pass | PASS | `go test ./cmd/gc -run "TestReconcile|TestCommitStart|TestStartPreparedStart|TestExecutePreparedStartWave|TestSessionLifecycle|TestCandidate" -count=1` → `ok 2.586s` on `release/ga-dnf3`. |
| 4 | No high-severity review findings open | PASS | Both findings in ga-dnf3 are `severity: info` (log-message accuracy, ErrSessionInitializing+deadline classification change — reviewer noted neither is blocking). |
| 5 | Final branch is clean | PASS | `git status` clean (no tracked modifications). |
| 6 | Branch diverges cleanly from main | PASS | Cut fresh from `origin/main`, one commit. No merge conflicts. `issues.jsonl` from the source commit was stripped during cherry-pick (doesn't exist on main, would cause add/delete conflicts). |

## Acceptance criteria matrix (ga-rhw8 scope)

| Criterion | Met | Evidence |
|-----------|-----|----------|
| Switch ordering: deadline → canceled → err==nil | YES | `cmd/gc/session_lifecycle_parallel.go:523` reordered so `startCtx.Err()` branches precede `err == nil`. |
| Nil err promoted to wrapped context error in deadline/canceled branches | YES | Lines 525, 529 wrap context sentinels so `result.err != nil` downstream, triggering `commitStartResultTraced` to record failure and clear `last_woke_at`. |
| New reproducer test | YES | `cmd/gc/session_lifecycle_start_deadline_test.go` exercises `ctxIgnoringStartProvider` that holds past the deadline and returns nil; asserts `outcome=deadline_exceeded` and `errors.Is(err, context.DeadlineExceeded)`. |
| Scope discipline: single file + one new test, no unrelated touches | YES | This commit touches only `session_lifecycle_parallel.go` and adds `session_lifecycle_start_deadline_test.go`. |

## Test evidence

```
$ go test ./cmd/gc -run "TestReconcile|TestCommitStart|TestStartPreparedStart|TestExecutePreparedStartWave|TestSessionLifecycle|TestCandidate" -count=1
ok github.com/gastownhall/gascity/cmd/gc 2.586s

$ go vet ./cmd/gc/...
(clean)

$ go build ./...
(clean)
```

## Security review

From ga-dnf3, OWASP Top 10 walkthrough: pure control-flow reordering plus `fmt.Errorf` wrap using canonical `context` sentinels. No new I/O, no new auth/access surface, no deserialization. A10 (logging) improves: failures previously tagged silent success are now explicit deadline_exceeded.

## Verdict: PASS

Cleared for PR.
