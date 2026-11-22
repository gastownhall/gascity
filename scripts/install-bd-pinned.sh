#!/usr/bin/env bash
# install-bd-pinned.sh DEST
#
# Installs, at DEST, a bd built from exactly the beads module version
# gascity's go.mod pins: the module gc links, and whose schema gc migrates a
# city store to. A bd from any other beads version fails every read of that
# store with "schema version mismatch".
#
# The build is `go install pkg@version` run outside the repository with
# GOFLAGS cleared and GOWORK=off, so gascity's go.mod, go.sum and any go.work
# take no part in it and are never written. bd is built from source rather
# than fetched from a release archive because go.mod may pin a pseudo-version
# that no release contains.
#
# The new binary replaces DEST through a temp file and a rename, and the
# ~/.local/bin/bd symlink is pointed at it the way `make install` does for gc.
set -euo pipefail

if [ "$#" -ne 1 ] || [ -z "$1" ]; then
	echo "usage: scripts/install-bd-pinned.sh <destination bd path>" >&2
	exit 2
fi

readonly beads_module=github.com/steveyegge/beads
dest=$1
install_dir=$(dirname "$dest")
repo_root=$(cd "$(dirname "$0")/.." && pwd)

stage=""
tmp=""
cleanup() {
	if [ -n "$tmp" ]; then rm -f "$tmp"; fi
	if [ -n "$stage" ]; then rm -rf "$stage"; fi
}
trap cleanup EXIT
trap 'exit 1' INT TERM HUP

# pinned_beads_version prints the beads version go.mod requires. It reads
# go.mod alone (`go mod edit -json`), never the module proxy, and refuses a
# go.mod that replaces beads: the replacement, not the pin, is what gc links.
pinned_beads_version() {
	local json require replace version
	json=$(go mod edit -json "$repo_root/go.mod" | tr -d ' \t\r\n')
	case "$json" in
	*'"Replace":['*)
		replace=${json#*\"Replace\":\[}
		if printf '%s' "$replace" | grep -Eq '"Old":\{"Path":"github\.com/steveyegge/beads"[,}]'; then
			echo "ERROR: go.mod replaces $beads_module; gc links the replacement, so no pinned bd matches it. Build bd from that replacement instead." >&2
			return 1
		fi
		;;
	esac
	require=${json#*\"Require\":\[}
	require=${require%%]*}
	version=$(printf '%s' "$require" | tr '{}' '\n\n' |
		grep -F "\"Path\":\"$beads_module\"," |
		sed -n 's/.*"Version":"\([^"]*\)".*/\1/p' | head -n 1)
	if [ -z "$version" ]; then
		echo "ERROR: $repo_root/go.mod does not require $beads_module" >&2
		return 1
	fi
	printf '%s\n' "$version"
}

# embedded_beads_version prints the beads version a bd binary was built
# from: the version of its main module when that module is beads, as `go
# version -m` reports it. It prints nothing for a file that is missing, not a
# Go binary, or built from some other main module.
embedded_beads_version() {
	[ -f "$1" ] || return 0
	go version -m "$1" 2>/dev/null |
		awk -v m="$beads_module" '$1 == "mod" && $2 == m { print $3; exit }'
}

pin=$(pinned_beads_version)

if [ "$(embedded_beads_version "$dest")" = "$pin" ]; then
	echo "bd at $dest is already built from $beads_module $pin; nothing to do"
	exit 0
fi

mkdir -p "$install_dir"
stage=$(mktemp -d "${TMPDIR:-/tmp}/gc-install-bd.XXXXXX")
echo "Building bd from $beads_module@$pin"
(
	cd "$stage"
	GOFLAGS='' GOWORK=off CGO_ENABLED=1 GOBIN="$stage" \
		go install -tags gms_pure_go "$beads_module/cmd/bd@$pin"
)

tmp="$install_dir/.bd.tmp.$$"
cp -f "$stage/bd" "$tmp"
chmod 0755 "$tmp"
mv -f "$tmp" "$dest"
tmp=""

# Migrate from the old install location the way `make install` does for gc:
# replace ~/.local/bin/bd with a symlink to the installed binary.
if [ "$install_dir" != "$HOME/.local/bin" ]; then
	if [ -f "$HOME/.local/bin/bd" ] || [ -L "$HOME/.local/bin/bd" ]; then
		rm -f "$HOME/.local/bin/bd"
	fi
	if [ -d "$HOME/.local/bin" ]; then
		ln -sf "$dest" "$HOME/.local/bin/bd"
		echo "Symlinked $HOME/.local/bin/bd -> $dest"
	fi
fi
echo "Installed bd ($beads_module $pin) to $dest"
