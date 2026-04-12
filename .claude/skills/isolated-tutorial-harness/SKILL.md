---
name: isolated-tutorial-harness
description: Run or debug the Gas City tutorial acceptance harness in this repo when you need the coding-agent CLIs, supervisor, and city state isolated in temp homes while still authenticating Claude and Codex correctly.
---

# Isolated Tutorial Harness

Use this skill when working in `gascity` on:
- `test/acceptance/tutorial_goldens`
- `test/acceptance/helpers`
- provider-readiness or auth/isolation regressions that affect tutorial tests

Do not use this skill for generic `go test ./...` work. It is specifically for the tutorial acceptance harness and its provider/auth boundary.

## Invariants

- Keep `gc` runtime isolated:
  - temp `HOME`
  - temp `GC_HOME`
  - temp `XDG_RUNTIME_DIR`
  - temp supervisor and tmux state
- Do not borrow the host `HOME` just to make `claude` work.
- Use a real `CLAUDE_CODE_OAUTH_TOKEN` loaded from repo-local `.env`.
- Keep `.env` local only. It is gitignored and must never be printed into chat, logs, patches, or commits.

The relevant implementation points are:
- `test/acceptance/tutorial_goldens/main_test.go`
- `internal/api/handler_provider_readiness.go`
- `.gitignore`

## Required Local Setup

The harness expects a repo-local `.env` with:

```bash
CLAUDE_CODE_OAUTH_TOKEN=...
```

Optional:

```bash
OPENAI_API_KEY=...
```

`main_test.go` loads `.env` at package startup. If the token is missing or invalid, the tutorial harness will either skip or fail in ways that look like provider/auth problems.

## Workflow

1. Confirm branch state and local diffs:

```bash
git status --short --branch
```

2. Verify the isolated auth plumbing and readiness probes first:

```bash
go test ./test/acceptance/helpers -run 'TestProviderShim|TestEnsureClaude' -count=1
go test ./internal/api -run 'TestProbeCommandEnv(PreservesXDGOverridesWhenGHConfigDirIsSet|PassesClaudeOAuthToken)$|TestHandleProviderReadinessAcceptsClaudeOAuthTokenAuth' -count=1
```

3. Run targeted tutorial tests before the full package:

```bash
go test -tags acceptance_c ./test/acceptance/tutorial_goldens -run '^TestTutorial01Cities$' -count=1 -v
go test -tags acceptance_c ./test/acceptance/tutorial_goldens -run '^TestTutorial04Communication$' -count=1 -v
go test -tags acceptance_c ./test/acceptance/tutorial_goldens -run '^TestTutorial03Sessions$' -count=1 -v
```

Why these first:
- Tutorial 01 proves isolated Claude init plus real rig-local work.
- Tutorial 04 proves the mayor can do useful Claude work in the isolated environment.
- Tutorial 03 is the suite-level timing canary for session/log behavior.

4. If those pass, run the full package:

```bash
go test -tags acceptance_c ./test/acceptance/tutorial_goldens -count=1
```

## How To Interpret Failures

### `claude auth status --json` says `loggedIn: false`

This is still an isolation/auth problem, not a tutorial-content problem.

Check:
- `.env` exists at repo root
- `CLAUDE_CODE_OAUTH_TOKEN` is present and valid
- `main_test.go` is still passing `CLAUDE_CODE_OAUTH_TOKEN` into the temp env
- readiness probe env still includes the token in `probeCommandEnv(...)`

### Tutorial 01 fails in `gc init --provider claude`

This usually means readiness rejected Claude auth, not that tutorial prose is wrong.

Check:
- `probeClaude(...)` still accepts first-party `oauth_token`
- `probeCommandEnv(...)` still passes the OAuth token through

### Tutorial 03 passes alone but fails in the full package

Treat that as suite-level timing/state coupling, not an auth regression.

Check:
- `gc session list`
- `gc session peek mayor`
- `gc session logs mayor`
- the diagnostics dumped by the tutorial harness

If individual Tutorial 03 is green but the full package fails, the next target is session timing or page-driver assumptions, not Claude auth.

## What Not To Do

- Do not wrap `claude` so it runs against the host `HOME`.
- Do not add host transcript roots or host observe paths to make the tests pass.
- Do not trust `claude auth status --json` alone for token validation; a bogus non-empty token can look logged-in but still fail real requests.
- Do not print or commit the token.

## Files To Inspect When Updating This Flow

- `test/acceptance/tutorial_goldens/main_test.go`
- `internal/api/handler_provider_readiness.go`
- `internal/api/handler_provider_readiness_test.go`
- `test/acceptance/helpers`
- `.gitignore`

## Exit Criteria

The harness is in good shape when all of these are true:
- helper/auth probe tests pass
- Tutorial 01 passes in isolation
- Tutorial 04 passes in isolation
- Tutorial 03 passes in isolation
- the full `tutorial_goldens` package passes without borrowing host Claude state
