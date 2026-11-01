#!/bin/sh
# Build the standalone bd from the Beads version linked by this GC checkout.
# Scratch module isolation keeps cmd/bd-only dependencies out of GC's go.mod.
# An explicit install directory is the only destination this script changes.
set -eu
repo_root=$(CDPATH='' cd -- "$(dirname -- "$0")/.." && pwd)
install_dir=${1:?usage: install-bd-pinned.sh <install-dir>}
installed="$install_dir/bd"

beads_module_pin() {
    metadata=$(go version -m "$1" 2>/dev/null) || return 1
    printf '%s\n' "$metadata" | awk '
        ($1 == "mod" || $1 == "dep") && $2 == "github.com/steveyegge/beads" {
            version = $3
            count++
            if (getline > 0 && $1 == "=>") replaced = 1
        }
        END {
            if (count != 1 || replaced || version == "(devel)") exit 1
            print version
        }'
}

pin=$(cd "$repo_root" && go list -m -f '{{.Version}}{{if .Replace}} replaced{{end}}' github.com/steveyegge/beads)
case "$pin" in
    ''|*' '*|*'
'*) echo 'ERROR: go.mod must name an unreplaced Beads module version' >&2; exit 1 ;;
esac
if [ -x "$installed" ] && current=$(beads_module_pin "$installed") && [ "$current" = "$pin" ]; then
    echo "bd already embeds pinned Beads $pin; skipping rebuild"
    exit 0
fi

scratch=$(mktemp -d /var/tmp/gc-install-bd.XXXXXX)
tmp="$install_dir/.bd.tmp.$$"
trap 'rm -rf "$scratch"; rm -f "$tmp"' EXIT HUP INT TERM
(
    cd "$scratch"
    go mod init bdinstall >/dev/null
    go get "github.com/steveyegge/beads/cmd/bd@$pin"
    go build -o bd github.com/steveyegge/beads/cmd/bd
)
if ! built=$(beads_module_pin "$scratch/bd") || [ "$built" != "$pin" ]; then
    echo "ERROR: built bd does not embed unreplaced Beads $pin; installed binary preserved" >&2
    exit 1
fi
mkdir -p "$install_dir"
cp -f "$scratch/bd" "$tmp"
chmod 0755 "$tmp"
mv -f "$tmp" "$installed"
echo "Installed bd with Beads $pin to $installed"
