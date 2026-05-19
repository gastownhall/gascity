#!/usr/bin/env bash
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "$SCRIPT_DIR/.." && pwd)"
T3_ROOT="$(cd "$REPO_ROOT/../../.." && pwd)"

library="libdoltlite.so"
case "$(uname -s)" in
  Darwin) library="libdoltlite.dylib" ;;
esac

build_dir="${DOLTLITE_BUILD_DIR:-$T3_ROOT/packages/doltlite/build}"
if [[ ! -f "$build_dir/sqlite3.h" || ! -f "$build_dir/libdoltlite.a" ]]; then
  echo "missing doltlite build at $build_dir; expected sqlite3.h and libdoltlite.a" >&2
  exit 1
fi

export CGO_ENABLED=1
export CGO_CFLAGS="${CGO_CFLAGS:+$CGO_CFLAGS }-I$build_dir"
export CGO_LDFLAGS="${CGO_LDFLAGS:+$CGO_LDFLAGS }$build_dir/libdoltlite.a -lz -lpthread -lm"
if [[ "${GOFLAGS:-}" != *libsqlite3* ]]; then
  export GOFLAGS="${GOFLAGS:+$GOFLAGS }-tags=libsqlite3"
fi

if [[ $# -eq 0 ]]; then
  set -- ./internal/beads -run 'Doltlite|doltlite'
fi

cd "$REPO_ROOT"
go test "$@"
