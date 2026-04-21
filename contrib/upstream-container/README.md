# upstream-container

Docker image used by the `gastownhall-upstream` formula's `run-tests`
step to run `make check` in a CI-parity environment.

## Why

The formula previously ran `make check` in the worker agent's shell,
which inherits city/session state (`GC_CITY_PATH`, `GC_DOLT_*`,
`GC_ALIAS`, …) that CI does not. That divergence is the root cause the
`internal/testenv` scrub and the Makefile `env -i` wrap were papering
over. Containerization fixes it at the source: the test harness runs in
a clean environment by construction.

See `engdocs/architecture/formulas.md` for the formula-step contract
and `engdocs/design/gastownhall-upstream-containerize.md` for the
design rationale.

## Contents

- `Dockerfile.upstream` — builds on `gc-agent-base:latest`, adds go, bd,
  and `make install-tools` outputs. Versions come from `deps.env` so
  the container and CI pin the same toolchain.

## Build

```bash
make upstream-test-image
```

Tags `gc-upstream-test:latest` and `gc-upstream-test:<deps-hash>` where
`<deps-hash>` is `sha256(deps.env + Dockerfile.upstream)` truncated to
12 hex. First build takes ~3 minutes; subsequent builds with the same
hash are a no-op tag update.

## Run

The formula invokes the container through `scripts/upstream-test-run`,
which bind-mounts the worktree read-only and runs `make check` inside.
Override the image tag with `GC_UPSTREAM_TEST_IMAGE`.

Registry publishing is out of scope for this phase — the image is built
locally on each host. See the follow-on work in the design doc for the
Phase-2 registry plan.

## Docker prerequisites

`check-docker` (dependency of `upstream-test-image`) enforces that
docker and `docker buildx` are available. The formula's `run-tests`
step hard-fails with a mayor mail if docker is missing — there is no
host fallback; that fallback is what containerization exists to
eliminate.
