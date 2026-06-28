#!/usr/bin/env bash
# Vendors the bd contract corpus from a published beads release into
# internal/beads/testdata/corpus/, verifying the downloaded archive against the
# release's signed checksums.txt — NOT a self-recomputed manifest. Run during a
# deliberate bd pin bump:
#
#   make sync-bd-corpus BD_CORPUS_TAG=v1.0.6
#
# Then review the diff and commit. The release must have been built with the
# corpus-publishing step (contract Phase 3); older releases predate it and are
# rejected. See engdocs/design/beads-gascity-contract-test-system.md.
set -euo pipefail

repo="${BD_REPO:-gastownhall/beads}"
tag="${BD_CORPUS_TAG:-}"
dest="internal/beads/testdata/corpus"

if [[ -z "$tag" ]]; then
  echo "usage: BD_CORPUS_TAG=<vX.Y.Z> $0   (the beads release tag to vendor the corpus from)" >&2
  exit 2
fi

sha256() {
  if command -v sha256sum >/dev/null 2>&1; then
    sha256sum "$1" | awk '{print $1}'
  else
    shasum -a 256 "$1" | awk '{print $1}'
  fi
}

version="${tag#v}"
archive="beads_${version}_contract_corpus.tar.gz"
work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT

echo "Downloading ${archive} + checksums.txt from ${repo} ${tag} ..."
gh release download "$tag" --repo "$repo" \
  --pattern "$archive" --pattern checksums.txt --dir "$work"

echo "Verifying ${archive} against the release checksums.txt ..."
want="$(awk -v a="$archive" '$2 == a {print $1}' "$work/checksums.txt")"
if [[ -z "$want" ]]; then
  echo "ERROR: ${archive} is not in the release checksums.txt; ${tag} predates the contract corpus (Phase 3)." >&2
  exit 1
fi
got="$(sha256 "$work/$archive")"
if [[ "$want" != "$got" ]]; then
  echo "ERROR: checksum mismatch for ${archive}: release=${want} downloaded=${got}" >&2
  exit 1
fi

echo "Extracting into ${dest} ..."
tar -xzf "$work/$archive" -C "$work"
rm -rf "${dest:?}"
mkdir -p "$dest"
cp -rf "$work/corpus/." "$dest/"
echo "Vendored corpus from ${repo} ${tag} into ${dest}. Review the diff and commit."
