# Bazel quickstart: local cache + remote build farm

The repo builds two ways: `go build`/`go test` (unchanged, the required
CI) and Bazel (side-by-side, remote-cached, the fast iteration loop).
This guide sets up the second for your dev machine.

## Why

| | `go test ./...` | `bazel test //...` |
|---|---|---|
| first run | ~15 min | ~15 min (same work) |
| warm run | ~15 min (per-package cache only) | **~0.6s** (action-level, remote) |
| cross-worktree | no | **yes** (shared CAS) |
| CI-parity | yes | yes (same test binaries) |

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

### 2. Use the shared cache (the free win)

No setup: the committed `.bazelrc` has a `fork-cache` config for rbe-west's
anonymous, read-only cache. Pass it to read every result CI already
computed; misses build and run locally, and nothing you run is uploaded:

```bash
bazel test //... --config=fork-cache
```

This only hits if your actions hash like CI's, so do not add key-affecting
flags (`--test_env`, `--action_env`, `--define`, platforms, ...) to
`.bazelrc.local`; it is for endpoints and credentials only
(`scripts/bazel_key_parity_test.go`). Locally run tests get the pinned test
`PATH`, so Go must be at `/usr/local/go`
(`sudo ln -s "$(go env GOROOT)" /usr/local/go` if it is elsewhere).

Maintainers can opt in to remote execution with an rbe-west client
certificate; see TESTING.md "Bazel cache tiers" for how to obtain one and
the `.bazelrc.local` lines, then use `--config=remote-exec`. The pre-push
hook picks the right mode automatically: remote execution when any rc file
(`.bazelrc.local`, `~/.bazelrc`, `/etc/bazel.bazelrc`) names a remote
executor, as agent hosts' `~/.bazelrc` does, and the read-only cache
otherwise.

### 3. Verify

```bash
bazel build //cmd/gc          # first run: compiles everything
bazel build //cmd/gc          # second run: "INFO: N processes: all action cache hit" — instant
bazel test //internal/config  # tests too: "Executed 1 out of 1: 1 test passes" in <1s
```

## Daily use

```bash
bazel test //...                        # full suite (the number agents care about)
bazel test //internal/beads/...         # subtree
bazel test //internal/config:config_test --test_output=errors  # one test, verbose
bazel run //cmd/gc -- --help            # run a binary
```

## When you change BUILD-relevant things

After adding a package, a file, or changing imports:

```bash
make bazel-sync     # regenerates BUILD files + the repo source tree
git add -A && git commit -m "build: sync"   # the CI gate checks this
```

## Troubleshooting

**"no such package"** after a rebase → run `make bazel-sync`.

**Slow first build** → normal; the CAS is warming from your changes.
Subsequent builds hit the cache.

**Tests fail under bazel but pass under go test** → the test probably
depends on something outside its declared inputs. Check
`engdocs/bazel-ci-budget.md`'s hermetic-input notes, or the
`internal/bazeltest` package docs.

**Want to measure the cache win?**

```bash
bazel test //... --profile=/tmp/p.json
python3 tools/bazel/critpath.py /tmp/p.json
```

