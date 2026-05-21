#!/usr/bin/env bash
set -u

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

if [ -n "${GC_SIGN_IDENTITY:-}" ]; then
	sign_with_stable_identity "$GC_SIGN_IDENTITY" "explicit"
	exit $?
fi

identity=""
if command -v security >/dev/null 2>&1; then
	identity=$(
		security find-identity -p codesigning -v 2>/dev/null |
			awk -F '"' '/Apple Development:|Developer ID Application:|GasCity Dev/{print $2; exit}'
	)
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
