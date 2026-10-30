#!/usr/bin/env bash
# Download every module the main module needs to build and test, retrying
# transient module-proxy failures, so later build and test steps never fetch.
#
# The go command does not retry a failed module fetch. One HTTP/2 stream
# reset from proxy.golang.org ("stream error: stream ID N; INTERNAL_ERROR;
# received from peer") otherwise fails a whole test package as
# "[setup failed]" minutes into the job. Each attempt here re-runs
# `go mod download`, which only fetches what the module cache still lacks.
#
# GOPROXY: the default "https://proxy.golang.org,direct" falls back to the
# next entry only on 404/410, so a stream error is fatal. When GOPROXY is the
# default, this script uses the list setup-bazel already gives gazelle's
# fetch_repo: "https://proxy.golang.org|https://proxy.golang.org|direct",
# where "|" moves on after any error (the proxy twice, then the module's
# origin). Every downloaded module is still checked against go.sum, and
# GOSUMDB is left as configured, so a fallback source can fail the step but
# cannot change what gets built. An explicitly configured GOPROXY is used
# unchanged.
#
# Tunables (for tests and incident response):
#   GO_MOD_DOWNLOAD_ATTEMPTS         attempts before giving up (default 4)
#   GO_MOD_DOWNLOAD_BACKOFF_SECONDS  first backoff, doubled each retry (default 10)
set -euo pipefail

readonly default_goproxy="https://proxy.golang.org,direct"
readonly resilient_goproxy="https://proxy.golang.org|https://proxy.golang.org|direct"

attempts="${GO_MOD_DOWNLOAD_ATTEMPTS:-4}"
backoff="${GO_MOD_DOWNLOAD_BACKOFF_SECONDS:-10}"
if ! [[ "$attempts" =~ ^[1-9][0-9]*$ ]]; then
  echo "go-mod-download-retry: GO_MOD_DOWNLOAD_ATTEMPTS must be a positive integer, got \"$attempts\"" >&2
  exit 2
fi
if ! [[ "$backoff" =~ ^[0-9]+$ ]]; then
  echo "go-mod-download-retry: GO_MOD_DOWNLOAD_BACKOFF_SECONDS must be a non-negative integer, got \"$backoff\"" >&2
  exit 2
fi

configured_goproxy="$(go env GOPROXY)"
if [[ "$configured_goproxy" == "$default_goproxy" ]]; then
  export GOPROXY="$resilient_goproxy"
else
  export GOPROXY="$configured_goproxy"
fi
echo "go-mod-download-retry: GOPROXY=$GOPROXY GOSUMDB=$(go env GOSUMDB) GOFLAGS=$(go env GOFLAGS)"

attempt=1
while :; do
  if go mod download; then
    echo "go-mod-download-retry: module download complete on attempt $attempt of $attempts"
    exit 0
  fi
  if ((attempt >= attempts)); then
    echo "::error title=go mod download failed::go mod download failed after $attempts attempts (GOPROXY=$GOPROXY)" >&2
    exit 1
  fi
  echo "::warning title=go mod download retry::attempt $attempt of $attempts failed; retrying in ${backoff}s" >&2
  sleep "$backoff"
  backoff=$((backoff * 2))
  attempt=$((attempt + 1))
done
