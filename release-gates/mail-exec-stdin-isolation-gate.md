# Release Gate: mail exec stdin isolation

Bead: ga-tsvwf7
Source review bead: ga-k4jzoi
Branch: fix/mail-exec-stdin-isolation
Reviewed commit: 4d8ee225bc7d9d611f90efcd9552e9d83434c8f6
Base: origin/main at 0dae71e3b33218250a23a3c35b889e325aaa9fe7
Gate run: 2026-06-08

## Summary

The reviewed change updates `internal/mail/exec.Provider.run` so every exec
provider subprocess gets an explicit stdin reader. `nil` input now becomes an
empty reader that returns EOF, preventing scripts from inheriting the caller's
stdin during archive/delete/check/read operations. Non-empty stdin paths still
use the same `bytes.NewReader(stdinData)` behavior as before.

## Criteria

| # | Criterion | Result | Evidence |
|---|-----------|--------|----------|
| 1 | Review PASS present | PASS | `bd show ga-k4jzoi` shows the review bead closed with `VERDICT: PASS`. |
| 2 | Acceptance criteria met | PASS | `git diff origin/main..HEAD -- internal/mail/exec/exec.go` shows `cmd.Stdin = bytes.NewReader(stdinData)` is unconditional, with no other files changed. This satisfies the stdin-isolation fix while preserving non-nil stdin behavior. |
| 3 | Tests pass | PASS | `go test ./internal/mail/exec/...` passed. `go vet ./internal/mail/exec/...` passed. `make test-fast-parallel` passed all fast shards. `go vet ./...` passed. |
| 4 | No high-severity review findings open | PASS | Review notes for `ga-k4jzoi` list security/correctness/style as PASS and no HIGH findings. The only noted gaps are follow-up tests tracked in `ga-0mhj1r`. |
| 5 | Final branch is clean | PASS | `git status --short --branch` was clean on `fix/mail-exec-stdin-isolation` before adding this gate file. |
| 6 | Branch diverges cleanly from main | PASS | `git merge-base HEAD origin/main` equals `origin/main` and `git merge-tree --write-tree origin/main HEAD` completed cleanly. |
| 7 | Single feature theme | PASS | Commit set is one commit touching only `internal/mail/exec/exec.go`; the change is a single mail exec provider stdin-isolation fix. |

## Commands

```text
go test ./internal/mail/exec/...
ok  	github.com/gastownhall/gascity/internal/mail/exec	20.574s

go vet ./internal/mail/exec/...
PASS

make test-fast-parallel
[unit-cmd-gc-1-of-6] ok
[unit-core] ok
[unit-cmd-gc-2-of-6] ok
[unit-cmd-gc-3-of-6] ok
[unit-cmd-gc-5-of-6] ok
[unit-cmd-gc-4-of-6] ok
[unit-cmd-gc-6-of-6] ok
All fast jobs passed

go vet ./...
PASS

git merge-tree --write-tree origin/main HEAD
PASS
```
