---
title: Installation
description: Build Gas City locally and install the contributor toolchain.
---

## Prerequisites

For the default local workflow, install:

- Go 1.25 or newer
- `tmux`
- `jq`
- the Beads CLI (`bd`)

Optional dependencies:

- `dolt` for bd-backed integration flows and some advanced local setups
- Docker for container and image workflows
- Kubernetes tooling for the native K8s provider

The CI pin set lives in [`deps.env`](https://github.com/gastownhall/gascity/blob/main/deps.env). If you need to match CI
exactly, start there.

## Install From A Release

Homebrew:

```bash
brew install gastownhall/gascity/gascity
gc version
```

Direct download:

```bash
VERSION=0.13.0
OS=$(uname -s | tr '[:upper:]' '[:lower:]')
ARCH=$(uname -m)
case "$ARCH" in
  x86_64) ARCH=amd64 ;;
  aarch64|arm64) ARCH=arm64 ;;
esac

curl -fsSLO "https://github.com/gastownhall/gascity/releases/download/v${VERSION}/gascity_${VERSION}_${OS}_${ARCH}.tar.gz"
tar -xzf "gascity_${VERSION}_${OS}_${ARCH}.tar.gz"
./gc version
```

## Build `gc` From Source

From a clean clone:

```bash
make install
gc version
```

If you do not want to install globally, build the local binary instead:

```bash
make build
./bin/gc version
```

## Contributor Setup

Install local dev tooling and hooks:

```bash
make setup
make check
```

`make check` runs the fast Go quality gates, including the repo's docs sync and
local-link tests.

## Docs Preview

The docs site now uses Mintlify. Preview it locally with:

```bash
cd docs
npx --yes mint@latest dev
```

Run a local docs check without starting the preview server:

```bash
make check-docs
```
