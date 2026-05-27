#!/usr/bin/env bash
# Badger-only soak (fills the gap from the OOM-killed full-matrix run).
# Same env as /var/tmp/coordstore-soak-launch.sh; -run filter restricts to badger.
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
: "${COORDSTORE_RESULTS_DIR:?must set COORDSTORE_RESULTS_DIR}"
mkdir -p "$COORDSTORE_RESULTS_DIR"
cd /var/tmp/coordstore-soak-wt || exit 1
{
  echo "soak_launch=$(date -u +%FT%TZ)"
  echo "duration_per_backend=$COORDSTORE_SOAK_DURATION"
  echo "results=$COORDSTORE_RESULTS_DIR"
  echo "backend=badger (filling gap from 05-25 OOM-killed full-matrix run)"
  echo "branch_commit=$(git rev-parse HEAD 2>/dev/null)"
} > "$COORDSTORE_RESULTS_DIR/launch.txt"
exec go test -tags sqlite_cgo -count=1 ./internal/benchmarks/coordstore/ \
  -run 'TestBenchmarkSoakFullMatrix/badger' -timeout 0 -v \
  > "$COORDSTORE_RESULTS_DIR/workflow.log" 2>&1
