#!/usr/bin/env sh
# install-bd-pinned.sh <install-dir>
#
# Build and install a bd whose beads module version matches the commit this
# repo's go.mod pins, so an installed gc and the standalone bd on PATH always
# share one schema catalog. gc migrates a city store forward on first write;
# a bd built from any other commit then fails every read with a schema-version
# mismatch, and every bare-bd caller on the host stops working.
#
# The build runs in a scratch module, never in the gascity module: gascity's
# go.sum has no entries for bd's cmd-only dependencies, so building bd there
# mutates go.sum as a side effect. The scratch lives under /var/tmp, not /tmp:
# /tmp is often a size-capped tmpfs that a module download and build can fill.
set -eu

repo_root=$(CDPATH='' cd -- "$(dirname -- "$0")/.." && pwd)
install_dir=${1:?usage: install-bd-pinned.sh <install-dir>}
installed="$install_dir/bd"

# beads_module_pin prints the beads module version a bd binary embeds, or
# nothing when the file is not a module-built Go binary.
beads_module_pin() {
    go version -m "$1" 2>/dev/null |
        awk '$1 == "mod" && $2 == "github.com/steveyegge/beads" {print $3}'
}

# go.mod is what gc actually compiles against, so it is the source of truth
# for the pin (deps.env BD_CURRENT_VERSION is kept in lockstep with it by
# scripts/bd_version_pin_test.go).
pin=$(cd "$repo_root" && go list -m -f '{{.Version}}' github.com/steveyegge/beads)
if [ -z "$pin" ]; then
    echo "ERROR: could not resolve the beads pin from go.mod" >&2
    exit 1
fi

# Mirror the gc install target's ~/.local/bin migration: agents resolve bd
# from PATH, and a stale (or dangling) binary there re-creates the exact skew
# this script exists to prevent. Runs on the fast path too, so a broken
# symlink is repaired even when no rebuild is needed.
link_local_bin() {
    if [ "$install_dir" != "$HOME/.local/bin" ] && [ -d "$HOME/.local/bin" ]; then
        if [ -e "$HOME/.local/bin/bd" ] || [ -L "$HOME/.local/bin/bd" ]; then
            rm -f "$HOME/.local/bin/bd"
        fi
        ln -sf "$installed" "$HOME/.local/bin/bd"
        echo "Symlinked $HOME/.local/bin/bd -> $installed"
    fi
}

# Fast path: the installed bd already embeds the pinned beads version.
if [ -x "$installed" ] && [ "$(beads_module_pin "$installed")" = "$pin" ]; then
    echo "bd already built from pinned beads $pin; skipping rebuild"
    link_local_bin
    exit 0
fi

scratch=$(mktemp -d /var/tmp/gc-install-bd.XXXXXX)
tmp="$install_dir/.bd.tmp.$$"
trap 'rm -rf "$scratch" "$tmp"' EXIT INT TERM HUP

(
    cd "$scratch"
    go mod init bdinstall >/dev/null
    go get "github.com/steveyegge/beads/cmd/bd@$pin"
    go build -o bd github.com/steveyegge/beads/cmd/bd
)

built=$(beads_module_pin "$scratch/bd")
if [ "$built" != "$pin" ]; then
    echo "ERROR: built bd embeds beads ${built:-<none>}, want pinned $pin" >&2
    exit 1
fi

mkdir -p "$install_dir"
cp -f "$scratch/bd" "$tmp"
chmod 0755 "$tmp"
mv -f "$tmp" "$installed"
echo "Installed bd ($pin) to $installed"
link_local_bin
