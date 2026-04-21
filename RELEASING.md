# Releasing Gas City

## Distribution Channels

| Channel | Mechanism | Automatic? |
|---------|-----------|------------|
| **GitHub Release** | GoReleaser via `release.yml` on tag push | Yes |
| **Homebrew tap** (`gastownhall/gascity`) | GoReleaser `brews:` block writes to the tap on tag push | Yes |
| **Homebrew core** (`Homebrew/homebrew-core`) | BrewTestBot autobump, once listed | Yes (~3h delay) |

The homebrew-core submission is [in progress](https://github.com/Homebrew/homebrew-core). Until it lands and is added to the autobump list, users install via `brew install gastownhall/gascity/gascity`.

## How to Release

### Recommended: bump script

```bash
./scripts/bump-version.sh X.Y.Z --commit --tag --push
```

This rewrites the `[Unreleased]` section of `CHANGELOG.md` into `[X.Y.Z] - YYYY-MM-DD`, commits, tags `vX.Y.Z`, and pushes. GitHub Actions takes it from there.

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

`.github/workflows/release.yml` fires on any `v*` tag and runs, in order:

1. **Reject `replace` directives in `go.mod`** — they break `go install ...@latest` and bottle builds in homebrew-core.
2. **`make check-version-tag`** — asserts the tag is a clean `vMAJOR.MINOR.PATCH` with no pre-release suffix. RC/beta tags will fail the release. Pre-release tags should be cut on a dedicated branch or not trigger this workflow.
3. **GoReleaser** — builds binaries for linux/darwin × amd64/arm64, creates the GitHub Release with grouped changelog (`feat:` → Features, `fix:` → Bug Fixes, others → Others), and writes the Homebrew tap formula via the `brews:` block in `.goreleaser.yml`.

Forks skip publish/announce steps automatically via the `--skip=publish --skip=announce` flag (the workflow checks `github.repository != 'gastownhall/gascity'`).

### Running checks locally before pushing the tag

```bash
make check-version-tag    # no-op unless HEAD is a release tag
grep '^replace' go.mod    # should print nothing
```

## Homebrew tap (`gastownhall/gascity`)

The GoReleaser `brews:` block automatically overwrites `Formula/gascity.rb` in the `gastownhall/homebrew-gascity` repo on every tag push, using `HOMEBREW_TAP_TOKEN`. No manual action required.

**This section will change when homebrew-core lands.** The `brews:` block will be removed, the tap will be deprecated, and releases will flow only through the source-built formula in homebrew-core.

## Homebrew core (planned)

Once the formula is merged to `Homebrew/homebrew-core` and added to the autobump list, the flow becomes:

1. Tag push → GoReleaser creates GitHub Release (as today).
2. BrewTestBot polls releases every ~3h, opens a PR to homebrew-core bumping the formula's `url` and `sha256`.
3. Homebrew maintainers merge; bottles are built for macOS (arm64 + x86_64) and Linux.
4. `brew upgrade gascity` picks up the new version worldwide.

Manual `brew bump-formula-pr` is refused for autobump formulae. If the bot stalls, check `https://github.com/Homebrew/homebrew-core/pulls?q=gascity` for stuck PRs.

## Files Updated During a Release

| File | What changes | Updated by |
|------|-------------|------------|
| `CHANGELOG.md` | `[Unreleased]` → `[X.Y.Z] - DATE` | `scripts/bump-version.sh` |
| Git tag `vX.Y.Z` | Created and pushed | `scripts/bump-version.sh` |
| GitHub Release page | Created with binaries + grouped changelog | GoReleaser in `release.yml` |
| `gastownhall/homebrew-gascity/Formula/gascity.rb` | `url` + `sha256` updated | GoReleaser in `release.yml` |

## Troubleshooting

### `make check-version-tag` fails with "pre-release suffix"

The tag has a suffix (`-rc1`, `-beta`, `-alpha.1`). The release workflow only publishes stable releases. Either retag without the suffix, or don't trigger `release.yml` for this tag.

### GoReleaser fails with "replace directives"

`go.mod` contains a `replace` directive. These break `go install ...@latest` and homebrew-core bottle builds. Remove the directive and commit before tagging.

### Release tag pushed but workflow didn't run

Check `.github/workflows/release.yml` still matches `tags: v*`. Verify the tag was pushed to `origin` (`git ls-remote origin refs/tags/vX.Y.Z`).

### Tap formula not updated

Check `HOMEBREW_TAP_TOKEN` in repo secrets. It needs `contents: write` on `gastownhall/homebrew-gascity`. The workflow logs will show the exact error.

### Homebrew shows old version after a release

While on the tap: a tag push updates the tap directly; users pick it up on the next `brew update && brew upgrade gascity`. If the formula wasn't updated, see above.

After homebrew-core lands: the autobump bot runs every ~3h. After ~6h without a PR, check `https://github.com/Homebrew/homebrew-core/pulls?q=gascity`.
