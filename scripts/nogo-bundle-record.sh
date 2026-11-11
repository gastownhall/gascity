#!/usr/bin/env bash
# nogo-bundle-record.sh [--check] [<golangci-lint version>]
#
# Generates tools/nogo/golangci-lint-bundle.txt: the analyzer versions the
# golangci-lint release compiles in. tools/nogo/go.mod and MODULE.bazel must
# pin the same versions (TestAnalyzerPinsMatchGolangciLintBundle in
# tools/nogo/bundle_test.go holds them equal, offline).
#
# <version> is the Makefile's GOLANGCI_LINT_VERSION (for example 2.14.0, no
# leading v) and defaults to it. The release is fetched with `go mod download
# -json`, which verifies it against sum.golang.org, so this needs the network.
#
#   scripts/nogo-bundle-record.sh            write the record, then print the
#                                            go.mod and MODULE.bazel edits it
#                                            implies
#   scripts/nogo-bundle-record.sh --check    re-derive the record and diff it
#                                            against the checked-in file
#
# Run it by hand (a bump, or a reviewer checking the record); never from
# `bazel test`, which has no network. Regeneration is this script and not a
# test's -update flag because bazel test runs in a sandbox and would pass
# while changing nothing.
set -euo pipefail

root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
record="$root/tools/nogo/golangci-lint-bundle.txt"

# The analyzer modules whose versions the record carries. Every one of them is
# a direct requirement of tools/nogo/go.mod except gofumpt, whose version
# lives in MODULE.bazel.
modules=(
	codeberg.org/polyfloyd/go-errorlint
	github.com/go-critic/go-critic
	github.com/golangci/misspell
	github.com/golangci/unconvert
	github.com/gordonklaus/ineffassign
	github.com/kisielk/errcheck
	github.com/mgechev/revive
	honnef.co/go/tools
	mvdan.cc/unparam
	mvdan.cc/gofumpt
)

check=0
if [[ "${1:-}" == "--check" ]]; then
	check=1
	shift
fi
if [[ $# -gt 1 ]]; then
	echo "usage: nogo-bundle-record.sh [--check] [<golangci-lint version>]" >&2
	exit 2
fi

version="${1:-}"
if [[ -z "$version" ]]; then
	version="$(sed -n 's/^GOLANGCI_LINT_VERSION[[:space:]]*:=[[:space:]]*\([^[:space:]]*\)[[:space:]]*$/\1/p' "$root/Makefile")"
	if [[ -z "$version" ]]; then
		echo "nogo-bundle-record: no GOLANGCI_LINT_VERSION := <version> line in Makefile" >&2
		exit 1
	fi
fi
version="${version#v}"

for tool in go jq; do
	if ! command -v "$tool" >/dev/null 2>&1; then
		echo "nogo-bundle-record: $tool not found" >&2
		exit 1
	fi
done

scratch="$(mktemp -d)"
trap 'rm -rf "$scratch"' EXIT

# download <module>@<version> prints the `go mod download -json` object. It
# runs outside any module so the repository's go.mod, go.work and vendor
# directory cannot influence the lookup.
download() {
	local out
	if ! out="$(cd "$scratch" && GOFLAGS='' GOWORK=off go mod download -json "$1")"; then
		echo "nogo-bundle-record: go mod download $1 failed" >&2
		return 1
	fi
	if [[ "$(jq -r '.Error // empty' <<<"$out")" != "" ]]; then
		echo "nogo-bundle-record: go mod download $1: $(jq -r .Error <<<"$out")" >&2
		return 1
	fi
	printf '%s\n' "$out"
}

lint_json="$(download "github.com/golangci/golangci-lint/v2@v$version")"
lint_mod="$(jq -r .GoMod <<<"$lint_json")"
lint_sum="$(jq -r .GoModSum <<<"$lint_json")"
if [[ ! -f "$lint_mod" || -z "$lint_sum" || "$lint_sum" == "null" ]]; then
	echo "nogo-bundle-record: no go.mod or GoModSum for golangci-lint v$version" >&2
	exit 1
fi

generated="$scratch/record.txt"
{
	cat <<'EOF'
# golangci-lint bundle record: the analyzer versions the golangci-lint release
# below compiles in. tools/nogo/go.mod must require each row's module at its
# version, and MODULE.bazel must pin gofumpt at its row; the guard test
# tools/nogo/bundle_test.go holds both. GENERATED, never hand-edited: the file
# covers the version the Makefile pins in GOLANGCI_LINT_VERSION only, and a
# bump replaces it.
# Regenerate with scripts/nogo-bundle-record.sh [<version>]; check it online
# with scripts/nogo-bundle-record.sh --check [<version>].
EOF
	echo "golangci-lint $version $lint_sum"
	for module in "${modules[@]}"; do
		# One requirement line per module, in a require block of the release's
		# go.mod, with or without a trailing `// indirect`.
		rows="$(awk -v m="$module" '
			$1 == "require" && $2 == m { print $3; next }
			$1 == m && NF >= 2 { print $2 }
		' "$lint_mod")"
		if [[ -z "$rows" || "$(wc -l <<<"$rows")" -ne 1 ]]; then
			echo "nogo-bundle-record: golangci-lint v$version go.mod has no single requirement for $module (got: ${rows:-none})" >&2
			exit 1
		fi
		echo "$module $rows"
	done
} >"$generated"

if [[ "$check" -eq 1 ]]; then
	if diff -u "$record" "$generated"; then
		echo "nogo-bundle-record: $record matches golangci-lint v$version"
		exit 0
	fi
	echo "nogo-bundle-record: $record differs from golangci-lint v$version (diff above); regenerate it: scripts/nogo-bundle-record.sh $version" >&2
	exit 1
fi

# Atomic write: temp file in the target directory, then rename.
tmp="$(mktemp "$record.XXXXXX")"
cp -f "$generated" "$tmp"
chmod 0644 "$tmp"
mv -f "$tmp" "$record"
echo "wrote $record for golangci-lint v$version"

echo
echo "tools/nogo/go.mod needs these requirements (then run go mod tidy in tools/nogo):"
for module in "${modules[@]}"; do
	[[ "$module" == "mvdan.cc/gofumpt" ]] && continue
	echo "  go mod edit -require=$module@$(awk -v m="$module" '$1 == m { print $2 }' "$record")"
done

gofumpt_version="$(awk '$1 == "mvdan.cc/gofumpt" { print $2 }' "$record")"
gofumpt_sum="$(download "mvdan.cc/gofumpt@$gofumpt_version" | jq -r .Sum)"
echo
echo "MODULE.bazel go_deps.module(path = \"mvdan.cc/gofumpt\", ...) needs:"
echo "    sum = \"$gofumpt_sum\","
echo "    version = \"$gofumpt_version\","
