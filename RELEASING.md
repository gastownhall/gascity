# Releasing Gas City

## Distribution Channels

| Channel | Mechanism | Automatic? |
|---------|-----------|------------|
| **GitHub Release** | GoReleaser via `release.yml` on tag push | Yes |
| **GitHub draft prerelease** | GoReleaser via `rc-release.yml` on RC tag push or manual dispatch | Yes |
| **Homebrew tap** (`gastownhall/gascity`) | `release.yml` writes an asset-based formula after archives upload | Yes |
| **Homebrew core** (`Homebrew/homebrew-core`) | BrewTestBot autobump | Yes (~3h delay) |

Users install from homebrew-core with `brew install gascity`. The tap stays
available for emergency updates.

## How to Release

### Recommended: bump script

```bash
./scripts/bump-version.sh X.Y.Z --commit --tag --push
```

This rewrites the `[Unreleased]` section of `CHANGELOG.md` into `[X.Y.Z] - YYYY-MM-DD`, commits, tags `vX.Y.Z`, and pushes. GitHub Actions takes it from there.

### Release candidates

Use the **RC Release** workflow to produce downloadable release-candidate
archives for manual verification without updating Homebrew or publishing the
stable release.

Recommended path:

1. On the release branch, [re-pin the bundled packs](#re-pin-the-bundled-packs).
2. Run the RC gate on the intended release branch or commit.
3. Open **Actions → RC Release → Run workflow**.
4. Set `tag_name` to an RC tag such as `v1.2.0-rc1`.
5. Set `target_ref` to the exact RC-gate-passing commit SHA.

Nothing enforces step 2: the **RC Gate** workflow (`rc-gate.yml`) runs only
when someone dispatches it, and neither `rc-release.yml` nor `release.yml`
checks for a passing run. It is also the only workflow that checks the bundled
pack pin (`BundledPackImportVersion`) against the content the release embeds.

The RC Release workflow creates the annotated RC tag if needed, builds
GoReleaser archives for linux/darwin × amd64/arm64, and creates a GitHub
**draft prerelease** with generated GoReleaser notes and downloadable assets.
It does not update the Homebrew tap, create attestations, or mark the release
latest.

Tag creation on the dispatch path uses the **gastownhall-release-tagger**
GitHub App (an `Integration` bypass actor on the "Protect release tags"
ruleset), minted per-run from the `RELEASE_TAGGER_APP_ID` /
`RELEASE_TAGGER_APP_PRIVATE_KEY` repository secrets — the workflow's default
`GITHUB_TOKEN` is deliberately NOT a bypass actor, so arbitrary workflows
cannot create, move, or delete `v*` tags. The tagger token is minted **only
when the dispatch must create a new tag**; re-dispatching an already-existing
RC tag re-drafts it without the tagger secrets. If the app secrets are missing
on a create dispatch, the workflow fails with instructions instead of a bare
`GH013`; the manual-tag path below always works for members of the
release-maintainers bypass team.

Because the tagger App token is a bypass actor, pushing the newly created tag
re-fires `rc-release.yml` on that tag push. To avoid drafting the same RC
twice, a dispatch that creates the tag stops after pushing it, and the
resulting tag-push run is the one that builds the GoReleaser draft. A dispatch
for a tag that already exists — and every direct tag push — goes straight to
the draft build.

You can also push an existing RC tag manually:

```bash
git tag -a vX.Y.Z-rcN <commit> -m "Release vX.Y.Z-rcN"
git push origin vX.Y.Z-rcN
```

Pushing an RC tag triggers `rc-release.yml`. The stable `release.yml` jobs
skip tags containing a prerelease suffix, so RC tags do not run the Homebrew
or stable publishing path.

### Re-pin the bundled packs

`BundledPackImportVersion` (`internal/config/public_packs.go`) names the
gascity.git commit whose bundled packs (`internal/bootstrap/packs/core` and
`examples/bd`) match what the release embeds. The RC Gate's `embedded pins`
job fails if those packs changed after that commit. On `main` they move past
the pin between releases, so re-pin on the release branch, after its last
change to the bundled packs:

```bash
old=$(sed -n 's/.*BundledPackImportVersion = "sha:\([0-9a-f]\{40\}\)".*/\1/p' internal/config/public_packs.go)
new=$(git rev-parse HEAD)
test -n "$old" && git grep -l "$old" -- internal/config/public_packs.go examples docs | xargs perl -pi -e "s/$old/$new/g"
```

That moves the pin, and the examples that import the bundled packs, to the
branch tip. `SupersededBundledPackImportVersions` must then list every pin an
earlier gc wrote as canonical. The release serves its embedded packs for a
listed pin, and `gc doctor --fix` moves the city to the new pin. Any other pin
counts as deliberate: a city an earlier release created at such a pin stops
loading, and `gc import install` then fetches that commit's older packs in
place of the release's. Append `"sha:<old>",`, then every pin the previous
release shipped that the list lacks, each with a comment naming where it came
from. This prints those pins:

```bash
prev=vX.Y.Z   # the previous release's tag
git show "$prev:internal/config/public_packs.go" |
  sed -n -e 's/.*BundledPackImportVersion = "\(sha:[0-9a-f]*\)".*/\1/p' \
    -e '/^var SupersededBundledPackImportVersions/,/^}/s/^[[:space:]]*"\(sha:[0-9a-f]*\)",.*/\1/p' |
  while read -r pin; do grep -q "\"$pin\"" internal/config/public_packs.go || echo "$pin"; done
```

A branch cut from the previous release's tag lists them already.
`[release-only]` commits never reach `main`, so a branch cut from `main` lacks
the pins of a release shipped from a hotfix branch until they are added to
`main` (below).

Commit the re-pin as
`[release-only] chore(pins): re-pin the bundled packs to <new>` and push.
`scripts/check-embedded-pins --bd-advisory` runs the RC Gate's check locally.
Re-pin again if a later backport changes the bundled packs.

After the release, add its pins to `main` too, or `main` builds treat the
cities the release created as deliberately pinned to an old commit. On a
branch from `main`, run the command above with `prev` set to the new release's
tag, append what it prints to `SupersededBundledPackImportVersions`, and open
a pull request. The command skips `main`'s own pin, which stays canonical
there.

### Manual

If you want to do the steps by hand:

1. Edit `CHANGELOG.md`: move `[Unreleased]` contents under a new `## [X.Y.Z] - YYYY-MM-DD` section.
2. Commit:
   ```bash
   git add CHANGELOG.md
   git commit -m "chore: release vX.Y.Z"
   ```
3. Tag and push:
   ```bash
   git tag -a vX.Y.Z -m "Release vX.Y.Z"
   git push origin HEAD
   git push origin vX.Y.Z
   ```

Version numbers live **only** in the git tag — there is no `Version` constant in Go source to keep in sync. `cmd/gc/cmd_version.go` has `var version = "dev"` that the Makefile and `.goreleaser.yml` inject at build time via `-X main.version=$(VERSION)`.

## What Happens After Tag Push

`.github/workflows/release.yml` fires on stable `vMAJOR.MINOR.PATCH` tags and
runs, in order:

1. **Reject `replace` directives in `go.mod`** — they break `go install ...@latest` and bottle builds in homebrew-core.
2. **`make check-version-tag`** — asserts the tag is a clean `vMAJOR.MINOR.PATCH` with no pre-release suffix. RC/beta tags are handled by `rc-release.yml` instead.
3. **GoReleaser** — builds binaries for linux/darwin × amd64/arm64 and creates the GitHub Release with grouped changelog (`feat:` → Features, `fix:` → Bug Fixes, others → Others).
4. **Release attestations** — downloads the published checksum manifest, uploads an SPDX SBOM asset, and creates GitHub artifact attestations for the release archives.
5. **Homebrew tap update** — downloads the published checksums and writes an asset-based formula to `gastownhall/homebrew-gascity`.

Forks skip publish/announce steps automatically via the `--skip=publish --skip=announce` flag (the workflow checks `github.repository != 'gastownhall/gascity'`).

### Running checks locally before pushing the tag

```bash
make check-version-tag    # no-op unless HEAD is a release tag
grep '^replace' go.mod    # should print nothing
goreleaser check          # also enforced by CI
```

## Homebrew tap (`gastownhall/gascity`)

The release workflow automatically overwrites `Formula/gascity.rb` in the `gastownhall/homebrew-gascity` repo on every tag push. Publishing is GitHub App only: `HOMEBREW_TAP_APP_ID` and `HOMEBREW_TAP_APP_PRIVATE_KEY` must be configured in repository secrets for an app installed on `gastownhall/homebrew-gascity` with contents write.

The tap formula installs prebuilt release assets, so it needs no Go or source build. Name the tap to install from it:

```bash
brew install gastownhall/gascity/gascity
```

The public install path is homebrew-core (`brew install gascity`). The tap stays available for emergency updates; normal releases flow through homebrew-core.

## Homebrew core

The formula is in `Homebrew/homebrew-core`, and BrewTestBot bumps it automatically:

1. Tag push → GoReleaser creates the GitHub Release.
2. BrewTestBot polls releases every ~3h, opens a PR to homebrew-core bumping the formula's `url` and `sha256`.
3. Homebrew maintainers merge; bottles are built for Apple Silicon macOS and Linux (arm64 + x86_64).
4. `brew upgrade gascity` picks up the new version worldwide.

Manual `brew bump-formula-pr` is refused for autobump formulae. If the bot stalls, check `https://github.com/Homebrew/homebrew-core/pulls?q=gascity` for stuck PRs.

## Files Updated During a Release

| File | What changes | Updated by |
|------|-------------|------------|
| `CHANGELOG.md` | `[Unreleased]` → `[X.Y.Z] - DATE` | `scripts/bump-version.sh` |
| Git tag `vX.Y.Z` | Created and pushed | `scripts/bump-version.sh` |
| GitHub Release page | Created with binaries + grouped changelog | GoReleaser in `release.yml` |
| Release SBOM + attestations | SPDX SBOM uploaded and release archives attested | `attest-release` in `release.yml` |
| `internal/config/public_packs.go` and the example cities | `BundledPackImportVersion` re-pinned on the release branch; after the release, its pins added to `main`'s `SupersededBundledPackImportVersions` | [Re-pin the bundled packs](#re-pin-the-bundled-packs) |
| `gastownhall/homebrew-gascity/Formula/gascity.rb` | asset URLs + `sha256` updated | `update-homebrew-formula` in `release.yml` |
| `Homebrew/homebrew-core` `Formula/g/gascity.rb` | source `url` + `sha256` bumped | BrewTestBot's autobump PR |

## Troubleshooting

### Stable release workflow skipped an RC tag

This is expected. Tags with suffixes such as `-rc1`, `-beta`, or `-alpha.1`
are intentionally excluded from the stable `release.yml` publishing jobs. Use
the **RC Release** workflow for release candidates.

### GoReleaser fails with "replace directives"

`go.mod` contains a `replace` directive. These break `go install ...@latest` and homebrew-core bottle builds. Remove the directive and commit before tagging.

### Release tag pushed but workflow didn't run

Check `.github/workflows/release.yml` still matches `tags: v*`. Verify the tag was pushed to `origin` (`git ls-remote origin refs/tags/vX.Y.Z`).

### Tap formula not updated

Check the Homebrew tap GitHub App credentials in repo secrets: `HOMEBREW_TAP_APP_ID` and `HOMEBREW_TAP_APP_PRIVATE_KEY`. The app must be installed on `gastownhall/homebrew-gascity` with contents write. The workflow intentionally fails instead of falling back to a long-lived token.

### Homebrew shows old version after a release

homebrew-core picks up a release through the autobump bot, which runs every ~3h; users get it with `brew update && brew upgrade gascity` once the bot's PR merges and its bottles build. After ~6h without a PR, check `https://github.com/Homebrew/homebrew-core/pulls?q=gascity`.

The tap updates on the tag push itself; if it didn't, see [Tap formula not updated](#tap-formula-not-updated).
