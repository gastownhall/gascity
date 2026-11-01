#!/bin/sh
# Installer protocol fixtures: no network, module download or installed host tools are changed.
set -eu
installer=$1
if [ "${installer#/}" = "$installer" ]; then installer="$PWD/$installer"; fi
sandbox=$(mktemp -d "${TEST_TMPDIR:-/tmp}/gc-bd-installer.XXXXXX")
trap 'rm -rf "$sandbox"' EXIT HUP INT TERM
mkdir -p "$sandbox/tools" "$sandbox/home/.local/bin" "$sandbox/install"
test_home="$sandbox/home"
export PATH="$sandbox/tools:$PATH"
export BD_TEST_PIN=v1.3.2
export BD_TEST_LOG="$sandbox/go.log"
export BD_TEST_INFO="$sandbox/build-info"
cat > "$sandbox/tools/go" <<'GO'
#!/bin/sh
set -eu
printf '%s\n' "$*" >> "$BD_TEST_LOG"
case "$1 $2" in
 'list -m') printf '%s\n' "$BD_TEST_PIN" ;;
 'version -m') cat "$3" ;;
 'mod init') : ;;
 'get github.com/steveyegge/beads/cmd/bd@v1.3.2') : ;;
 'build -o')
   if [ "${BD_TEST_FAIL_BUILD:-0}" = 1 ]; then exit 7; fi
   cp "$BD_TEST_INFO" "$3"
   chmod 755 "$3" ;;
 *) printf 'unexpected Go invocation: %s\n' "$*" >&2; exit 19 ;;
esac
GO
chmod 755 "$sandbox/tools/go"
printf 'unrelated installation\n' > "$test_home/.local/bin/bd"
printf '\tdep\tgithub.com/steveyegge/beads\tv1.3.2\th1:fixture\n' > "$BD_TEST_INFO"

printf '[1/5] builds the pinned module into an explicit prefix\n'
env HOME="$test_home" sh "$installer" "$sandbox/install"
cmp "$BD_TEST_INFO" "$sandbox/install/bd"
test "$(cat "$test_home/.local/bin/bd")" = 'unrelated installation'
grep -F 'get github.com/steveyegge/beads/cmd/bd@v1.3.2' "$BD_TEST_LOG"

printf '[2/5] matching dependency or main-module metadata skips rebuilding\n'
: > "$BD_TEST_LOG"
env HOME="$test_home" sh "$installer" "$sandbox/install"
if grep -E '^(get|build)' "$BD_TEST_LOG"; then exit 1; fi
printf '\tmod\tgithub.com/steveyegge/beads\tv1.3.2\th1:fixture\n' > "$sandbox/install/bd"
: > "$BD_TEST_LOG"
env HOME="$test_home" sh "$installer" "$sandbox/install"
if grep -E '^(get|build)' "$BD_TEST_LOG"; then exit 1; fi

printf '[3/5] a failed build preserves the installed binary\n'
printf 'old installation\n' > "$sandbox/install/bd"
export BD_TEST_FAIL_BUILD=1
if env HOME="$test_home" sh "$installer" "$sandbox/install"; then exit 1; fi
test "$(cat "$sandbox/install/bd")" = 'old installation'
unset BD_TEST_FAIL_BUILD

printf '[4/5] wrong built module metadata is refused before replacement\n'
printf '\tdep\tgithub.com/steveyegge/beads\tv1.2.0\th1:fixture\n' > "$BD_TEST_INFO"
if env HOME="$test_home" sh "$installer" "$sandbox/install"; then exit 1; fi
test "$(cat "$sandbox/install/bd")" = 'old installation'

printf '[5/5] replacement-module metadata cannot masquerade as the pin\n'
printf '\tdep\tgithub.com/steveyegge/beads\tv1.3.2\th1:fixture\n\t=>\t/tmp/replaced\t(devel)\n' > "$BD_TEST_INFO"
if env HOME="$test_home" sh "$installer" "$sandbox/install"; then exit 1; fi
test "$(cat "$sandbox/install/bd")" = 'old installation'
test "$(cat "$test_home/.local/bin/bd")" = 'unrelated installation'
