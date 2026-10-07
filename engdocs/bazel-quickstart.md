# Bazel quickstart: building and testing Gas City

Bazel is how Gas City is built and tested. CI gates on `bazel test`
(`.github/workflows/bazel.yml`), and `make test`, `make check`,
`make test-acceptance` and `make test-integration` run the same commands.
`go build`/`go test` still work for a quick inner loop on one package, but
they are not what CI enforces: nogo (lint and vet), formatting,
generated-artifact drift and the policy guards exist only as Bazel targets.
This guide sets Bazel up on your machine; TESTING.md "Building and testing"
lists the command for every test tier.

## Why

| | `go test ./...` | `bazel test //...` |
|---|---|---|
| what CI gates on | no | **yes** |
| lint, vet, format, generated artifacts | no | **yes** (nogo and test targets) |
| first run | ~15 min | ~15 min (same work), or seconds on remote execution |
| warm run | ~15 min (per-package cache only) | **seconds** (action-level, remote) |
| cross-worktree | no | **yes** (shared CAS) |

Bazel's remote cache stores every compiled object, test result, and
file digest in a shared remote cache. Any machine — your laptop,
a worktree, a CI runner — hits the same cache. Work you've already
done never repeats.

## One-time setup

### 1. Install Bazel

```bash
# macOS
brew install bazelisk

# Linux
curl -sSfL https://github.com/bazelbuild/bazelisk/releases/latest/download/bazelisk-linux-amd64 \
  | sudo tee /usr/local/bin/bazel > /dev/null && sudo chmod +x /usr/local/bin/bazel
```

Bazelisk reads `.bazelversion` (committed) and pins the exact version.

No C compiler is needed: cgo and the Go stdlib build with the LLVM toolchain
and Ubuntu 24.04 sysroot that `MODULE.bazel` pins by sha256, so every Linux
x86_64 machine computes the same action keys as CI (host toolchain detection
is off). The first fetch downloads the 2GB LLVM release archive once and
keeps ~700MB of it. Running the toolchain needs glibc 2.34+, `xz` (to unpack
it), and the runtime libraries the official LLVM binaries load: libstdc++6,
zlib1g and libxml2. Bazel-built binaries load glibc, libstdc++ and ICU 74
(`libicu74`) at run time, as on the RBE workers.

Other hosts (macOS arm64, Linux arm64) build with toolchains_llvm's stock
release of the same LLVM version, also pinned by sha256, but without a
sysroot: cgo uses the host's C headers and libraries (ICU included), and
their action keys do not match CI's.

### 2. Choose where actions run

Bazel decides where to run from the rc files; the committed `.bazelrc` has
no executor, so pick one of these per machine.

**Contributors: the read-only shared cache.** The committed `.bazelrc` has a
`fork-cache` config for rbe-west's anonymous, read-only cache. It reads every
result CI already computed; misses build and run locally, and nothing you
run is uploaded. No credential is needed:

```bash
bazel test //... --config=fork-cache
echo 'build --config=fork-cache' >> .bazelrc.local   # or make it the default
```

This only hits if your actions hash like CI's, so do not add key-affecting
flags (`--test_env`, `--action_env`, `--define`, platforms, ...) to
`.bazelrc.local`; it is for endpoints and credentials only
(`scripts/bazel_key_parity_test.go`). Locally run tests get the pinned test
`PATH`, so Go must be at `/usr/local/go`
(`sudo ln -s "$(go env GOROOT)" /usr/local/go` if it is elsewhere).

**Maintainers: remote execution.** With an rbe-west mTLS client certificate
(TESTING.md "Bazel cache tiers" has how to obtain one and the four
`build:remote-exec` lines for `.bazelrc.local`), `--config=remote-exec`
executes every action on rbe-west's `oss` pool; your machine only analyzes.
Results are written by rbe-west's workers alone, so CI and contributors hit
what you ran. Add `build --config=remote-exec` to `.bazelrc.local` to make
it the default.

**Agent hosts.** The operator's `~/.bazelrc` names the executor and
certificate, so plain `bazel test` already executes remotely.

The pre-push hook picks the mode automatically: remote execution when any rc
file (`.bazelrc.local`, `~/.bazelrc`, `/etc/bazel.bazelrc`) names a remote
executor and the checkout's `worker-env` pin is main's, the read-only cache
otherwise.

### 3. Verify

