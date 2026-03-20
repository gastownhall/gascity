---
title: "Image Dependency Versioning"
---

The `gc-agent` Docker image (`contrib/k8s/Dockerfile.agent`) currently copies
pre-built binaries (`gc`, `bd`, `br`) from the build context with no version
pinning or provenance tracking. Binaries must be compiled separately and placed
in the repo root before `docker build`. This is fragile: there is no record of
which commit each binary came from, and adding new dependencies (e.g. `wl`)
means more ad-hoc copy steps.

This document evaluates three approaches and recommends one.

## Current state

```
Makefile                    # "make build" compiles gc
Dockerfile.agent            # COPY gc bd br from build context
```

The operator runs `go build`, `cp $(which bd) .`, etc., then
`docker build -f contrib/k8s/Dockerfile.agent`. Nothing enforces that the
binaries match a specific version.

Dependencies that need versioning:

| Binary | Source repo | Notes |
|--------|------------|-------|
| `gc`   | gascity (this repo) | Built from `./cmd/gc` |
| `bd`   | beads | Bead store CLI |
| `br`   | beads_rust | Rust bead store CLI |
| `wl`   | wasteland | Wasteland CLI (not yet included) |

## Option 1: Multi-stage Docker build from source

Each dependency is cloned and built inside the Docker build at a pinned
git ref. A `deps.env` file in the repo root records the refs.

```env
# deps.env
GC_REF=v0.14.0
WL_REF=abc1234
BD_REF=v0.9.0
BR_REF=v0.2.1
```

```dockerfile
# Dockerfile.agent (sketch)
FROM golang:1.23 AS build-gc
ARG GC_REF=main
COPY . /src
WORKDIR /src
RUN go build -ldflags "..." -o /out/gc ./cmd/gc

FROM golang:1.23 AS build-wl
ARG WL_REF=main
RUN git clone https://github.com/user/wasteland /src \
    && cd /src && git checkout ${WL_REF} \
    && go build -o /out/wl ./cmd/wl

FROM golang:1.23 AS build-bd
ARG BD_REF=main
RUN git clone https://github.com/user/beads /src \
    && cd /src && git checkout ${BD_REF} \
    && go build -o /out/bd ./cmd/bd

FROM gc-agent-base:latest
COPY --from=build-gc /out/gc   /usr/local/bin/gc
COPY --from=build-wl /out/wl   /usr/local/bin/wl
COPY --from=build-bd /out/bd   /usr/local/bin/bd
# br is a Rust binary — similar pattern with rust:1.x stage
```

The Makefile reads `deps.env` and passes `--build-arg` values:

```makefile
include deps.env
docker-agent:
	docker build -f contrib/k8s/Dockerfile.agent \
	  --build-arg GC_REF=$(GC_REF) \
	  --build-arg WL_REF=$(WL_REF) \
	  --build-arg BD_REF=$(BD_REF) \
	  -t gc-agent:latest .
```

**Pros:**
- Fully reproducible — `deps.env` + Dockerfile is the complete build recipe.
- Single command produces the image; no manual pre-build steps.
- Version pinning is visible in version control (diff `deps.env`).
- Works without a release pipeline.

**Cons:**
- Slower builds (compiles everything from source; mitigated by Docker layer cache
  and BuildKit cache mounts).
- Needs git clone access to private repos from inside Docker build
  (solvable with `--ssh` or `--secret`).
- `br` (Rust) needs a separate toolchain stage.

## Option 2: GitHub release artifacts

Each project publishes tagged release binaries. The Dockerfile downloads them.

```dockerfile
ARG GC_VERSION=0.14.0
ADD https://github.com/.../releases/download/v${GC_VERSION}/gc-linux-amd64 \
    /usr/local/bin/gc
RUN chmod +x /usr/local/bin/gc
```

**Pros:**
- Fast builds — downloads pre-compiled binaries.
- Clear versioning via release tags.
- Cacheable Docker layers (version in URL acts as cache key).

**Cons:**
- Requires a release pipeline for every dependency (CI to build, tag, publish).
- Heavier infrastructure commitment upfront.
- Private repos need auth tokens for artifact download.

## Option 3: Version manifest with host-side build

A `deps.env` manifest pins versions, but the Makefile builds/fetches binaries
on the host before `docker build`. The Dockerfile still `COPY`s from build
context, but the Makefile enforces the right versions.

```makefile
include deps.env
.PHONY: fetch-deps
fetch-deps:
	cd /data/projects/wasteland && git checkout $(WL_REF) && go build -o $(PWD)/wl ./cmd/wl
	cd /data/projects/beads && git checkout $(BD_REF) && go build -o $(PWD)/bd ./cmd/bd
```

**Pros:**
- Simple; minimal changes to existing Dockerfile.
- Version pinning via manifest file.
- Fast iteration (reuse host Go cache).

**Cons:**
- Not self-contained — depends on host having the repos checked out.
- Reproducibility depends on host state (Go version, module cache).
- Still `COPY`-based — no provenance in the image itself.

## Recommendation

**Option 1 (multi-stage build from source)** with a version manifest.

Rationale:
- All four dependencies are private Go/Rust projects in active development
  without release pipelines. Option 2 is premature.
- Option 3 improves the status quo but still depends on host state. The
  Dockerfile should be self-contained.
- Multi-stage builds are the standard Docker pattern for this. BuildKit cache
  mounts (`--mount=type=cache,target=/root/.cache/go-build`) keep rebuild times
  reasonable after the first build.
- When projects mature enough for tagged releases, switching to Option 2 is a
  Dockerfile-only change — the `deps.env` interface stays the same.

### Migration path

1. Add `deps.env` to repo root with current git refs for each dependency.
2. Rewrite `Dockerfile.agent` as multi-stage (gc built from local context,
   external deps cloned at pinned refs).
3. Update Makefile `docker-agent` target to read `deps.env` and pass build args.
4. Add `wl` as a new build stage and COPY target.
5. Add OCI labels (`org.opencontainers.image.version`, `org.opencontainers.image.revision`)
   so `docker inspect` shows exactly what's inside.
6. Later: add CI job that bumps `deps.env` refs when upstream repos tag releases.
