# syntax=docker/dockerfile:1
# Gas City single-container city: one image, one process tree.
#
# `gc supervisor run` is the container's only long-lived process, started by
# tini so children are reaped and SIGTERM drains cities before exit. Docker
# owns the image, mounts, port, and restarts; Gas City owns everything else.
#
# Build:  make docker-city
#         (builds contrib/k8s/Dockerfile.base first; Dolt is copied from it)
# Run:    contrib/docker/compose.yaml; guide: docs/guides/running-in-a-container.md
#
# The supervisor binds all interfaces inside the container and accepts
# unverified writes, so the loopback-only port publish in compose.yaml is the
# trust boundary. Publish beyond loopback only with the hardened posture in
# docs/runbooks/remote-hardened-city.md.

ARG DOLT_IMAGE=gc-agent-base:latest

FROM --platform=$BUILDPLATFORM golang:1.26.9-bookworm@sha256:d9c68c2c51161e12fd77e4c6320687c9cd86e1af1e3ad6e6cd63ff970641453c AS build
WORKDIR /src
COPY . .
ARG TARGETOS TARGETARCH
ARG GC_VERSION=dev
ARG GC_COMMIT=unknown
ARG GC_DATE=unknown
RUN --mount=type=cache,target=/go/pkg/mod --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath -buildvcs=false \
      -ldflags "-s -w -X main.version=$GC_VERSION -X main.commit=$GC_COMMIT -X main.date=$GC_DATE" \
      -o /out/gc ./cmd/gc

# Rebuild the pinned beads source with the same toolchain and dependency
# security overrides as the Kubernetes agent image.
FROM golang:1.26.9-bookworm@sha256:d9c68c2c51161e12fd77e4c6320687c9cd86e1af1e3ad6e6cd63ff970641453c AS bd
ARG GRPC_VERSION=1.83.2
ARG THRIFT_VERSION=0.24.0
ARG XNET_VERSION=0.60.0
WORKDIR /bd
RUN --mount=type=bind,source=deps.env,target=/deps.env \
    --mount=type=cache,target=/go/pkg/mod --mount=type=cache,target=/root/.cache/go-build \
    set -eu && . /deps.env && git init -q . \
 && git fetch -q --depth 1 "https://github.com/${BD_REPO}.git" "$BD_CURRENT_REF" && git checkout -q FETCH_HEAD \
 && go get "google.golang.org/grpc@v${GRPC_VERSION}" \
 && go get "github.com/apache/thrift@v${THRIFT_VERSION}" \
 && go get "golang.org/x/net@v${XNET_VERSION}" \
 && CGO_ENABLED=1 go build -trimpath -tags gms_pure_go \
      -ldflags "-s -w -X main.Version=${BD_CURRENT_VERSION#v} -X main.Commit=${BD_CURRENT_REF} -X main.Build=$(git rev-parse --short=10 HEAD)" \
      -o /out/bd ./cmd/bd \
 && /out/bd --version \
 && go version -m /out/bd | tr '\t' ' ' | grep -Fq "dep google.golang.org/grpc v${GRPC_VERSION} " \
 && go version -m /out/bd | tr '\t' ' ' | grep -Fq "dep github.com/apache/thrift v${THRIFT_VERSION} " \
 && go version -m /out/bd | tr '\t' ' ' | grep -Fq "dep golang.org/x/net v${XNET_VERSION} "

# Reuse the source-built Dolt from the canonical Kubernetes base image,
# including its current toolchain and dependency-security overrides.
FROM ${DOLT_IMAGE} AS dolt

FROM debian:trixie-slim@sha256:a29215f6a35e51e22adffa17f89e9d2ef06214e64a2bad10d765c46aea49f11f
RUN apt-get update && apt-get install -y --no-install-recommends \
    ca-certificates \
    curl \
    git \
    jq \
    # lsof: TCP-LISTEN / port discovery for Dolt — runtime hard dep (gc doctor), not optional diagnostics.
    lsof \
    openssh-client \
    procps \
    tini \
    tmux \
    && rm -rf /var/lib/apt/lists/*

ARG CLAUDE_CODE_VERSION=2.1.258
ARG CODEX_VERSION=0.152.1
RUN --mount=type=bind,source=.github/scripts/install-claude-native.sh,target=/install-claude-native.sh \
    bash /install-claude-native.sh "$CLAUDE_CODE_VERSION" && rm -rf /root/.claude /root/.claude.json /root/.cache
RUN --mount=type=bind,source=.github/scripts/install-codex-archive.sh,target=/install-codex-archive.sh \
    bash /install-codex-archive.sh "$CODEX_VERSION" && rm -rf /root/.codex

COPY --from=dolt /usr/local/bin/dolt /usr/local/bin/dolt
COPY --from=build /out/gc /usr/local/bin/gc
COPY --from=bd /out/bd /usr/local/bin/bd

# A real account, not a bare --user uid: Claude Code refuses root, and the
# supervisor, Dolt, and both clients resolve HOME through passwd.
ARG UID=1000
ARG GID=1000
RUN groupadd --non-unique --gid "$GID" gascity \
 && useradd --uid "$UID" --gid gascity --create-home --shell /bin/bash gascity \
 && install -d -o gascity -g gascity -m 0700 \
      /home/gascity/.gc /home/gascity/.dolt /home/gascity/.claude /home/gascity/.codex

COPY --chown=gascity:gascity <<TOML /home/gascity/.gc/supervisor.toml
[supervisor]
bind = "0.0.0.0"
allow_mutations = true
write_auth_allow_unverified = true
TOML

ENV LANG=C.UTF-8 GC_SUPERVISOR_LOG_TEE=0
USER gascity
WORKDIR /home/gascity
EXPOSE 8372
HEALTHCHECK --interval=30s --timeout=5s --start-period=30s \
  CMD curl -fsS -o /dev/null http://127.0.0.1:8372/health || exit 1
ENTRYPOINT ["/usr/bin/tini", "--"]
CMD ["gc", "supervisor", "run"]