```bash
bazel build //cmd/gc          # first run: compiles everything
bazel build //cmd/gc          # second run: "INFO: N processes: all action cache hit" — instant
bazel test //internal/config  # tests too: "Executed 1 out of 1: 1 test passes" in <1s
```

## Daily use

```bash
make test                               # bazel test //...: CI's unit lane
make check                              # make test plus the shell guards
bazel test //internal/beads/...         # subtree
bazel test //internal/config:config_test --test_filter=TestAgentFieldSync  # one test
bazel test --config=acceptance //test/acceptance:acceptance_test           # make test-acceptance
bazel test --config=integration //test:integration_packages                # gating integration lane
bazel run //cmd/gc -- --help            # run a binary
```

`make` passes `BAZEL_FLAGS` through, so `make test BAZEL_FLAGS=--config=fork-cache`
works without editing `.bazelrc.local`.

**When to use which:**

| situation | use |
|---|---|
| iterating on one package's tests | `bazel test //pkg:pkg_test` (remote-cached) |
| verifying a change before pushing | `make check` (`bazel test //...`) |
| acceptance or integration behavior changed | the matching `--config=acceptance` / `--config=integration` target |
| quick compile-and-run of one file, no Bazel server | `go build ./pkg/` or `go test ./pkg -run TestX` (inner loop only; not what CI checks) |
| offline, or a host Bazel does not serve (macOS) | the `-go` make twins: `make test-go`, `make check-go` |
| adding a new dependency | `go get` then `make bazel-sync` |

**Test sharding:** the heavy suites (cmd/gc, scripts, api, examples) are
sharded for parallel remote execution. Sharded helpers re-exec the test
binary; if you add a helper-spawning test, strip `TEST_SHARD_INDEX` /
`TEST_TOTAL_SHARDS` from the helper's env (see `sanitizedBaseEnv` in
`cmd/gc/fast_loop_helpers_test.go`).

**Do NOT commit machine-specific endpoints or credentials.** They belong
in `.bazelrc.local` (gitignored) for dev machines, or in CI secrets. The
repo's `.bazelrc` has no executor hardcoded.

## When you change BUILD-relevant things

After adding a package, a file, or changing imports:

```bash
make bazel-sync     # regenerates BUILD files + the repo file trees
git add -A && git commit -m "build: sync"   # the CI gate checks this
```

The CI gate `BUILD files in sync` fails if you forget.

## Declaring the repo files a test reads

A test that reads repository files (a guard that scans source, a pack
fixture, a published schema) must list them in its `go_test` `data`,
together with `//:go.mod`, which is how `internal/bazeltest` finds the
root. Declare the narrowest set that covers the reads: every declared
file is part of the test's cache key, so a wider set re-runs the test on
unrelated edits.

| the test reads | declare |
|---|---|
| a few specific files | the file labels (`exports_files` them if needed) |
| one package's files | `//pkg:bazel_repo_srcs`, `:bazel_go_srcs` or `:bazel_go_test_srcs` |
| an example or pack tree | `//examples/<name>:pack_files`, `//examples:all_examples`, `//internal/bootstrap/packs/core:pack_files` |
| every non-test `.go` file | `//:repo_go_srcs` |
| every `_test.go` file | `//:repo_go_test_srcs` |
| docs, workflows and source together | `//:repo_source_tree` |

`make bazel-sync` (`tools/bazel/repo_tree.py`) generates the per-package
filegroups and the root aggregates. Keep whole-repo guards in small
dedicated targets (for example `//internal/beads/bdboundary`), not in a
package's main test target: a guard that reads every `.go` file re-runs on
every Go edit, and it takes its whole target with it.

## Troubleshooting

**"no such package"** after a rebase → run `make bazel-sync`.

**Slow first build** → normal; the CAS is warming from your changes.
Subsequent builds hit the cache.

**Remote actions sit queued** → the checkout's `worker-env` pin is not
main's (TESTING.md "Stale `worker-env` pin"). Rebase onto main, or run with
`--config=fork-cache` meanwhile.

**Tests fail under bazel but pass under go test** → the Bazel result is the
one CI sees. The test probably depends on something outside its declared
inputs. Check
`engdocs/bazel-ci-budget.md`'s hermetic-input notes, or the
`internal/bazeltest` package docs.

**Want to measure the cache win?**

```bash
bazel test //... --profile=/tmp/p.json
python3 tools/bazel/critpath.py /tmp/p.json
```

