#!/usr/bin/env bash
# Build DoltLite-linked binaries with CGO/libsqlite3.
set -euo pipefail

usage() {
  cat <<'EOF'
usage: gc beads-doltlite build [gc|bd|client|all] [options]

Builds DoltLite-linked binaries from the Gas City and beads-doltlite source trees.
The default target is gc.

Options:
  --source DIR       Source checkout for the selected single target.
  --gc-source DIR    Gas City source checkout. Default: ./gascity, current dir, or script checkout.
  --bd-source DIR    beads-doltlite source checkout. Default: ./beads-doltlite or adjacent checkout.
  --doltlite-source DIR
                     DoltLite source checkout. Default: ./doltlite or adjacent checkout.
  --lib DIR          Directory containing libdoltlite.so. Default: ./doltlite-work/build or ./doltlite/build.
  --bootstrap-dir DIR
                     Source checkout cache for missing DoltLite/beads-doltlite deps.
                     Default: <city>/.cache/beads-doltlite/sources.
  --doltlite-repo URL
                     DoltLite source repo. Default: https://github.com/dolthub/doltlite.git.
  --bd-repo URL      beads-doltlite source repo. Default: https://github.com/duncan4123/beads-doltlite.git.
  --no-bootstrap     Do not clone/build missing DoltLite or beads-doltlite deps.
  --output FILE      Build output path for the selected single target.
  --gc-output FILE   Build output path for gc. Default: <gc-source>/bin/gc.
  --bd-output FILE   Build output path for bd. Default: <bd-source>/bin/bd.
  --client-output FILE
                     Build output path for doltlite-client.
                     Default: <build-details-dir>/bin/doltlite-client.
  --install          Install built binaries after link verification.
  --install-dir DIR  Install directory for both binaries.
                     Default: existing supervisor gc path, active binary under $HOME,
                     else $HOME/.local/bin.
  --gc-install FILE  Install path for gc.
  --bd-install FILE  Install path for bd.
  --build-details-dir DIR
                     Directory for last-build-*.json.
                     Default: runtime pack state dir.
  --restart          Stop supervisor/city before building gc, then start after install. Default.
  --no-restart       Do not restart supervisor/city after installing gc.
  --version VALUE    Version string embedded in gc. Default: dev.
  --bd-build VALUE   Build string embedded in bd. Default: dev.

Environment overrides:
  GASCITY_SRC, GC_GASCITY_SRC
  BD_SRC, BEADS_DOLTLITE_SRC, GC_BEADS_DOLTLITE_SRC
  DOLTLITE_LIB, GC_DOLTLITE_LIB
  DOLTLITE_SRC, GC_DOLTLITE_SRC
  GC_DOLTLITE_BOOTSTRAP_SOURCES, GC_DOLTLITE_BOOTSTRAP_DIR
  GC_DOLTLITE_REPO, GC_BEADS_DOLTLITE_REPO
  OUTPUT, GC_DOLTLITE_GC_OUTPUT, BD_OUTPUT, GC_DOLTLITE_BD_OUTPUT
  GC_DOLTLITE_INSTALL, GC_DOLTLITE_INSTALL_DIR
  GC_DOLTLITE_GC_INSTALL, GC_DOLTLITE_BD_INSTALL
  GC_DOLTLITE_CLIENT_OUTPUT
  GC_DOLTLITE_BUILD_DETAILS_DIR
  GC_DOLTLITE_GO_CACHE_ROOT, GOCACHE, GOMODCACHE, GOTMPDIR
  GC_DOLTLITE_RESTART_AFTER_INSTALL, GC_DOLTLITE_RESTART_WAIT_SECONDS
  GC_VERSION, GC_COMMIT, GC_BUILD_DATE
  BD_BUILD, BD_COMMIT, BD_BRANCH
EOF
}

die() {
  echo "$*" >&2
  exit 1
}

usage_error() {
  echo "$*" >&2
  usage >&2
  exit 2
}

require_value() {
  if [ -z "${2:-}" ] || [[ "${2:-}" == --* ]]; then
    usage_error "$1 requires a value"
  fi
}

abs_dir() {
  cd "$1" && pwd
}

has_gascity_source() {
  [ -f "$1/go.mod" ] && [ -d "$1/cmd/gc" ]
}

has_bd_source() {
  [ -f "$1/go.mod" ] && [ -d "$1/cmd/bd" ]
}

has_doltlite_lib() {
  [ -r "$1/libdoltlite.so" ] || [ -r "$1/libdoltlite.so.0" ] || [ -r "$1/libdoltlite.dylib" ]
}

has_doltlite_source() {
  [ -f "$1/main.mk" ] && [ -f "$1/Makefile" ]
}

find_gascity_source() {
  for candidate in \
    "$CITY_ROOT/gascity" \
    "$CITY_ROOT/../gascity" \
    "$(pwd)" \
    "$SCRIPT_CHECKOUT"; do
    if has_gascity_source "$candidate"; then
      abs_dir "$candidate"
      return 0
    fi
  done
  return 1
}

find_bd_source() {
  for candidate in \
    "$CITY_ROOT/beads-doltlite" \
    "$CITY_ROOT/../beads-doltlite" \
    "$SCRIPT_CHECKOUT/../beads-doltlite" \
    "$(pwd)"; do
    if has_bd_source "$candidate"; then
      abs_dir "$candidate"
      return 0
    fi
  done
  return 1
}

