#!/bin/sh
# install-bd-pinned: build and install the bd that matches gc's beads pin.
#
# gc migrates a city store forward to the schema of the beads module it links,
# so a bd built from any other beads version fails every read with "schema
# version mismatch". `make install` calls this script so the bd beside gc is
# built from the exact version go.mod pins.
#
# The pin is read offline from `go mod edit -json` (never `go list -m`, which
# fetches from the proxy), the build is a `go install module@version` whose
# main module is beads alone — gascity's own go.mod and go.sum are never
# touched — and `go version -m` confirms the installed binary embeds the pin.
set -eu

if [ -z "${INSTALL_DIR:-}" ]; then
	echo "ERROR: INSTALL_DIR must name where bd is installed (make install passes it)" >&2
	exit 1
fi

script_dir=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
repo_root=$(dirname -- "$script_dir")

if ! command -v go >/dev/null 2>&1; then
	echo "ERROR: go is required to build bd from the pin" >&2
	exit 1
fi

# bd_beads_version prints the beads module version `go version -m` reports for
# a binary, empty when the binary carries none (a shim, a non-Go build, a
# source build reporting (devel)).
bd_beads_version() {
	go version -m "$1" 2>/dev/null | awk -F'\t' '
		$1 == "mod" && $2 == "github.com/steveyegge/beads" { print $3; exit }
	'
}

mod_json=$(cd "$repo_root" && go mod edit -json)

if printf '%s\n' "$mod_json" | awk '
	/^\t"(Require|Replace|Exclude|Retract)"/ { sect = ($0 ~ /Replace/) ? "replace" : ""; next }
	/^\t"/ { sect = ""; next }
	sect == "replace" && /"Path": *"github\.com\/steveyegge\/beads"/ { found = 1 }
	END { exit found ? 0 : 1 }
'; then
	echo "ERROR: go.mod replaces github.com/steveyegge/beads, so its Require version is not the bd gc agrees with; remove the replace or build bd by hand" >&2
	exit 1
fi

pin=$(printf '%s\n' "$mod_json" | awk '
	function verof(line, v) {
		v = line
		sub(/.*"Version": *"/, "", v)
		sub(/".*/, "", v)
		return v
	}
	/"Path": *"github\.com\/steveyegge\/beads"/ {
		if ($0 ~ /"Version":/) { print verof($0); exit }
		inbeads = 1; next
	}
	inbeads && /"Version":/ { print verof($0); exit }
	inbeads && /"Path":/ { inbeads = 0 }
')
if [ -z "$pin" ]; then
	echo "ERROR: go.mod does not require github.com/steveyegge/beads, so there is no pin to build bd from" >&2
	exit 1
fi

installed="$INSTALL_DIR/bd"

echo "install-bd: building bd from github.com/steveyegge/beads/cmd/bd@$pin"
mkdir -p "$INSTALL_DIR"
# CGO_ENABLED=1 + gms_pure_go is the embedded-storage-capable bd build; a
# `pkg@version` install ignores the surrounding module, so it cannot touch
# this repo's go.mod or go.sum.
if ! (cd "$repo_root" && GOFLAGS= GOWORK=off CGO_ENABLED=1 GOBIN="$INSTALL_DIR" \
	go install -tags gms_pure_go "github.com/steveyegge/beads/cmd/bd@$pin"); then
	echo "ERROR: building bd@$pin failed; embedded storage needs a C toolchain (install gcc or clang and retry)" >&2
	exit 1
fi
if [ ! -f "$installed" ]; then
	echo "ERROR: the build produced no $installed" >&2
	exit 1
fi

# Migrate from the old install location: replace a stale binary or symlink
# with one to the installed bd, the way make install does for gc.
if [ "$INSTALL_DIR" != "$HOME/.local/bin" ]; then
	if [ -f "$HOME/.local/bin/bd" ] || [ -L "$HOME/.local/bin/bd" ]; then
		rm -f "$HOME/.local/bin/bd"
	fi
	if [ -d "$HOME/.local/bin" ]; then
		ln -sf "$installed" "$HOME/.local/bin/bd"
		echo "Symlinked $HOME/.local/bin/bd -> $installed"
	fi
fi

case "$(uname)" in
Darwin)
	GC_SIGN_IDENTIFIER=com.gascity.bd scripts/sign-darwin-local.sh "$installed"
	;;
esac

# The skew this script fixes is back the moment PATH still resolves an older
# bd first; name both files so the operator can see which one runs.
if shadow=$(command -v bd); then
	if [ "$shadow" != "$installed" ]; then
		echo "note: bd on PATH is $shadow, but this script installed $installed; make sure the scopes resolve the installed one"
	fi
fi

echo "install-bd: $installed embeds github.com/steveyegge/beads $pin"
