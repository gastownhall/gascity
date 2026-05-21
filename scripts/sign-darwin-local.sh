#!/usr/bin/env bash
set -euo pipefail

if [ "$#" -ne 1 ]; then
	echo "usage: scripts/sign-darwin-local.sh <binary>" >&2
	exit 2
fi

binary=$1
binary_name=$(basename "$binary")
identifier=${GC_SIGN_IDENTIFIER:-com.gascity.gc}

if [ "$(uname -s)" != "Darwin" ]; then
	exit 0
fi

if [ ! -f "$binary" ]; then
	echo "cannot sign missing binary: $binary" >&2
	exit 1
fi

if ! command -v codesign >/dev/null 2>&1; then
	echo "codesign not found; leaving Go linker signature unchanged for $binary_name"
	exit 0
fi

strip_provenance() {
	if command -v xattr >/dev/null 2>&1; then
		xattr -d com.apple.provenance "$binary" 2>/dev/null || true
	fi
}

sign_with_stable_identity() {
	local identity=$1
	local source=$2

	if codesign --force --sign "$identity" --identifier "$identifier" "$binary" 2>/dev/null; then
		strip_provenance
		echo "Signed $binary_name with stable macOS identity: $identity"
		return 0
	fi

	if [ "$source" = "explicit" ]; then
		echo "failed to sign $binary_name with GC_SIGN_IDENTITY=$identity" >&2
		return 1
	fi

	echo "Could not sign $binary_name with auto-detected identity; leaving Go linker signature unchanged." >&2
	return 0
}

find_stable_identity() {
	local candidates=$1
	local pattern
	local candidate

	for pattern in 'Apple Development:' 'Developer ID Application:' 'GasCity Dev'; do
		candidate=$(printf '%s\n' "$candidates" | awk -F '"' -v pattern="$pattern" 'index($0, pattern) {print $2; exit}')
		if [ -n "$candidate" ]; then
			printf '%s\n' "$candidate"
			return 0
		fi
	done
	return 0
}

if [ -n "${GC_SIGN_IDENTITY:-}" ]; then
	sign_with_stable_identity "$GC_SIGN_IDENTITY" "explicit"
	exit $?
fi

identity=""
if command -v security >/dev/null 2>&1; then
	candidates=$(security find-identity -p codesigning -v 2>/dev/null || true)
	identity=$(find_stable_identity "$candidates")
fi

if [ -n "$identity" ]; then
	sign_with_stable_identity "$identity" "auto"
	exit $?
fi

if [ "${GC_ADHOC_SIGN:-0}" = "1" ]; then
	if codesign --force --sign - "$binary" 2>/dev/null; then
		strip_provenance
		echo "Ad-hoc signed $binary_name by explicit opt-in"
	else
		echo "Could not ad-hoc sign $binary_name; leaving Go linker signature unchanged." >&2
	fi
	exit 0
fi

echo "No stable macOS signing identity found; leaving Go linker signature unchanged for $binary_name."
echo "Set GC_SIGN_IDENTITY='<certificate name>' for persistent local TCC grants."