find_doltlite_source() {
  for candidate in \
    "$CITY_ROOT/doltlite" \
    "$CITY_ROOT/doltlite-work" \
    "$CITY_ROOT/../doltlite" \
    "$CITY_ROOT/../doltlite-work" \
    "$SCRIPT_CHECKOUT/../doltlite"; do
    if has_doltlite_source "$candidate"; then
      abs_dir "$candidate"
      return 0
    fi
  done
  return 1
}

find_doltlite_lib() {
  for candidate in \
    "$CITY_ROOT/doltlite" \
    "$CITY_ROOT/doltlite-work" \
    "$CITY_ROOT/doltlite-work/build" \
    "$CITY_ROOT/doltlite/build" \
    "$CITY_ROOT/../doltlite" \
    "$CITY_ROOT/../doltlite-work" \
    "$CITY_ROOT/../doltlite-work/build" \
    "$CITY_ROOT/../doltlite/build"; do
    if has_doltlite_lib "$candidate"; then
      abs_dir "$candidate"
      return 0
    fi
  done
  return 1
}

target_includes_bd() {
  [ "$TARGET" = "bd" ] || [ "$TARGET" = "all" ]
}

clone_source_if_missing() {
  local repo="$1"
  local dest="$2"
  local label="$3"

  if [ -d "$dest/.git" ]; then
    echo "using existing $label source: $dest"
    return 0
  fi
  if [ -e "$dest" ]; then
    die "$label bootstrap path exists but is not a git checkout: $dest"
  fi
  command -v git >/dev/null 2>&1 || die "git is required to bootstrap missing $label source"
  mkdir -p "$(dirname "$dest")"
  echo "cloning $label source from $repo into $dest"
  git clone "$repo" "$dest"
}

bootstrap_bd_source() {
  if [ "$BOOTSTRAP_SOURCES" != "1" ] || ! target_includes_bd; then
    return 0
  fi
  if [ -n "$BD_SRC" ] && has_bd_source "$BD_SRC"; then
    return 0
  fi
  local found
  found="$(find_bd_source || true)"
  if [ -n "$found" ]; then
    BD_SRC="$found"
    return 0
  fi

  BD_SRC="$BOOTSTRAP_DIR/beads-doltlite"
  clone_source_if_missing "$BD_REPO" "$BD_SRC" "beads-doltlite"
  has_bd_source "$BD_SRC" || die "bootstrapped beads-doltlite source is missing cmd/bd: $BD_SRC"
}

bootstrap_doltlite_lib() {
  if [ "$BOOTSTRAP_SOURCES" != "1" ]; then
    return 0
  fi
  if [ -n "$DOLTLITE_LIB" ] && has_doltlite_lib "$DOLTLITE_LIB"; then
    return 0
  fi
  local found
  found="$(find_doltlite_lib || true)"
  if [ -n "$found" ]; then
    DOLTLITE_LIB="$found"
    return 0
  fi

  if [ -z "$DOLTLITE_SRC" ]; then
    DOLTLITE_SRC="$(find_doltlite_source || true)"
  fi
  if [ -z "$DOLTLITE_SRC" ]; then
    DOLTLITE_SRC="$BOOTSTRAP_DIR/doltlite"
    clone_source_if_missing "$DOLTLITE_REPO" "$DOLTLITE_SRC" "DoltLite"
  fi
  has_doltlite_source "$DOLTLITE_SRC" || die "bootstrapped DoltLite source is missing Makefile/main.mk: $DOLTLITE_SRC"

  echo "building libdoltlite from $DOLTLITE_SRC"
  make -C "$DOLTLITE_SRC" doltlite-lib

  for candidate in "$DOLTLITE_SRC" "$DOLTLITE_SRC/build"; do
    if has_doltlite_lib "$candidate"; then
      DOLTLITE_LIB="$(abs_dir "$candidate")"
      return 0
    fi
  done
  die "DoltLite build completed but libdoltlite was not found under $DOLTLITE_SRC"
}

revision_for() {
  local source_dir="$1"
  if command -v jj >/dev/null 2>&1 && [ -d "$source_dir/.jj" ]; then
    (cd "$source_dir" && jj log --no-graph -r @ -T 'commit_id.short()' 2>/dev/null | tr -d '\n' || true)
    return 0
  fi
  if command -v git >/dev/null 2>&1; then
    git -C "$source_dir" rev-parse --short HEAD 2>/dev/null || true
  fi
}

branch_for() {
  local source_dir="$1"
  if command -v git >/dev/null 2>&1; then
    git -C "$source_dir" symbolic-ref --short HEAD 2>/dev/null || true
  fi
}

common_env_prefix() {
  local tags="$1"
  export CGO_ENABLED=1
  export GOFLAGS="${BASE_GOFLAGS:+$BASE_GOFLAGS }-tags=${tags}"
  export CGO_LDFLAGS="${BASE_CGO_LDFLAGS:+$BASE_CGO_LDFLAGS }-L${DOLTLITE_LIB} -Wl,-rpath,${DOLTLITE_LIB} -ldoltlite"
  export LD_LIBRARY_PATH="${DOLTLITE_LIB}${BASE_LD_LIBRARY_PATH:+:${BASE_LD_LIBRARY_PATH}}"
}

