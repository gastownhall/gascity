#!/usr/bin/env bash
# Dolt-only soak (baseline vs the 4 RC backends).
# Expects COORDSTORE_DOLT_DSN, COORDSTORE_RESULTS_DIR set.
set -uo pipefail
export GOPATH=/home/jaword/mayor-claude/go
export GOCACHE=/home/jaword/mayor-claude/.cache/go-build
export GOMODCACHE=/home/jaword/mayor-claude/go/pkg/mod
export GOROOT=/usr/lib/golang
export PATH="$GOROOT/bin:/usr/lib64/ccache:/usr/bin:/bin:$PATH"
export CGO_ENABLED=1
export COORDSTORE_SOAK=1
export COORDSTORE_FULL_MATRIX=1
export COORDSTORE_SQLITE_CGO=1
export COORDSTORE_SOAK_DURATION="${COORDSTORE_SOAK_DURATION:-2h}"
: "${COORDSTORE_DOLT_DSN:?must set COORDSTORE_DOLT_DSN}"
: "${COORDSTORE_RESULTS_DIR:?must set COORDSTORE_RESULTS_DIR}"
mkdir -p "$COORDSTORE_RESULTS_DIR"
cd /var/tmp/coordstore-soak-wt || exit 1
{
  echo "soak_launch=$(date -u +%FT%TZ)"
  echo "backend=dolt (baseline run, ga-babhr)"
  echo "duration_per_phase=$COORDSTORE_SOAK_DURATION"
  echo "dolt_dsn=$COORDSTORE_DOLT_DSN"
  echo "results=$COORDSTORE_RESULTS_DIR"
  echo "branch_commit=$(git rev-parse HEAD 2>/dev/null)"
} > "$COORDSTORE_RESULTS_DIR/launch.txt"
exec go test -tags sqlite_cgo -count=1 ./internal/benchmarks/coordstore/ \
  -run 'TestBenchmarkSoakDolt' -timeout 0 -v \
  > "$COORDSTORE_RESULTS_DIR/workflow.log" 2>&1
