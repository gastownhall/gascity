#!/usr/bin/env bash
# install-bd-pinned.sh DEST
# install-bd-pinned.sh --note DEST
#
# Installs, at DEST, a bd built from exactly the beads module version
# gascity's go.mod pins, the version of the beads library gc links, so bd
# and gc's own library know the same schema. A bd older than that library
# refuses to read a store once the store is migrated past that bd's schema
# (by gc's library where beads.allow_schema_behind_migrate allows it, or by a
# newer client); a newer release of the same major version reads it. So the
# install only ever upgrades: a bd at DEST that is newer than the pin is kept,
# and one whose beads version cannot be read is refused rather than replaced.
#
# The build is `go install pkg@version` run outside the repository with
# GOFLAGS cleared and GOWORK=off, so gascity's go.mod, go.sum and any go.work
# take no part in it and are never written. bd is built from source rather
# than fetched from a release archive because go.mod may pin a pseudo-version
# that no release contains. The new binary replaces DEST through a temp file
# and a rename.
#
# Nothing but DEST is written. When the bd PATH resolves is another one (a
# ~/.local/bin/bd from beads' own installer, say), it is named and left alone.
#
# --note builds and installs nothing: it prints a NOTE when PATH resolves no
# bd, or one older than the pin, and exits 0. `make install` runs it after it
# installs gc.
set -euo pipefail

mode=install
if [ "${1:-}" = --note ]; then
	mode=note
	shift
fi
if [ "$#" -ne 1 ] || [ -z "$1" ]; then
	echo "usage: scripts/install-bd-pinned.sh [--note] <destination bd path>" >&2
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

# read_pin sets pin to the beads version go.mod requires, or returns 1 with
# pin_error saying why there is none. It reads go.mod alone (`go mod edit
# -json`), never the module proxy. A go.mod that replaces beads has no pin:
# gc links the replacement. The JSON is matched with shell patterns, not an
# early-exiting reader in a pipeline, which pipefail would turn into a miss.
read_pin() {
	local json require replace key
	pin=""
	pin_error=""
	if ! json=$(go mod edit -json "$repo_root/go.mod" | tr -d ' \t\r\n'); then
		pin_error="\`go mod edit -json $repo_root/go.mod\` failed"
		return 1
	fi
	case "$json" in
	*'"Replace":['*)
		replace=${json#*\"Replace\":\[}
		case "$replace" in
		*"\"Old\":{\"Path\":\"$beads_module\","* | *"\"Old\":{\"Path\":\"$beads_module\"}"*)
			pin_error="go.mod replaces $beads_module; gc links the replacement, so no pinned bd matches it. Build bd from that replacement instead."
			return 1
			;;
		esac
		;;
	esac
	require=${json#*\"Require\":\[}
	require=${require%%]*}
	key="\"Path\":\"$beads_module\",\"Version\":\""
	case "$require" in
	*"$key"*)
		pin=${require#*"$key"}
		pin=${pin%%\"*}
		;;
	esac
	if [ -z "$pin" ]; then
		pin_error="$repo_root/go.mod does not require $beads_module"
		return 1
	fi
}

# embedded_beads_version prints the beads version a bd binary was built
# from: the version of its main module when that module is beads, as `go
# version -m` reports it. It prints nothing for a file that is missing, not a
# Go binary, or built from some other main module, and always succeeds. The
# output is read whole before it is searched: a reader that stopped at the
# mod line would kill `go version -m` mid-write with SIGPIPE.
embedded_beads_version() {
	local info
	[ -f "$1" ] || return 0
	info=$(go version -m "$1" 2>/dev/null) || return 0
	printf '%s\n' "$info" |
		awk -v m="$beads_module" '$1 == "mod" && $2 == m { v = $3 } END { if (v != "") print v }'
}

# semver_compare A B prints -1, 0 or 1 as version A precedes, equals or
# follows B by semver precedence, build metadata ignored, and prints nothing
# when either is not a v-prefixed semver version. A pseudo-version is a
# prerelease of the version after its base and orders by its commit time.
semver_compare() {
	awk -v a="$1" -v b="$2" '
	function valid(v) {
		return v ~ /^v[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.-]+)?(\+[0-9A-Za-z.-]+)?$/
	}
	# ident orders two identifiers: numerically when both are numbers, a
	# number before an alphanumeric one, otherwise in ASCII order.
	function ident(x, y,    xn, yn) {
		xn = x ~ /^[0-9]+$/
		yn = y ~ /^[0-9]+$/
		if (xn && yn) return (x + 0 > y + 0) - (x + 0 < y + 0)
		if (xn != yn) return xn ? -1 : 1
		return (x "" > y "") - (x "" < y "")
	}
	function order(a, b,    i, pa, pb, A, B, na, nb, r) {
		sub(/\+.*/, "", a)
		sub(/\+.*/, "", b)
		pa = ""
		pb = ""
		if ((i = index(a, "-")) > 0) { pa = substr(a, i + 1); a = substr(a, 1, i - 1) }
		if ((i = index(b, "-")) > 0) { pb = substr(b, i + 1); b = substr(b, 1, i - 1) }
		split(substr(a, 2), A, ".")
		split(substr(b, 2), B, ".")
		for (i = 1; i <= 3; i++) if ((r = ident(A[i], B[i])) != 0) return r
		if (pa == pb) return 0
		if (pa == "") return 1
		if (pb == "") return -1
		na = split(pa, A, ".")
		nb = split(pb, B, ".")
		for (i = 1; i <= na && i <= nb; i++) if ((r = ident(A[i], B[i])) != 0) return r
		return (na > nb) - (na < nb)
	}
	BEGIN { if (valid(a) && valid(b)) print order(a, b) }'
}