verify_linked_binary() {
  local output="$1"
  local name="$2"
  if ! go version -m "$output" 2>/dev/null | grep -q 'CGO_ENABLED=1'; then
    die "built $name binary does not report CGO_ENABLED=1"
  fi
  if command -v ldd >/dev/null 2>&1; then
    if ! ldd "$output" 2>/dev/null | grep -q 'libdoltlite'; then
      die "built $name binary does not appear to link libdoltlite"
    fi
  fi
}

path_under_home() {
  local path="$1"
  [ -n "$path" ] && [ -n "${HOME:-}" ] && [[ "$path" == "$HOME"/* ]]
}

supervisor_gc_path() {
  local unit="gascity-supervisor.service"
  local value path candidate

  for candidate in \
    "${XDG_CONFIG_HOME:-${HOME:-}/.config}/systemd/user/$unit" \
    "${HOME:-}/.local/share/systemd/user/$unit"; do
    if [ -r "$candidate" ]; then
      value="$(awk -F= '$1 == "ExecStart" { print substr($0, index($0, "=") + 1); exit }' "$candidate")"
      if [ -n "$value" ]; then
        case "$value" in
          \"*) path="${value#\"}"; path="${path%%\"*}" ;;
          *) path="${value%% *}" ;;
        esac
        if [ -n "$path" ]; then
          echo "$path"
          return 0
        fi
      fi
    fi
  done

  if command -v systemctl >/dev/null 2>&1; then
    value="$(systemctl --user show "$unit" -p ExecStart --value 2>/dev/null || true)"
    if [ -n "$value" ]; then
      path="${value#*path=}"
      if [ "$path" != "$value" ]; then
        path="${path%% ;*}"
        path="${path%%;*}"
        if [ -n "$path" ]; then
          echo "$path"
          return 0
        fi
      fi
    fi
  fi

  return 1
}

default_install_path() {
  local name="$1"
  local current=""
  if [ -n "$INSTALL_DIR" ]; then
    echo "$INSTALL_DIR/$name"
    return 0
  fi
  if [ "$name" = "gc" ]; then
    current="$(supervisor_gc_path || true)"
    if path_under_home "$current"; then
      echo "$current"
      return 0
    fi
  fi
  current="$(command -v "$name" 2>/dev/null || true)"
  if path_under_home "$current"; then
    echo "$current"
    return 0
  fi
  if [ -n "${HOME:-}" ]; then
    echo "$HOME/.local/bin/$name"
    return 0
  fi
  die "could not choose install path for $name; set --install-dir or --${name}-install"
}

install_binary() {
  local source="$1"
  local dest="$2"
  local name="$3"
  local dest_dir tmp current

  dest_dir="$(dirname "$dest")"
  mkdir -p "$dest_dir"
  tmp="$dest_dir/.${name}.tmp.$$"
  rm -f "$tmp"
  if ! install -m 0755 "$source" "$tmp"; then
    rm -f "$tmp"
    die "installing $name to temporary path failed: $tmp"
  fi
  if ! mv -f "$tmp" "$dest"; then
    rm -f "$tmp"
    die "installing $name failed: $dest"
  fi
  echo "installed $name: $dest"

  current="$(command -v "$name" 2>/dev/null || true)"
  if [ -n "$current" ] && [ "$current" != "$dest" ]; then
    echo "note: current $name resolves to $current; ensure $dest is earlier on PATH"
  fi
}

artifact_path_for_source() {
  local source_dir="$1"
  local output="$2"
  case "$output" in
    /*) echo "$output" ;;
    *) echo "$source_dir/$output" ;;
  esac
}

sha256_for() {
  local path="$1"
  local sum rest
  if command -v sha256sum >/dev/null 2>&1; then
    read -r sum rest < <(sha256sum "$path")
    echo "$sum"
    return 0
  fi
  if command -v shasum >/dev/null 2>&1; then
    read -r sum rest < <(shasum -a 256 "$path")
    echo "$sum"
    return 0
  fi
  die "sha256sum or shasum is required to write build details"
}

json_escape() {
  local value="${1-}"
  value=${value//\\/\\\\}
  value=${value//\"/\\\"}
  value=${value//$'\n'/\\n}
  value=${value//$'\r'/\\r}
  value=${value//$'\t'/\\t}
  printf '%s' "$value"
}

write_build_details() {
  local name="$1"
  local source_dir="$2"
  local output="$3"
  local installed_to="$4"
  local commit="$5"
  local version="$6"
  local branch="$7"
  local tags="$8"
  local built_at="$9"
  local output_path output_sha binary_path binary_sha install_sha stamp tmp go_version go_version_m

  output_path="$(artifact_path_for_source "$source_dir" "$output")"
  output_sha="$(sha256_for "$output_path")"
  binary_path="$output_path"
  binary_sha="$output_sha"
  install_sha=""
  if [ -n "$installed_to" ]; then
    binary_path="$installed_to"
    install_sha="$(sha256_for "$installed_to")"
    binary_sha="$install_sha"
  fi

  go_version="$(go version 2>/dev/null || true)"
  go_version_m="$(go version -m "$binary_path" 2>/dev/null || true)"
  if [ -z "$go_version_m" ] && [ "$binary_path" != "$output_path" ]; then
    go_version_m="$(go version -m "$output_path" 2>/dev/null || true)"
  fi

  mkdir -p "$BUILD_DETAILS_DIR"
  stamp="$BUILD_DETAILS_DIR/last-build-${name}.json"
  tmp="$stamp.tmp.$$"
  rm -f "$tmp"
  {
    printf '{\n'
    printf '  "schema_version": "1",\n'
    printf '  "pack": "beads-doltlite",\n'
    printf '  "target": "%s",\n' "$(json_escape "$name")"
    printf '  "built_at": "%s",\n' "$(json_escape "$built_at")"
    printf '  "source": "%s",\n' "$(json_escape "$source_dir")"
    printf '  "output": "%s",\n' "$(json_escape "$output_path")"
    printf '  "installed_to": "%s",\n' "$(json_escape "$installed_to")"
    printf '  "binary_path": "%s",\n' "$(json_escape "$binary_path")"
    printf '  "sha256": "%s",\n' "$(json_escape "$binary_sha")"
    printf '  "output_sha256": "%s",\n' "$(json_escape "$output_sha")"
    printf '  "install_sha256": "%s",\n' "$(json_escape "$install_sha")"
    printf '  "commit": "%s",\n' "$(json_escape "$commit")"
    printf '  "branch": "%s",\n' "$(json_escape "$branch")"
    printf '  "version": "%s",\n' "$(json_escape "$version")"
    printf '  "tags": "%s",\n' "$(json_escape "$tags")"
    printf '  "doltlite_lib": "%s",\n' "$(json_escape "$DOLTLITE_LIB")"
    printf '  "gocache": "%s",\n' "$(json_escape "${GOCACHE:-}")"
    printf '  "gomodcache": "%s",\n' "$(json_escape "${GOMODCACHE:-}")"
    printf '  "gotmpdir": "%s",\n' "$(json_escape "${GOTMPDIR:-}")"
    printf '  "go_version": "%s",\n' "$(json_escape "$go_version")"
    printf '  "goflags": "%s",\n' "$(json_escape "${GOFLAGS:-}")"
    printf '  "cgo_ldflags": "%s",\n' "$(json_escape "${CGO_LDFLAGS:-}")"
    printf '  "go_version_m": "%s"\n' "$(json_escape "$go_version_m")"
    printf '}\n'
  } >"$tmp"
  mv -f "$tmp" "$stamp"
  echo "wrote $name build details: $stamp"
}

controller_field_for_city() {
  local gc_bin="$1"
  local field="$2"
  local status_file
  status_file="$(mktemp "${TMPDIR:-/tmp}/gc-status.XXXXXX")"
  if ! "$gc_bin" status --json "$CITY_ROOT" >"$status_file" 2>/dev/null && [ ! -s "$status_file" ]; then
    rm -f "$status_file"
    return 0
  fi
  if command -v python3 >/dev/null 2>&1; then
    python3 - "$field" "$status_file" <<'PY'
import json
import sys

field, path = sys.argv[1], sys.argv[2]
try:
    with open(path, "r", encoding="utf-8") as f:
        data = json.load(f)
except Exception:
    sys.exit(0)
value = (data.get("controller") or {}).get(field, "")
if value is None:
    value = ""
print(value)
PY
    rm -f "$status_file"
    return 0
  fi
  awk -v want="$field" '
    /"controller"[[:space:]]*:/ { in_controller=1; next }
    in_controller && /^[[:space:]]*}/ { exit }
    in_controller && $0 ~ "\"" want "\"" {
      value=$0
      sub("^[^:]*:[[:space:]]*", "", value)
      gsub("[,\"]", "", value)
      gsub("^[[:space:]]+|[[:space:]]+$", "", value)
      print value
      exit
    }
  ' "$status_file"
  rm -f "$status_file"
}

controller_mode_for_city() {
  controller_field_for_city "$1" "mode"
}

controller_pid_for_city() {
  controller_field_for_city "$1" "pid"
}

process_alive() {
  local pid="$1"
  [ -n "$pid" ] && kill -0 "$pid" >/dev/null 2>&1
}

wait_for_pid_exit() {
  local pid="$1"
  local remaining="$RESTART_WAIT_SECONDS"
  while [ "$remaining" -gt 0 ]; do
    if ! process_alive "$pid"; then
      return 0
    fi
    sleep 1
    remaining=$((remaining - 1))
  done
  ! process_alive "$pid"
}

terminate_controller_pid() {
  local pid="$1"
  if ! process_alive "$pid"; then
    return 0
  fi
  echo "stopping leftover city controller PID $pid"
  kill "$pid" >/dev/null 2>&1 || true
  if wait_for_pid_exit "$pid"; then
    return 0
  fi
  echo "force-stopping leftover city controller PID $pid"
  kill -KILL "$pid" >/dev/null 2>&1 || true
  wait_for_pid_exit "$pid" || die "city controller PID $pid did not stop"
}

stop_standalone_controller_if_running() {
  local gc_bin="$1"
  local mode pid
  mode="$(controller_mode_for_city "$gc_bin" || true)"
  if [ "$mode" != "standalone" ]; then
    return 0
  fi
  pid="$(controller_pid_for_city "$gc_bin" || true)"
  echo "stopping standalone city controller${pid:+ PID $pid}"
  "$gc_bin" stop "$CITY_ROOT" --force --timeout "${RESTART_WAIT_SECONDS}s" || die "stopping standalone city controller failed"
  if [ -n "$pid" ]; then
    terminate_controller_pid "$pid"
  fi
}

stop_supervisor_if_running() {
  local gc_bin="$1"
  if ! "$gc_bin" supervisor status >/dev/null 2>&1; then
    echo "supervisor already stopped"
    return 0
  fi
  echo "stopping supervisor"
  "$gc_bin" supervisor stop --wait --wait-timeout "${RESTART_WAIT_SECONDS}s" || die "stopping supervisor failed"
  if "$gc_bin" supervisor status >/dev/null 2>&1; then
    die "supervisor still running after stop"
  fi
}

stop_leftover_controller_if_running() {
  local gc_bin="$1"
  local pid
  pid="$(controller_pid_for_city "$gc_bin" || true)"
  if [ -z "$pid" ]; then
    return 0
  fi
  terminate_controller_pid "$pid"
}

current_gc_for_stop() {
  if [ -n "$GC_INSTALL" ] && [ -x "$GC_INSTALL" ]; then
    echo "$GC_INSTALL"
    return 0
  fi
  if command -v gc >/dev/null 2>&1; then
    command -v gc
    return 0
  fi
  return 1
}

target_includes_gc() {
  [ "$TARGET" = "gc" ] || [ "$TARGET" = "all" ]
}

prepare_gc_install_path() {
  if [ "$INSTALL_BUILT" != "1" ] || [ "$RESTART_AFTER_INSTALL" != "1" ] || ! target_includes_gc; then
    return 0
  fi
  if [ -z "$GC_INSTALL" ]; then
    GC_INSTALL="$(default_install_path gc)"
  fi
}

stop_before_gc_build() {
  if [ "$INSTALL_BUILT" != "1" ] || [ "$RESTART_AFTER_INSTALL" != "1" ] || ! target_includes_gc; then
    return 0
  fi
  local gc_bin
  gc_bin="$(current_gc_for_stop || true)"
  if [ -z "$gc_bin" ]; then
    die "restart requires an existing gc binary to stop current services; pass --no-restart to build without cycling services"
  fi

  echo "stopping gc services before build with $gc_bin"
  stop_standalone_controller_if_running "$gc_bin"
  stop_supervisor_if_running "$gc_bin"
  stop_leftover_controller_if_running "$gc_bin"
  GC_SERVICES_STOPPED_FOR_BUILD=1
}

start_after_gc_install() {
  local gc_bin="$1"
  if [ "$RESTART_AFTER_INSTALL" != "1" ]; then
    echo "restart after gc install disabled"
    return 0
  fi
  if [ ! -x "$gc_bin" ]; then
    die "installed gc is not executable: $gc_bin"
  fi

  if [ "$GC_SERVICES_STOPPED_FOR_BUILD" != "1" ]; then
    die "refusing to start gc services because they were not stopped before build"
  fi

  echo "starting city from $CITY_ROOT"
  "$gc_bin" start "$CITY_ROOT" || die "starting city after gc install failed"
}

build_gc() {
  if [ -z "$GASCITY_SRC" ]; then
    GASCITY_SRC="$(find_gascity_source || true)"
  fi
  if [ -z "$GASCITY_SRC" ] || ! has_gascity_source "$GASCITY_SRC"; then
    die "could not find Gas City source; set GASCITY_SRC=/path/to/gascity or pass --gc-source"
  fi
  GASCITY_SRC="$(abs_dir "$GASCITY_SRC")"

  if [ -z "$GC_OUTPUT" ]; then
    GC_OUTPUT="$GASCITY_SRC/bin/gc"
  fi

  local commit="${GC_COMMIT:-}"
  if [ -z "$commit" ]; then
    commit="$(revision_for "$GASCITY_SRC")"
  fi
  commit="${commit:-unknown}"
  local date="${GC_BUILD_DATE:-$(date -u +%Y-%m-%dT%H:%M:%SZ)}"

  mkdir -p "$(dirname "$GC_OUTPUT")"
  common_env_prefix "gascity_doltlite_lib,libsqlite3"

  echo "building gc from $GASCITY_SRC"
  echo "linking libdoltlite from $DOLTLITE_LIB"
  echo "writing $GC_OUTPUT"

  (
    cd "$GASCITY_SRC"
    go build \
      -ldflags "-X main.version=${VERSION} -X main.commit=${commit} -X main.date=${date}" \
      -o "$GC_OUTPUT" \
      ./cmd/gc
  )

  verify_linked_binary "$GC_OUTPUT" "gc"
  echo "built libdoltlite-linked gc: $GC_OUTPUT"

  local installed_to=""
  if [ "$INSTALL_BUILT" = "1" ]; then
    if [ -z "$GC_INSTALL" ]; then
      GC_INSTALL="$(default_install_path gc)"
    fi
    install_binary "$GC_OUTPUT" "$GC_INSTALL" "gc"
    installed_to="$GC_INSTALL"
  fi
  write_build_details "gc" "$GASCITY_SRC" "$GC_OUTPUT" "$installed_to" "$commit" "$VERSION" "" "gascity_doltlite_lib,libsqlite3" "$date"
  if [ -n "$installed_to" ]; then
    start_after_gc_install "$installed_to"
    write_build_details "gc" "$GASCITY_SRC" "$GC_OUTPUT" "$installed_to" "$commit" "$VERSION" "" "gascity_doltlite_lib,libsqlite3" "$date"
  fi
}

build_bd() {
  if [ -z "$BD_SRC" ]; then
    BD_SRC="$(find_bd_source || true)"
  fi
  if [ -z "$BD_SRC" ] || ! has_bd_source "$BD_SRC"; then
    die "could not find beads-doltlite source; set BD_SRC=/path/to/beads-doltlite or pass --bd-source"
  fi
  BD_SRC="$(abs_dir "$BD_SRC")"

  if [ -z "$BD_OUTPUT" ]; then
    BD_OUTPUT="$BD_SRC/bin/bd"
  fi

  local commit="${BD_COMMIT:-}"
  if [ -z "$commit" ]; then
    commit="$(revision_for "$BD_SRC")"
  fi
  commit="${commit:-unknown}"
  local branch="${BD_BRANCH:-}"
  if [ -z "$branch" ]; then
    branch="$(branch_for "$BD_SRC")"
  fi
  local date
  date="$(date -u +%Y-%m-%dT%H:%M:%SZ)"
  local ldflags="-X main.Build=${BD_BUILD_VALUE} -X main.Commit=${commit}"
  if [ -n "$branch" ]; then
    ldflags="${ldflags} -X main.Branch=${branch}"
  fi

  mkdir -p "$(dirname "$BD_OUTPUT")"
  common_env_prefix "libsqlite3"

  echo "building bd from $BD_SRC"
  echo "linking libdoltlite from $DOLTLITE_LIB"
  echo "writing $BD_OUTPUT"

  (
    cd "$BD_SRC"
    go build \
      -ldflags "$ldflags" \
      -o "$BD_OUTPUT" \
      ./cmd/bd
  )

  verify_linked_binary "$BD_OUTPUT" "bd"
  echo "built libdoltlite-linked bd: $BD_OUTPUT"

  local installed_to=""
  if [ "$INSTALL_BUILT" = "1" ]; then
    if [ -z "$BD_INSTALL" ]; then
      BD_INSTALL="$(default_install_path bd)"
    fi
    install_binary "$BD_OUTPUT" "$BD_INSTALL" "bd"
    installed_to="$BD_INSTALL"
  fi
  write_build_details "bd" "$BD_SRC" "$BD_OUTPUT" "$installed_to" "$commit" "$BD_BUILD_VALUE" "$branch" "libsqlite3" "$date"
}

build_client() {
  if [ -z "$GASCITY_SRC" ]; then
    GASCITY_SRC="$(find_gascity_source || true)"
  fi
  if [ -z "$GASCITY_SRC" ] || ! has_gascity_source "$GASCITY_SRC"; then
    die "could not find Gas City source; set GASCITY_SRC=/path/to/gascity or pass --gc-source"
  fi
  GASCITY_SRC="$(abs_dir "$GASCITY_SRC")"

  if [ ! -f "$GASCITY_SRC/tools/doltlite-client/main.go" ]; then
    die "could not find doltlite-client source under $GASCITY_SRC/tools/doltlite-client"
  fi

  if [ -z "$CLIENT_OUTPUT" ]; then
    CLIENT_OUTPUT="$BUILD_DETAILS_DIR/bin/doltlite-client"
  fi

  local commit="${GC_COMMIT:-}"
  if [ -z "$commit" ]; then
    commit="$(revision_for "$GASCITY_SRC")"
  fi
  commit="${commit:-unknown}"
  local date="${GC_BUILD_DATE:-$(date -u +%Y-%m-%dT%H:%M:%SZ)}"

  mkdir -p "$(dirname "$CLIENT_OUTPUT")"
  common_env_prefix "gascity_doltlite_lib,libsqlite3"

  echo "building doltlite-client from $GASCITY_SRC"
  echo "linking libdoltlite from $DOLTLITE_LIB"
  echo "writing $CLIENT_OUTPUT"

  (
    cd "$GASCITY_SRC"
    go build \
      -o "$CLIENT_OUTPUT" \
      ./tools/doltlite-client
  )

  verify_linked_binary "$CLIENT_OUTPUT" "doltlite-client"
  echo "built libdoltlite-linked doltlite-client: $CLIENT_OUTPUT"
  write_build_details "doltlite-client" "$GASCITY_SRC" "$CLIENT_OUTPUT" "$CLIENT_OUTPUT" "$commit" "dev" "" "gascity_doltlite_lib,libsqlite3" "$date"
}

CITY_ROOT="${GC_CITY_PATH:-$(pwd)}"
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PACK_DIR="$(cd "$SCRIPT_DIR/../.." && pwd)"
SCRIPT_CHECKOUT="$(cd "$SCRIPT_DIR/../../../.." && pwd)"
BASE_GOFLAGS="${GOFLAGS:-}"
BASE_CGO_LDFLAGS="${CGO_LDFLAGS:-}"
BASE_LD_LIBRARY_PATH="${LD_LIBRARY_PATH:-}"

TARGET="gc"
COMMON_SOURCE=""
COMMON_OUTPUT="${OUTPUT:-}"
GASCITY_SRC="${GASCITY_SRC:-${GC_GASCITY_SRC:-}}"
BD_SRC="${BD_SRC:-${BEADS_DOLTLITE_SRC:-${GC_BEADS_DOLTLITE_SRC:-}}}"
DOLTLITE_LIB="${DOLTLITE_LIB:-${GC_DOLTLITE_LIB:-}}"
DOLTLITE_SRC="${DOLTLITE_SRC:-${GC_DOLTLITE_SRC:-}}"
DOLTLITE_REPO="${GC_DOLTLITE_REPO:-https://github.com/dolthub/doltlite.git}"
BD_REPO="${GC_BEADS_DOLTLITE_REPO:-https://github.com/duncan4123/beads-doltlite.git}"
BOOTSTRAP_SOURCES="${GC_DOLTLITE_BOOTSTRAP_SOURCES:-1}"
BOOTSTRAP_DIR="${GC_DOLTLITE_BOOTSTRAP_DIR:-}"
GC_OUTPUT="${GC_DOLTLITE_GC_OUTPUT:-}"
BD_OUTPUT="${BD_OUTPUT:-${GC_DOLTLITE_BD_OUTPUT:-}}"
CLIENT_OUTPUT="${GC_DOLTLITE_CLIENT_OUTPUT:-}"
INSTALL_BUILT="${GC_DOLTLITE_INSTALL:-0}"
INSTALL_DIR="${GC_DOLTLITE_INSTALL_DIR:-}"
GC_INSTALL="${GC_DOLTLITE_GC_INSTALL:-}"
BD_INSTALL="${GC_DOLTLITE_BD_INSTALL:-}"
BUILD_DETAILS_DIR="${GC_DOLTLITE_BUILD_DETAILS_DIR:-}"
GO_CACHE_ROOT="${GC_DOLTLITE_GO_CACHE_ROOT:-}"
RESTART_AFTER_INSTALL="${GC_DOLTLITE_RESTART_AFTER_INSTALL:-1}"
RESTART_WAIT_SECONDS="${GC_DOLTLITE_RESTART_WAIT_SECONDS:-180}"
GC_SERVICES_STOPPED_FOR_BUILD=0
VERSION="${GC_VERSION:-dev}"
BD_BUILD_VALUE="${BD_BUILD:-dev}"

while [ "$#" -gt 0 ]; do
  case "$1" in
    gc|bd|client|all)
      TARGET="$1"
      shift
      ;;
    --target)
      require_value "$1" "${2:-}"
      TARGET="$2"
      shift 2
      ;;
    --target=*)
      TARGET="${1#*=}"
      shift
      ;;
    --source)
      require_value "$1" "${2:-}"
      COMMON_SOURCE="$2"
      shift 2
      ;;
    --source=*)
      COMMON_SOURCE="${1#*=}"
      shift
      ;;
    --gc-source)
      require_value "$1" "${2:-}"
      GASCITY_SRC="$2"
      shift 2
      ;;
    --gc-source=*)
      GASCITY_SRC="${1#*=}"
      shift
      ;;
    --bd-source)
      require_value "$1" "${2:-}"
      BD_SRC="$2"
      shift 2
      ;;
    --bd-source=*)
      BD_SRC="${1#*=}"
      shift
      ;;
    --lib)
      require_value "$1" "${2:-}"
      DOLTLITE_LIB="$2"
      shift 2
      ;;
    --lib=*)
      DOLTLITE_LIB="${1#*=}"
      shift
      ;;
    --doltlite-source)
      require_value "$1" "${2:-}"
      DOLTLITE_SRC="$2"
      shift 2
      ;;
    --doltlite-source=*)
      DOLTLITE_SRC="${1#*=}"
      shift
      ;;
    --doltlite-repo)
      require_value "$1" "${2:-}"
      DOLTLITE_REPO="$2"
      shift 2
      ;;
    --doltlite-repo=*)
      DOLTLITE_REPO="${1#*=}"
      shift
      ;;
    --bd-repo)
      require_value "$1" "${2:-}"
      BD_REPO="$2"
      shift 2
      ;;
    --bd-repo=*)
      BD_REPO="${1#*=}"
      shift
      ;;
    --bootstrap-dir)
      require_value "$1" "${2:-}"
      BOOTSTRAP_DIR="$2"
      shift 2
      ;;
    --bootstrap-dir=*)
      BOOTSTRAP_DIR="${1#*=}"
      shift
      ;;
    --no-bootstrap)
      BOOTSTRAP_SOURCES=0
      shift
      ;;
    --output)
      require_value "$1" "${2:-}"
      COMMON_OUTPUT="$2"
      shift 2
      ;;
    --output=*)
      COMMON_OUTPUT="${1#*=}"
      shift
      ;;
    --gc-output)
      require_value "$1" "${2:-}"
      GC_OUTPUT="$2"
      shift 2
      ;;
    --gc-output=*)
      GC_OUTPUT="${1#*=}"
      shift
      ;;
    --bd-output)
      require_value "$1" "${2:-}"
      BD_OUTPUT="$2"
      shift 2
      ;;
    --bd-output=*)
      BD_OUTPUT="${1#*=}"
      shift
      ;;
    --client-output)
      require_value "$1" "${2:-}"
      CLIENT_OUTPUT="$2"
      shift 2
      ;;
    --client-output=*)
      CLIENT_OUTPUT="${1#*=}"
      shift
      ;;
    --install)
      INSTALL_BUILT=1
      shift
      ;;
    --install-dir)
      require_value "$1" "${2:-}"
      INSTALL_DIR="$2"
      INSTALL_BUILT=1
      shift 2
      ;;
    --install-dir=*)
      INSTALL_DIR="${1#*=}"
      INSTALL_BUILT=1
      shift
      ;;
    --gc-install)
      require_value "$1" "${2:-}"
      GC_INSTALL="$2"
      INSTALL_BUILT=1
      shift 2
      ;;
    --gc-install=*)
      GC_INSTALL="${1#*=}"
      INSTALL_BUILT=1
      shift
      ;;
    --bd-install)
      require_value "$1" "${2:-}"
      BD_INSTALL="$2"
      INSTALL_BUILT=1
      shift 2
      ;;
    --bd-install=*)
      BD_INSTALL="${1#*=}"
      INSTALL_BUILT=1
      shift
      ;;
    --build-details-dir)
      require_value "$1" "${2:-}"
      BUILD_DETAILS_DIR="$2"
      shift 2
      ;;
    --build-details-dir=*)
      BUILD_DETAILS_DIR="${1#*=}"
      shift
      ;;
    --restart)
      RESTART_AFTER_INSTALL=1
      shift
      ;;
    --no-restart)
      RESTART_AFTER_INSTALL=0
      shift
      ;;
    --version)
      require_value "$1" "${2:-}"
      VERSION="$2"
      shift 2
      ;;
    --version=*)
      VERSION="${1#*=}"
      shift
      ;;
    --bd-build)
      require_value "$1" "${2:-}"
      BD_BUILD_VALUE="$2"
      shift 2
      ;;
    --bd-build=*)
      BD_BUILD_VALUE="${1#*=}"
      shift
      ;;
    -h|--help)
      usage
      exit 0
      ;;
    *)
      usage_error "unknown argument: $1"
      ;;
  esac
done

case "$TARGET" in
  gc|bd|client|all) ;;
  *) usage_error "unknown target: $TARGET" ;;
esac

if [ -n "$COMMON_SOURCE" ]; then
  case "$TARGET" in
    gc) GASCITY_SRC="$COMMON_SOURCE" ;;
    bd) BD_SRC="$COMMON_SOURCE" ;;
    client) GASCITY_SRC="$COMMON_SOURCE" ;;
    all) usage_error "--source is ambiguous with target all; use --gc-source and --bd-source" ;;
  esac
fi

if [ -n "$COMMON_OUTPUT" ]; then
  case "$TARGET" in
    gc) GC_OUTPUT="$COMMON_OUTPUT" ;;
    bd) BD_OUTPUT="$COMMON_OUTPUT" ;;
    client) CLIENT_OUTPUT="$COMMON_OUTPUT" ;;
    all) usage_error "--output is ambiguous with target all; use --gc-output, --bd-output, and --client-output" ;;
  esac
fi

case "${INSTALL_BUILT,,}" in
  1|true|yes|on) INSTALL_BUILT=1 ;;
  ""|0|false|no|off) INSTALL_BUILT=0 ;;
  *) usage_error "GC_DOLTLITE_INSTALL must be true or false" ;;
esac

case "${BOOTSTRAP_SOURCES,,}" in
  1|true|yes|on) BOOTSTRAP_SOURCES=1 ;;
  ""|0|false|no|off) BOOTSTRAP_SOURCES=0 ;;
  *) usage_error "GC_DOLTLITE_BOOTSTRAP_SOURCES must be true or false" ;;
esac

case "${RESTART_AFTER_INSTALL,,}" in
  1|true|yes|on) RESTART_AFTER_INSTALL=1 ;;
  ""|0|false|no|off) RESTART_AFTER_INSTALL=0 ;;
  *) usage_error "GC_DOLTLITE_RESTART_AFTER_INSTALL must be true or false" ;;
esac

case "$RESTART_WAIT_SECONDS" in
  ''|*[!0-9]*) usage_error "GC_DOLTLITE_RESTART_WAIT_SECONDS must be a positive integer" ;;
  0) usage_error "GC_DOLTLITE_RESTART_WAIT_SECONDS must be greater than zero" ;;
esac

if [ -z "$BOOTSTRAP_DIR" ]; then
  BOOTSTRAP_DIR="$CITY_ROOT/.cache/beads-doltlite/sources"
fi
mkdir -p "$BOOTSTRAP_DIR"

bootstrap_bd_source
bootstrap_doltlite_lib

if [ -z "$DOLTLITE_LIB" ]; then
  DOLTLITE_LIB="$(find_doltlite_lib || true)"
fi
if [ -z "$DOLTLITE_LIB" ] || ! has_doltlite_lib "$DOLTLITE_LIB"; then
  die "could not find libdoltlite; set DOLTLITE_LIB=/path/to/doltlite-work/build, pass --lib, or leave bootstrapping enabled"
fi
DOLTLITE_LIB="$(abs_dir "$DOLTLITE_LIB")"

if [ -z "$BUILD_DETAILS_DIR" ]; then
  BUILD_DETAILS_DIR="${GC_PACK_STATE_DIR:-$CITY_ROOT/.gc/runtime/packs/beads-doltlite}"
fi
if [ -z "$GO_CACHE_ROOT" ]; then
  GO_CACHE_ROOT="$CITY_ROOT/.cache/go"
fi
if [ -z "${GOCACHE:-}" ]; then
  export GOCACHE="$GO_CACHE_ROOT/build"
fi
if [ -z "${GOMODCACHE:-}" ]; then
  export GOMODCACHE="$GO_CACHE_ROOT/mod"
fi
if [ -z "${GOTMPDIR:-}" ]; then
  export GOTMPDIR="$GO_CACHE_ROOT/tmp"
fi
mkdir -p "$GOCACHE" "$GOMODCACHE" "$GOTMPDIR"

if ! command -v go >/dev/null 2>&1; then
  die "go is required to build DoltLite-linked binaries"
fi

prepare_gc_install_path
stop_before_gc_build

case "$TARGET" in
  gc)
    build_gc
    ;;
  bd)
    build_bd
    ;;
  client)
    build_client
    ;;
  all)
    build_bd
    build_client
    build_gc
    ;;
esac
