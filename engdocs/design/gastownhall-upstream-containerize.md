---
title: "gastownhall-upstream: containerize run-tests for CI-parity env isolation"
---

| Field | Value |
|---|---|
| Status | Proposed |
| Date | 2026-04-21 |
| Author(s) | foundations/worker-1 |
| Issue | `fo-upstream-formula-containerize` (parent: `fo-upstream-epic`) |
| Supersedes | N/A |

## Summary

The `gastownhall-upstream` formula's `run-tests` step currently runs
`make check` in the worker agent's own environment. Agent env inherits
city-controller state (`GC_CITY_PATH`, `GC_DOLT_*`, `GC_ALIAS`, …) that
CI does not have. This divergence is why we needed PR #746 (Makefile
`env -i` wrap + `internal/testenv` scrub): two layered workarounds for a
root cause we never addressed — the test harness and the live agent
share a process environment.

Phase 1 moves `make check` into an ephemeral Docker container so the
test environment matches CI by construction. Specifically:

- The `run-tests` step invokes `docker run` against a test image built
  from `contrib/k8s/Dockerfile.base` (already `ubuntu:24.04`, aligned
  with CI's `runs-on: ubuntu-latest`) plus a thin test layer that adds
  go, `bd`, and tool binaries at the `deps.env`-pinned versions.
- The container bind-mounts the host worktree checked out at
  `upstream-test-ref` read-only, runs `make check`, streams stdout to
  the formula log, exits.
- Docker missing → hard-fail with a mayor mail. No host fallback — that
  would re-introduce the divergence this bead exists to fix.
- The existing `rebuild-integration` step stays on host (it needs git +
  the `cwalv` remote, which are host-side concerns) and produces the
  worktree the container will bind-mount. Phase 2 revisits this split.

Out of scope for Phase 1 (tracked as follow-on work below):
- Renaming `integration-refs.toml` → `deploy-refs.toml` or splitting the
  deploy-branch from test-integration. Phase 2.
- Publishing the test image to a registry. Local image only for Phase 1.
- Changing formulas other than `gastownhall-upstream.formula.toml`.
- Migrating any currently in-flight tier-4 molecules. Operator has
  already parked them; they re-pour cleanly under the new formula after
  Phase 1 lands.

## Problem

`gastownhall-upstream.formula.toml`'s `run-tests` step is:

```bash
git checkout upstream-test-ref
make check
TEST_RC=$?
git checkout "$FEATURE_BRANCH"
```

`make check` executes in the pool-worker agent's shell. That shell
inherits:

- `GC_ALIAS`, `GC_SESSION_NAME`, `GC_TEMPLATE`, `GC_CITY_PATH`
- `GC_DOLT_HOST`, `GC_DOLT_PORT`, `GC_DOLT_USER`, `BEADS_DOLT_*`
- Other city/session state the agent runtime exports.

CI (per `.github/workflows/ci.yml`, job `check`) exports none of these.
It starts from a GitHub Actions runner with go, dolt, bd, tmux, jq,
Claude CLI freshly installed and otherwise empty env.

Two concrete recent failure modes from this divergence:

1. **`TestTutorial01/mail` passes locally, fails in a clean env** when
   `GC_ALIAS` leaks into a child `gc mail send` and the subprocess
   reports `FROM=human` instead of `mayor`. Surfaced on PR #746's own
   branch during the `fo-spawn-storm` tier-4 ship retry
   (2026-04-21 07:55Z note on `fo-mol-ff6j`).
2. **Four k8s-manifest tests pass in CI, fail locally** because they
   assume `GC_DOLT_*` / `GC_CITY_PATH` are unset, and only the Makefile
   `env -i` wrap (PR #746 commit 1) plus `internal/testenv` scrub
   (commits 2–3) papered over the leak.

Both scrubbing layers exist because the host environment is wrong. The
right fix is to run tests in a host that is right by construction.

## Proposal

### Phase 1: containerize `run-tests`

The `run-tests` step's `make check` is the only thing changing in
Phase 1. The surrounding steps (`validate-worktree`,
`rebuild-integration`, `push-branch`, `open-pr`, …) stay on host and
unchanged.

New artifacts Phase 1 adds to the gascity repo:

- `contrib/upstream-container/Dockerfile.test` — the test image
  definition. `FROM gc-agent-base:latest`, add go at the version in the
  CI runner's `actions/setup-go`, add `bd` at `deps.env`'s
  `BD_VERSION`, run `make install-tools` so the resulting image has
  `golangci-lint` and `oapi-codegen` pinned.
- `make upstream-test-image` target — builds the image, tagged
  `gc-upstream-test:<deps-hash>`. `<deps-hash>` is `sha256(deps.env +
  Dockerfile.test)` truncated to 12 hex chars. Latest tag
  `gc-upstream-test:latest` also points at it.
- `scripts/upstream-test-run` (new) — wraps `docker run` with the
  bind-mount + env-scrub conventions the formula expects. Keeps the
  formula step bash short and testable.

The formula's `run-tests` step becomes:

```bash
# Host-side preamble (regression-tier gate stays on host — it reads
# TESTING.md + git diff, no tests run yet).
TEST_FILES_CHANGED=$(git diff --name-only "origin/{{base_branch}}..HEAD" \\
  | grep -E '_test\\.go$|\\.txtar$|_test\\.py$|\\.test\\.ts$|\\.spec\\.ts$' || true)
if [ "{{regression_required}}" = "true" ] && [ -z "$TEST_FILES_CHANGED" ]; then
  echo "FATAL: no test files in the commits ahead of origin/{{base_branch}}"
  gc mail send mayor -s "BLOCKED: submission {{issue}} missing regression test" \\
    -m "gastownhall-upstream refusing to submit — no test file modified..."
  exit 1
fi

# Docker preflight — hard-fail if unavailable.
if ! command -v docker >/dev/null 2>&1; then
  echo "FATAL: docker not found on host. gastownhall-upstream requires"
  echo "       docker to run make check in a CI-parity environment."
  gc mail send mayor -s "BLOCKED: docker missing on {{rig}} — containerized run-tests cannot run" \\
    -m "Install docker per contrib/upstream-container/README.md, or revert this formula to the pre-containerize shape. See fo-upstream-formula-containerize."
  exit 1
fi

# Image build (idempotent — cached by deps hash).
make -C "$REPO_PATH" upstream-test-image

# Switch worktree to the test-ref, run tests in container, restore feature branch.
git checkout upstream-test-ref
scripts/upstream-test-run       # docker run; bind-mounts $PWD read-only; streams stdout
TEST_RC=$?
git checkout "$FEATURE_BRANCH"
git branch -D upstream-test-ref
rm -f /tmp/upstream-integration-{{issue}}.env

POST_SHA=$(git rev-parse HEAD)
[ "$POST_SHA" = "$FEATURE_SHA" ] || { echo "FATAL: HEAD moved"; exit 1; }
[ "$TEST_RC" = "0" ] || { echo "FATAL: make check failed (rc=$TEST_RC)"; exit 1; }
```

### Image shape

`contrib/upstream-container/Dockerfile.test`:

```dockerfile
# Derived from contrib/k8s/Dockerfile.base (ubuntu:24.04 + tmux, jq, dolt,
# gh, Claude CLI, node, git, python3). That image is maintained for gc
# agents and already tracks deps.env's DOLT_VERSION. We add go, bd, and
# build-time tools on top so `make check` sees the same toolchain CI does.
ARG BASE_IMAGE=gc-agent-base:latest
FROM ${BASE_IMAGE}

ARG GO_VERSION=1.25.8
ARG BD_VERSION
ARG BD_REPO=gastownhall/beads

# go — version must match .github/workflows/ci.yml actions/setup-go@v6 input.
RUN curl -fsSL https://go.dev/dl/go${GO_VERSION}.linux-amd64.tar.gz \
  | tar -C /usr/local -xz \
  && ln -s /usr/local/go/bin/go /usr/local/bin/go \
  && ln -s /usr/local/go/bin/gofmt /usr/local/bin/gofmt

# bd — fetched the same way CI fetches it, so container and CI resolve
# the same binary.
RUN ARCH="linux_amd64" \
 && TARBALL="beads_${BD_VERSION#v}_${ARCH}.tar.gz" \
 && curl -fsSL -o "/tmp/${TARBALL}" \
      "https://github.com/${BD_REPO}/releases/download/${BD_VERSION}/${TARBALL}" \
 && tar -xzf "/tmp/${TARBALL}" -C /tmp bd \
 && install -m 0755 /tmp/bd /usr/local/bin/bd \
 && rm -f "/tmp/${TARBALL}" /tmp/bd

# Tools needed by `make check`: golangci-lint, oapi-codegen.
# install-tools is a Makefile target in upstream — copy just enough of the
# repo to run it, then discard so the image stays slim.
WORKDIR /src
COPY Makefile /src/Makefile
COPY go.mod go.sum /src/
RUN make install-tools && rm -rf /src

# Tests run as the non-root gcagent user that base already creates.
USER gcagent
WORKDIR /workspace
```

Build: `make upstream-test-image` (new target in gascity `Makefile`)
sources `deps.env`, computes the deps hash, calls
`docker buildx build --load -f contrib/upstream-container/Dockerfile.test
 --build-arg BD_VERSION=$BD_VERSION
 --build-arg BD_REPO=$BD_REPO
 -t gc-upstream-test:<deps-hash> -t gc-upstream-test:latest .`.

Idempotence: the target skips `docker build` when
`docker image inspect gc-upstream-test:<deps-hash>` already exists.
First pour on a fresh host pays a ~3-minute rebuild cost; every
subsequent pour hits the cache (seconds).

### scripts/upstream-test-run

```bash
#!/usr/bin/env bash
# upstream-test-run — wraps `docker run` for gastownhall-upstream's
# containerized make check. Expects the repo working tree to already be
# switched to upstream-test-ref. Runs with the host worktree bind-mounted
# read-only; writes no artifacts back to the host.
set -euo pipefail

IMAGE="${GC_UPSTREAM_TEST_IMAGE:-gc-upstream-test:latest}"
WORKTREE="$(git rev-parse --show-toplevel)"

# env -i parity with Makefile: scrub host env, re-export only what the
# test harness needs. `docker run --env-file -` would inherit the host
# env entirely; we want an allowlist.
exec docker run --rm \
  --init \
  --env-file /dev/null \
  -e HOME=/home/gcagent \
  -e PATH=/usr/local/go/bin:/usr/local/bin:/usr/bin:/bin \
  -e GOCACHE=/home/gcagent/.cache/go-build \
  -e GOMODCACHE=/home/gcagent/go/pkg/mod \
  -v "${WORKTREE}:/workspace:ro" \
  -v "upstream-gocache:/home/gcagent/.cache/go-build" \
  -v "upstream-gomodcache:/home/gcagent/go/pkg/mod" \
  --user "$(id -u):$(id -g)" \
  --tmpfs /tmp \
  "${IMAGE}" \
  bash -lc 'cd /workspace && cp -a /workspace /tmp/src && cd /tmp/src && make check'
```

Details worth surfacing:

- **Read-only worktree mount** — tests can't mutate the host checkout.
  We copy into a tmpfs `/tmp/src` so `go build`, `go generate`, and
  anything that writes to the repo root works; the copy costs ~1s for
  gascity and is thrown away on exit.
- **Named volumes for `GOCACHE` and `GOMODCACHE`** — warm cache across
  pours. Phase 2 revisits whether to share these with the host.
- **`--user $(id -u):$(id -g)`** — keeps stdout ownership sane if the
  formula later wants to bind-mount a log dir. Phase 1 streams to the
  formula log, so this is defensive.
- **`--env-file /dev/null`** — explicit: we want zero host env leakage.
  Matches CI semantically, matches `env -i` in the Makefile.

### `rebuild-integration` stays on host

Mayor's Phase-1 ask #3: keep `rebuild-integration` on host in Phase 1,
bind-mount the resulting worktree read-only into the container. Agreed
and adopted. Rationale:

- `rebuild-integration` fetches from `cwalv`/`origin` remotes, force-
  pushes `cwalv:integration`, mutates local git refs — all host
  concerns. Running git inside a container that doesn't have push
  credentials is strictly harder than leaving git on host.
- The feature-branch HEAD-preservation invariant is enforced on host,
  where the worker already tracks it.
- Phase 2's deploy-refs split removes this step entirely from the
  per-shipment path, so over-engineering it now would be wasted work.

### Failure semantics

Mayor's Phase-1 ask #2: hard-fail when docker is unavailable rather
than falling back to host `make check`. Adopted. The fallback path is
what Phase 1 exists to eliminate — offering it as a safety net would
let divergence regress silently. The preflight check above produces a
clear mayor-mail with instructions.

### Test output capture

Mayor's Phase-1 ask #4: stream to the formula step's log. Adopted.
`docker run --rm` propagates child stdout/stderr through the formula
step's normal log channel. No bind-mount for logs in Phase 1; if we
later need structured test output (e.g. go test -json for flake
triage), Phase 2 can add a `-v` mount for a scratch log dir.

## Image build + distribution

Mayor's Phase-1 ask #1: MVP = build on first pour, cached by
`deps.env` hash. Adopted, with two refinements:

1. **`deps.env` is the single source of truth** for DOLT_VERSION and
   BD_VERSION. `Dockerfile.test` takes these as `ARG` and the Makefile
   passes them via `--build-arg` from a sourced `deps.env`. No drift.
2. **`GO_VERSION` is pinned in `deps.env` too** (new entry). Currently
   CI hardcodes `"1.25.8"` in `.github/workflows/ci.yml` job `check`.
   Phase 1 lifts that string into `deps.env` and both CI and the test
   image read from the same file. Lockstep bumps.

Registry publishing is explicitly out of Phase 1. First pour rebuilds
take ~3 min on a cold host; cached pours take ~1–2 s of image lookup
plus the test run. Phase 2 evaluates pushing to ghcr for faster cold
starts across rigs.

## Platform parity

Mayor's ask D: confirm CI and Dockerfile.base share a platform.

- CI: `runs-on: ubuntu-latest`. As of GHA's current pointer rollout,
  this is `ubuntu-24.04`.
- `contrib/k8s/Dockerfile.base`: `FROM ubuntu:24.04`. Match.

Risks worth calling out:

- `ubuntu-latest` rolls to `ubuntu-26.04` eventually. When it does,
  gascity's CI and Dockerfile.base will diverge unless the Dockerfile
  is updated in the same PR. Recommend a short note in
  `contrib/k8s/Dockerfile.base` ("If CI bumps `ubuntu-latest`, bump
  this FROM too"). Not Phase 1 blocking.
- Architecture: CI runners are `linux/amd64`. The test image build
  targets `linux/amd64` explicitly via `docker buildx build --platform
  linux/amd64 --load`. Developers on Apple Silicon pay an emulation
  penalty at image build time; acceptable for Phase 1.
- `deps.env` currently only pins DOLT/BD/BR. Phase 1 adds `GO_VERSION`
  to it so the image and CI pin the same go.

## deps.env consolidation

Today:
- `deps.env` has `DOLT_VERSION`, `BD_VERSION`, `BD_REPO`, `BR_VERSION`.
- `.github/workflows/ci.yml` re-declares `DOLT_VERSION: "1.86.1"` and
  `BD_VERSION: "v1.0.0"` in the `check` job's `env:` block.
- Go version only lives in `ci.yml` (`go-version: "1.25.8"`).

Phase 1 does the minimum consolidation needed for the container:

- Add `GO_VERSION=1.25.8` to `deps.env`.
- Change the container Makefile target to `source deps.env` and pass
  versions via `--build-arg`.
- Out-of-scope cleanup (`ci.yml` sourcing `deps.env`, or a preflight
  that fails loudly on drift) is a good follow-up but not load-bearing
  for Phase 1.

## Migration of in-flight molecules

Mayor's ask D: document the stance.

Current in-flight tier-4 molecules (2026-04-21):

- `fo-mol-wmg9`: 3/8 steps done, parked on `fo-mol-ff6j` (run-tests)
  failures from `upstream-test-ref` lint breakage (PR #746's four-
  commit cherry-pick mismatch). Operator parked tier 4.
- `fo-mol-gp9e`: orphaned from an earlier tier-4 pour attempt.

Stance: **Phase 1 does not migrate these.** Rationale:

- Both are already parked by operator for reasons unrelated to the
  formula shape (PR #746 is what's actually stuck). Making Phase 1
  responsible for resuming them couples two unrelated changes.
- The formula change is additive within the existing step IDs
  (`rebuild-integration`, `run-tests`, etc. keep the same IDs). A
  molecule poured under the old formula will, if resumed, execute the
  new formula's `run-tests` body on its next turn — which means
  Phase 1's container path. In practice the operator will re-pour
  rather than resume, because the parked steps' state (upstream-test-
  ref branch, `/tmp/upstream-integration-*.env` file) is stale.
- Phase 2 (deploy-refs split) renames or removes steps, which IS
  incompatible. That bead will own the migration for any molecules
  still in flight at that time.

Recommendation to operator: after Phase 1 merges, supersede
`fo-mol-wmg9` and `fo-mol-gp9e` with a clean re-pour. No automation
needed in Phase 1.

## Alternatives considered

### Alt 1 — `gc-session-docker` directly

The bead notes suggest invoking `gc-session-docker` or its underlying
`docker run` primitive. I looked at the script: it's an exec session
provider (protocol from `internal/session/exec/exec.go`), intended for
running interactive agent tmux sessions. It's well-tested for that
shape but drags in unnecessary surface area for a one-shot `make check`
— tmux inside the container, multi-exec dispatch, a container that
`sleep infinity`s then attaches.

A straight `docker run --rm ... make check` is a dozen lines of shell
and exactly matches what CI does. Using gc-session-docker would be
"clever reuse" that obscures what's happening and makes debugging
harder when make check misbehaves inside the container.

Adopted: plain `docker run` through `scripts/upstream-test-run`. The
script is under 30 lines, no tmux, one process, clean exit code.

Cross-link: if a future formula step needs an interactive/tmux-backed
container (e.g. a multi-step debug-in-container flow), that's the time
to reach for `gc-session-docker`. Keeping them separate for now.

### Alt 2 — `FROM gc-agent:latest`

The bead suggests `FROM gc-agent:latest`. `gc-agent` adds gc/bd/br
binaries to the base. For make check we want a **deterministic** bd
(the `deps.env` version, fetched the same way CI fetches) rather than
whatever bd happened to be copied into the local agent image. Also,
the agent image depends on the gc binary being pre-built on host,
which would create a rebuild cascade every time gc changes.

Adopted: `FROM gc-agent-base:latest`. Slightly more RUN steps, but the
test image becomes independent of gc/bd binary churn and always pins
exactly what CI pins.

### Alt 3 — nix shell / direnv / `act`

Nix gives reproducibility without containers, but:

- gascity doesn't have a `flake.nix` today. Adding one is far bigger
  than Phase 1.
- `act` (running GHA locally) would literally replay ci.yml. Tempting,
  but `act` has its own quirks (networking, secret handling, slow
  cold-start) and debuggability inside CI actions is worse than inside
  a shell container.

Adopted: plain docker. Phase 2 can revisit if we grow more than one
formula that wants container isolation.

## Acceptance (Phase 1)

- `run-tests` step invokes docker; success/failure semantics unchanged
  from the worker's perspective (TEST_RC=0 means ship).
- Running `make check` on `upstream-test-ref` inside the container
  succeeds on an upstream-test-ref that CI would also pass. Two
  targeted checks against PR #746's tests:
  - `TestTutorial01/mail` passes (env leak gone).
  - Four k8s-manifest tests pass without the Makefile env-wrap
    depending on anything other than `env -i` matching a clean host
    env — because the host env IS clean inside the container.
- Docker missing on host → hard-fail with mayor mail (no host
  fallback).
- `rebuild-integration` stays on host; feature-branch HEAD-preservation
  invariant holds.
- `make upstream-test-image` is idempotent across pours; first pour
  ~3 min, subsequent pours hit cache.
- `engdocs/architecture/formulas.md` (or wherever the formula contract
  currently lives — see Open questions) gets a paragraph describing
  the container contract.

## Non-goals (Phase 1)

- Renaming `integration-refs.toml`. Phase 2.
- Splitting deploy-branch from test-integration. Phase 2.
- Publishing the test image to a registry. Phase 2 or later.
- Containerizing other formulas. Separate bead when the need arises.
- Rewriting gascity's CI. CI stays authoritative; the container just
  mirrors it.
- Removing the testenv scrub / Makefile env-wrap. They coexist as
  belt-and-suspenders for developer-invoked test runs and for the
  transition period while Phase 1 soaks.

## Follow-on work (Phase 2+)

- **Phase 2 — deploy-branch split** (new bead, to file when Phase 1
  lands):
  - Rename `integration-refs.toml` → `deploy-refs.toml`.
  - Extract `rebuild-deploy` step, decouple from per-shipment flow
    (run at `gc` binary install time).
  - Remove the `rebuild-integration` step from the per-shipment path.
  - Document the deploy-branch lifecycle in
    `docs/gc-chost-city-deviations.md`.
  - Migration of any remaining old-formula molecules happens here.

- **Phase 3 — test image distribution** (defer until Phase 1 soak):
  - Publish `gc-upstream-test` to ghcr (or private registry).
  - `make upstream-test-image` becomes a pull, not a build, by default.
  - Developers can still `make upstream-test-image-local` for hacking
    on the image itself.

- **deps.env consolidation** (opportunistic):
  - `ci.yml` sources `deps.env` for DOLT/BD/GO versions rather than
    re-declaring them.
  - A `make check-deps-drift` target asserts CI and deps.env agree.

- **Nudge/mail `--notify` bug** (separate bead, surfaced in claim
  notes): `gc session nudge` + `gc mail send --notify` error with
  `setting metadata on gc-17: ambiguous ID 'gc-17' matches 86 issues`.
  Short-ID resolution bug in the nudge metadata-write path. Not
  blocking containerize, but the test image soak could easily hit it
  again during mayor correspondence — worth a bead.

## Open questions for mayor

1. **Where does the formula contract doc live?** The bead references
   `engdocs/architecture/formulas.md`; that file doesn't exist today.
   Options: create it in Phase 1, or extend an existing arch doc
   (`engdocs/architecture/dispatch.md` touches formulas at the edges).
   Pref: new file `engdocs/architecture/formulas.md` since formulas
   will only grow as an architectural concept.

2. **`contrib/upstream-container/` vs `contrib/upstream/`?** Phase 2
   may add deploy-refs-related scripts too. If we want them under the
   same directory, name it `contrib/upstream/`. If the container layer
   is the only thing foundations-external that lives in gascity,
   `contrib/upstream-container/` is clearer. Slight pref for
   `contrib/upstream-container/` — discoverable.

3. **`scripts/upstream-test-run` vs inline bash in the formula?** The
   helper script is ~25 lines; folding it into the formula step's
   bash is doable but bloats the TOML and makes iterating on the
   docker args harder (no way to test the docker invocation in
   isolation without running a full formula pour). Pref: keep as a
   script.

4. **GOCACHE volume lifetime.** The design above uses named volumes
   `upstream-gocache`, `upstream-gomodcache`. They persist
   indefinitely across pours (cache warmth) but also accumulate
   forever. Reasonable policies: leave them forever (current),
   periodic `docker volume prune` in a maintenance order, or a
   fixed-size LRU. Pref: leave them forever for Phase 1, add pruning
   in Phase 2 if size becomes a problem.

5. **Approval gate.** Per the bead, hard gate on implementation until
   mayor approves. Please drop approval in the bead notes or by
   mail — I'll hold implementation until the design is signed off.

## References

- `fo-upstream-formula-containerize` (this bead) — scope + decisions.
- `fo-pr746-test-debug` — concrete example of local/CI divergence.
- `fo-upstream-formula-rig-refactor` — shipped; predecessor design
  whose shape this doc mimics
  (`engdocs/design/gastownhall-upstream-rig-refactor.md`).
- `gastownhall-upstream.formula.toml` at
  `projects/foundations/formulas/` — target formula.
- `.github/workflows/ci.yml` job `check` — CI pin source.
- `contrib/k8s/Dockerfile.base` — upstream-maintained base image,
  parent of the test image.
- `scripts/gc-session-docker` — alternative-2 reference.
- `deps.env` — single source of truth for pinned deps (Phase 1 extends
  it with `GO_VERSION`).
