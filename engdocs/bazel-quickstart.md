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
file digest on the shared farm (cherry). Any machine — your laptop,
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

### 2. Point at the remote cache (the free win)

Create `.bazelrc.local` in the repo root (gitignored):

```bash
# read + write the shared CAS — safe: content-addressed, never corrupts
build --remote_cache=grpcs://rbe.ops.gascity.com:443
```

For **read-only** (cheaper, no upload):

```bash
build --remote_cache=grpcs://rbe.ops.gascity.com:443
build --remote_upload_local_results=false
```

### 3. (Optional) remote execution — compile on the farm

If you want compiles to run on elastic workers instead of your machine:

```bash
# same as above, plus:
build --remote_executor=grpcs://rbe.ops.gascity.com:443
build --tls_client_certificate=/path/to/client-cert.pem
build --tls_client_key=/path/to/client-key.pem
build --jobs=32
```

Client certs are issued by the farm team (the CA lives in OpenBao,
`kv/operator-env/rbe-edge-mtls`). For most dev work the cache alone is
enough — you compile locally but hit the shared results.

### 4. Verify

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