# pin_relation prints how the beads version a bd embeds relates to the pin:
# same, older or newer, or unknown when there is none, it is not semver, or
# it is a modified (+dirty) source build, which names no published version.
pin_relation() {
	case "$1" in
	'' | *+dirty)
		echo unknown
		return
		;;
	esac
	case "$(semver_compare "$1" "$pin")" in
	-1) echo older ;;
	0) echo same ;;
	1) echo newer ;;
	*) echo unknown ;;
	esac
}

resolve_path() {
	readlink -f "$1" 2>/dev/null || printf '%s\n' "$1"
}

# note_path_bd names the bd PATH resolves when it is not the one at $dest:
# commands that run `bd` keep using that one. It is never modified here.
note_path_bd() {
	local found version
	found=$(command -v bd 2>/dev/null || true)
	if [ -z "$found" ]; then
		echo "NOTE: no bd on PATH; put $install_dir on PATH (or point BD_BIN at $dest) so commands run $dest."
		return
	fi
	if [ "$(resolve_path "$found")" != "$(resolve_path "$dest")" ]; then
		version=$(embedded_beads_version "$found")
		echo "NOTE: bd on PATH is $found (beads ${version:-version unknown}), not $dest; put $install_dir first on PATH (or point BD_BIN at $dest) so commands run $dest. $found is left as it is."
	fi
}

# note_bd_on_path is --note: it says when PATH resolves no bd, or one older
# than the pin, which `make install-bd` installs over.
note_bd_on_path() {
	local found version
	found=$(command -v bd 2>/dev/null || true)
	if [ -z "$found" ]; then
		echo "NOTE: no bd on PATH, and gc needs one; \`make install-bd\` (with the same INSTALL_DIR) builds one from $beads_module $pin, the version gc links, at $dest."
		return
	fi
	version=$(embedded_beads_version "$found")
	if [ "$(pin_relation "$version")" = older ]; then
		echo "NOTE: bd on PATH ($found) is built from $beads_module $version, older than the $pin gc links, and refuses to read a store once the store is migrated past its schema; \`make install-bd\` (with the same INSTALL_DIR) installs one built from $pin at $dest."
	fi
}

# install_pinned_bd builds bd from the pin in a scratch directory, checks the
# version the build embeds, and only then renames it over $dest.
install_pinned_bd() {
	local built
	mkdir -p "$install_dir"
	stage=$(mktemp -d "${TMPDIR:-/tmp}/gc-install-bd.XXXXXX")
	echo "Building bd from $beads_module@$pin"
	if ! (
		cd "$stage"
		GOFLAGS='' GOWORK=off CGO_ENABLED=1 GOBIN="$stage" \
			go install -tags gms_pure_go "$beads_module/cmd/bd@$pin"
	); then
		echo "ERROR: building bd from $beads_module@$pin failed; $dest is unchanged." >&2
		echo "       bd's embedded storage needs CGO, so a C toolchain (cc) is required." >&2
		exit 1
	fi
	if [ "$(uname -s)" = Darwin ]; then
		GC_SIGN_IDENTIFIER=com.gascity.bd "$repo_root/scripts/sign-darwin-local.sh" "$stage/bd"
	fi
	built=$(embedded_beads_version "$stage/bd") || built=""
	if [ "$built" != "$pin" ]; then
		echo "ERROR: the bd built from $beads_module@$pin embeds beads '${built:-unknown}', not $pin; $dest is unchanged." >&2
		exit 1
	fi

	tmp="$install_dir/.bd.tmp.$$"
	cp -f "$stage/bd" "$tmp"
	chmod 0755 "$tmp"
	mv -f "$tmp" "$dest"
	tmp=""
	echo "Installed bd ($beads_module $pin) to $dest"
}

if ! read_pin; then
	if [ "$mode" = note ]; then
		echo "NOTE: bd not checked: $pin_error"
		exit 0
	fi
	echo "ERROR: $pin_error" >&2
	exit 1
fi

if [ "$mode" = note ]; then
	note_bd_on_path
	exit 0
fi

current=$(embedded_beads_version "$dest")
case "$(pin_relation "$current")" in
same)
	echo "bd at $dest is already built from $beads_module $pin; nothing to build"
	;;
newer)
	echo "bd at $dest is built from $beads_module $current, newer than the $pin gc links; keeping it, because an older bd cannot read a store the newer one migrated"
	;;
older)
	echo "bd at $dest is built from $beads_module $current, older than the $pin gc links; replacing it"
	install_pinned_bd
	;;
unknown)
	if [ -e "$dest" ]; then
		echo "ERROR: $dest names no beads version gc can compare (${current:-no Go build info for $beads_module}); it is left as it is. Move it aside to install a bd built from $beads_module $pin." >&2
		exit 1
	fi
	install_pinned_bd
	;;
esac
note_path_bd
