# Bazel Test Hermeticity Audit

**Target:** `origin/main` @ `6f5f6fec4a` (2026-10-04). The audit was static
(source reading plus targeted probes on rbe-west). It covers every `go_test`
target: 185 packages with exactly one `go_test` each.

## Why this matters

Later phases plan to skip tests on a Bazel test-result cache hit, from
pre-push through PR to main. A cached PASS is only true while every input
the test reads is part of the action key. The key covers sources, `data`,
forwarded `--test_env` *strings*, and the platform. It does not cover:

- the internet;
- the calendar date;
- the contents of host binaries found through `PATH`;
- `$HOME`;
- files outside the sandbox;
- whatever a forwarded env var points at.

A test that reads any of these can keep serving a cached PASS that is no
longer true.

Each finding is classified as:

1. **Make hermetic.** A cheap fix that declares or removes the input.
2. **Never cache.** Tag the target `external`, which always re-executes, or
   `no-remote-cache`. Prefer `external`: it is unambiguous for both the remote
   cache and the shared host disk cache (`/etc/bazel.bazelrc` sets
   `--disk_cache`).
3. **Fine.** Either cache-safe, or covered by the separate effort to pin the
   worker image into the action key.

## Execution environment

These facts were measured with a probe `go_test` on rbe-west, instance
`oss`, on 2026-10-04.

| Fact | Evidence | Consequence |
|---|---|---|
| The `oss` worker tier has public internet egress. Only private ranges are blocked, by nftables, with `NETNS=0`. | A test dialing `github.com:443` PASSED. Its rerun printed `(cached) PASSED`. | Network-dependent tests are cached today with no signal. |
| The fork tier (`oss-fork`) runs every action with loopback only (`NETNS=1`). It caches nothing. | `tools/rbe/blacksmith-worker.sh` `WORKER_TIER=fork`. `bazel-test.yml` skipped acceptance there while `gc init` needed github.com (#6976); it runs there again since #7005. | A network-less executor already exists. |
| `HOME` is `TEST_TMPDIR`, which is per action. `TMPDIR` is unset, so `os.TempDir()` is `/tmp`. | Probe: `HOME="/tmp/bt/_tmp/<hash>"`, `TMPDIR=""`. | `$HOME` reads are hermetic under Bazel. Remotely, `/tmp` is private to each action (isolation hides other mounts). Locally it is the shared host `/tmp`. |
| Actions run as an isolated slot user, not root. | Probe: `uid=59001 USER=rbe-a01`. | Root-only `t.Skip` paths (about 25 sites) do run. |
| `PATH` is `.:/usr/local/go/bin:/usr/local/bin:/usr/bin:/bin`. It comes from the client wrapper and CI `--test_env`. | Probe. | The PATH string is keyed. The binaries it points to are not, which is the image-pin effort's concern. |
| `linux-sandbox` is unavailable on cherry and on Ubuntu 24.04 runners: `kernel.apparmor_restrict_unprivileged_userns=1`. | `bazel test --strategy=TestRunner=linux-sandbox` fails with "no strategy with that identifier was registered". `unshare -rn` fails with EPERM. | A local `--sandbox_default_allow_network=false` cannot enforce anything there without a sysctl change. |

## Findings

### Counts

| Class | Count | Packages / sites |
|---|---|---|
| (2) tagged `external` in this change | 4 targets | `test/integration`, `internal/testpolicy/resourcecensus`, `internal/testutil/providerledger`, `internal/beads/beadstest` |
| (1) fixed on main since the audit | 2 | `test/acceptance` (`gc init` no longer clones, #7005); `test/integration` start-drift (prebuilt `//cmd/gc:gc_drift_*`) |
| (1) fixed in this change | 4 fixes, 5 sites | 2030 credential expiry; 3 tzdata-dependent tests; the host-sshd test |
| (1) open follow-ups | 17 | See [Class 1: open](#class-1-open) |
| (3) fine, or covered by the image pin | ~10 groups | About 35 host-binary skip sites; about 25 uid-gated skips; about 300 data-only URLs; env-gated opt-in tests |

### Worst offenders

1. **`internal/testutil/providerledger`** (`ledger_test.go:1583`)
   - What it does: `Validate(entries, time.Now().UTC(), mode)` checks dated
     waivers against the real clock.
   - Why it is a problem: the earliest waiver expires on 2026-10-08 and turns
     fatal in grace mode on **2026-10-23**, with no code change. A PASS
     cached today stays green after that date.
   - Decision: class 2, now tagged `external`.
2. **`internal/testpolicy/resourcecensus`** (`census_test.go:2376`)
   - What it does: the same pattern against 88 rows that expire on
     2026-10-31.
   - Why it is a problem: it turns fatal on 2026-11-15.
   - Decision: class 2, now tagged `external`.
3. **`test/acceptance`**
   - What it did: `gc init` cloned gascity-packs from github.com for the
     default `gascity/roles` import (`internal/config/public_packs.go:29`,
     ga-73eoo).
   - Why it was a problem: this is the gating CI suite, and its result
     depended on GitHub.
   - Decision: class 1, fixed on main by #7005 (`a913e735bd`):
     `gascity/roles` is now a bundled subpack of the embedded gascity pack,
     so `gc init` resolves it offline. The full `acceptance_a` suite was then
     run from its Bazel runfiles inside a loopback-only network namespace
     (`unshare -n`, where `curl https://github.com` fails with "Could not
     resolve host"), on 2026-10-04 in its two CI shards: 101 of 102
     top-level tests passed. The one failure (`TestBeadsProxiedDefault`'s
     doctor-green subtests) reproduces identically with network and is a
     host-HOME leak (see Class 1: open). No test failed for lack of network,
     so the target carries no `external`/`requires-network` tag and is not
     in the ledger, and `bazel-test.yml` runs it on the fork pool again. Dolt's best-effort
     usage-metrics egress remains when a host `dolt` is on `PATH`; it does
     not change pass/fail (see the dolt metrics row under Class 1: open).
4. **`.bazelrc` `--test_env=GC_TEST_REPO_ROOT`**
   - What it does: when the variable is set, every repo-scan guard reads that
     live checkout instead of the declared `//:repo_source_tree`. This goes
     through `internal/bazeltest` `envRoot`/`OverrideRoot` and through
     providerledger's own root lookup.
   - Why it is a problem: only the path string is keyed, so edits to that
     checkout keep hitting a stale PASS.
   - Decision: class 1, open. Stop forwarding the variable by default, or
     ignore it when `bazeltest.IsBazel()`. It was left to the `.bazelrc`
     owners because other changes to that file are in flight.
5. **`internal/beads/beadstest`** (`conformance_skips.go:88`,
   `requireLedgeredSkip`)
   - What it does: conformance-skip expiries (2026-12-15) are checked against
     `time.Now()` inside every beads-store conformance run.
   - Why it is a problem: the date dependency reaches `internal/beads` and
     the other consumers. It turns fatal on about 2026-12-29.
   - Decision: the package's own guard test is tagged `external`. The
     consumers are an open class 1 item: make `requireLedgeredSkip` date-free
     and leave the expiry enforcement to the one never-cached guard.
     **Deadline: before 2026-12-15.**
6. **`test/integration` start-drift** (`start_drift_test.go:616`)
   - What it does: `go build ./cmd/gc` runs on the worker, downloading every
     module from proxy.golang.org into an empty private `GOMODCACHE`.
   - Why it is a problem: the result depended on the network.
   - Decision: class 1, fixed on main: under Bazel the drift tests install
     the prebuilt `//cmd/gc:gc_drift_old`/`gc_drift_new` binaries from
     runfiles. The package stays tagged `external` plus `requires-network`
     (`dolt_config_test.go:48` resolves the worker's hostname) until a
     network-less run of the integration tier proves the rest offline.

### Class 2: tagged in this change

These four targets are listed in `test/bazel-hermeticity.toml` with reasons
and evidence: `test/integration`, `internal/testpolicy/resourcecensus`,
`internal/testutil/providerledger`, `internal/beads/beadstest`. The ledger is
per package, so `tools/bazel/hermetic_tags.py` tags every `go_test` in a
listed package (`test/integration` has two). Under `bazel test //...` with
the default configuration, the integration targets compile only their
untagged files. The tag therefore costs little there. It bites in the
`--define=gotags=integration` invocation, which is where the network use
happens.

### Class 1: fixed in this change

- **`cmd/gc/remote_client_test.go:100,103`.** The credential-helper fixture
  expired on `2030-01-01`, and production code compares that date with the
  real clock. The test would have changed behavior in 2030 under a cached
  PASS. Moved to 2099.
- **Embedded zoneinfo.** `internal/events/events_test.go:333` (skipped
  without tzdata), `cmd/gc/clock_inject_test.go:16` (skipped), and
  `internal/orders/triggers_test.go:963` (failed) depended on the host's
  zoneinfo. They now import `time/tzdata`.
- **`internal/runtime/ssh` `TestConn_ExecOverRealLocalhost`.** The result
  depended on the host's sshd and on the passwd-entry user's keys: it passed
  on a dev box and skipped on workers under the same key. It moved to
  `ssh_localhost_integration_test.go` behind the integration build tag. The
  resource-census subprocess baselines were lowered by one call and one file
  to bank the move.

### Class 1: open

| Location | Undeclared input | Fix |
|---|---|---|
| `.bazelrc` `--test_env=GC_TEST_REPO_ROOT` | Live checkout contents | Drop the default forward, or have `bazeltest` ignore it under Bazel |
| `beadstest.requireLedgeredSkip` consumers | Calendar date | Make it date-free and keep the expiry in the `external` guard (deadline above) |
| `internal/agentutil/routed_to_boundary_test.go:14` | Nothing; it passes vacuously under Bazel because it scans no files | Use `bazeltest.RepoRoot` and add `//:repo_source_tree` to data |
| `internal/beads/contract/migrate_journal_test.go:33` | `$GOMODCACHE`, `$HOME/go/pkg/mod` (permanent SKIP) | `beadstest.PinnedBeadsModuleDir` plus a data dep |
| `internal/api` POST `/packs` tests (`idempotency_endpoints_test.go:379`, `handler_packs_write_test.go:57`, `handler_beads_test.go:2403`) | Real DNS for github.com through `ssrf.HostResolver`. It fails open, so the result is stable. | `stubPackSourceResolver(t, ...)` |
| `internal/doctor/checks_test.go:2917` | Host `python3` listener | In-process `net.Listen` |
| Acceptance tiers (`helpers/beads_topology.go:276,623`, `tool_home.go:85`) | Host `dolt`, while `@dolt_bin_v2_1_7` is already declared | Prepend the runfiles dolt directory to PATH in `TestMain`, as `integration_test.go` does |
| `examples/gastown`, `examples/bd/dolt` | Host `dolt`/`bd`, or a permanent skip (`requireRealBd`) | Declare `@dolt_bin_*` and `@bd_bin_*` and resolve them from runfiles |
| `cmd/gc` `beads_provider_lifecycle_test.go:8254` | Host `bd` with a skip | Use the runfiles `bd`, and fail rather than skip under Bazel |
| `tierc_test.go:1042`, `worker_inference/main_test.go:1158`, `tutorial_goldens/main_test.go:603` | Fixed `/tmp/gcac`, reaped across concurrent runs (local runs only) | Use `TEST_TMPDIR` |
| `scripts/test_go_test_shard_test.go:764,795`, `git_test_env_test.go:44` | Fixed-name `os.TempDir()/gc-...-gomodcache` (local runs only) | Use `t.TempDir()` |
| `scripts` tests that run real `go test`/`go env` | Toolchain download when the host `go` is older than `go.mod` | `GOTOOLCHAIN=local` |
| `internal/supervisor/maintenance_snapshot_test.go:451` | dolt metrics egress (pass/fail unaffected) | `"metrics.disabled":"true"` in the seeded config |
| `test/acceptance/helpers/env.go:127` (HOME swapped to the passwd home under Bazel) | `gc doctor`'s proxied-shared-server check reads `$HOME/.beads/shared-server` (`internal/doctor/checks_proxied_shared_server.go:203`) on the real home. On a host with a bd shared server there (cherry) `TestBeadsProxiedDefault/doctor-green{,-with-rig}` fail; clean worker homes pass | Resolve the shared-server root from the bd tool home, or seed an isolated home the supervisor accepts |
| `test/acceptance/helpers/env.go:166` (proxied/topology shapes with a host `dolt`) | dolt `send-metrics` egress to eventsapi.dolthub.com after each dolt exit (best effort; pass/fail unaffected, verified offline) | `"metrics.disabled":"true"` in the seeded `config_global.json` |
| `cmd/gc` `TestMain` | Product-metrics uploader egress (gated; pass/fail unaffected) | `GC_DISABLE_USAGE_METRICS=1` |
| 18 `cmd/gc` git-using files | `/etc/gitconfig`, `init.defaultBranch` | `GIT_CONFIG_NOSYSTEM=1` plus a temp global config in `TestMain` |
| `internal/runtime/tmux/main_test.go:28` | Literal `/tmp` socket parent, flock-guarded (local runs only) | Use `os.TempDir()` |

### Class 3: fine, or covered by the image pin

- **Host binaries through `PATH`:** git, jq, tmux, sh/bash, coreutils, and
  dolt. Some tests skip when a binary is absent, and a cached SKIP reads as a
  PASS: node, python3, flock, zsh, openssl, strace, setsid, systemctl, and
  ssh (about 35 sites). Pinning the image makes these stable. Turning each
  skip into a `t.Fatal` under `bazeltest.IsBazel()` would make a missing tool
  loud.
- **Permanent skips (deterministic, but a coverage hole):**
  - `internal/api/genclient` needs `oapi-codegen`, which is not pinned.
  - `internal/doctor` custom-types checks and
    `internal/beads/binary_versions_test.go` need `bd`.
  - Several env-gated opt-in tests never run under Bazel: `GC_FAST_UNIT=0`
    (63 `cmd/gc` slow tests, one of which runs `go install ...@ver` from the
    network), herdr live, `GC_TMUX_INTEGRATION`, and `GC_TEST_MCP_MAIL`.
  - `test/acceptance/tier_c` calls `os.Exit(0)` without inference
    credentials, so it is an empty PASS.
  - None of these are cache-trust bugs, because the outcome is fixed for a
    given key. If a variable is ever forwarded to enable the slow mode, it
    must get its own `external` target.
- **Data-only URLs:** about 300 in `cmd/gc` and more in `packman`, `rig`,
  `remotesource`, and `ssrf`. Network seams are stubbed (`runGit`, the clone
  seam, `HostResolver`, the registry HTTP client).
- **`os.MkdirTemp("/tmp", ...)`** for short socket paths: unique names, so
  safe.
- **`$HOME`:** set to `TEST_TMPDIR` under Bazel.

## Enforcement: making non-hermetic tests fail automatically

### Options evaluated

**(a) Run tests with the network disabled, and treat `requires-network` as
the opt-out.**

- Locally, `--sandbox_default_allow_network=false` needs `linux-sandbox`,
  which is unavailable on cherry and on stock Ubuntu 24.04 runners (see
  above). CI would need `sudo sysctl kernel.apparmor_restrict_unprivileged_userns=0`
  and full local execution, which is too slow on 2-vCPU runners.
- Remotely, the launcher already implements the switch (`NETNS=1`), but per
  worker tier rather than per action. A per-action version needs four pieces:
  1. A NativeLink `additional_environment` mapping from a platform property
     to the launcher, e.g. `"RBE_X_NETWORK": {"property": "network"}`.
  2. `rbe-action-launch` choosing `NETNS=1` unless the value is `allow`.
  3. The scheduler declaring the property.
  4. `exec_properties = {"network": "allow"}` on `requires-network` targets,
     written by `hermetic_tags.py` alongside the tags.

  The `rbe-action-*` files are copies of infra's, so this lands in infra
  first.
- Because the property is part of the action key, network-allowed results
  key separately from the rest.
- **Assessment:** this is the highest-fidelity check. It catches indirect
  paths that no static check sees, such as `gc init`'s clone or a `go build`
  module download.

**(b) Static check.** Prototyped here (`internal/testpolicy/bazelhermetic`).

- It parses `*_test.go` with import-aware AST matching and flags five
  signals:
  - DNS lookups;
  - literal non-loopback URLs or `host:port` passed directly to
    `net`/`http`/`tls` dialers or `exec`;
  - network CLIs (`gh`, `curl`, `ssh`, `docker`, `kubectl`, `go get`,
    `go mod download`, ...);
  - a fixed calendar date related to the wall clock (`time.Since(date)`,
    `date.Before(time.Now())`, ...);
  - writes to fixed `/tmp` paths.

  It also flags any test file that imports
  `internal/testpolicy/waiverclock`, since dated waivers make the outcome
  date-dependent. Package code importing it is not flagged: it may only build
  `waiverclock.Expiry` values.
- **Assessment:** fast and precise. It produces 12 findings on the whole
  repository. A first, coarser wall-clock rule produced 30, mostly
  fixed-clock false positives, and was narrowed. All 12 are covered by the
  ledger: 8 by `[[target]]` entries and 4 by `[[reviewed]]` entries. It cannot see indirect
  or non-literal paths, which is why it pairs with (a) or (d).

**(c) Read-only or empty `HOME` and a minimal `PATH`.**

- `HOME` is already per action under Bazel (measured), so a read-only empty
  `HOME` adds little.
- A minimal `PATH` forces tools to be declared as `data`. It overlaps with
  the image pin and breaks the ~40 files that run real git, jq, or tmux
  until they are runfile-backed, so it costs a lot for now.
- A cheaper variant is to poison the network env for remote CI runs:
  `GOPROXY=off`, `HTTP(S)_PROXY`/`ALL_PROXY=http://127.0.0.1:9`, a loopback
  `NO_PROXY`, and `GIT_ALLOW_PROTOCOL=file`. This makes Go HTTP clients,
  git over https, curl, and module downloads fail loudly with no infra
  change.
- Caveats for the variant:
  - It misses raw `net.Dial` and custom transports.
  - `--test_env` applies to every target, so `requires-network` targets
    would need a way out.
  - Changing it re-keys the whole suite once.

  It is a stopgap if (a) slips.

**(d) Canary double-run.**

- A scheduled job re-executes a random sample of targets that passed from
  the cache, plus every ledger-reviewed and dated-ledger package, with
  `--noremote_accept_cached --nocache_test_results`. Comparing the outcome
  with the cached one and alerting on divergence catches every class,
  including time bombs on the day they detonate and flakes.
- Running it on the `oss-fork` instance makes it combine (a) and (d) with no
  infra change: that pool is network-less and never caches, so anything
  that passes on `oss` but fails there is network-dependent.
- **Assessment:** lagging, but complete. It is the safety net under (b).

**(e) Tags that survive regeneration.**

- Verified that gazelle preserves a `go_test` `tags` attribute: after a
  standalone `bazel run //:gazelle`, `hermetic_tags.py --check` exits 0.
- The prototype makes the ledger the source of truth.
  `tools/bazel/hermetic_tags.py` runs in `make bazel-sync` and sets the
  managed tags (`external`, `no-cache`, `no-remote-cache`,
  `no-remote-exec`, `requires-network`) to exactly the ledger's on the
  listed packages, and strips them everywhere else.
- The existing CI "BUILD files in sync" job therefore fails on a hand-edited
  or missing tag.
- A gazelle language extension that derived tags from source was rejected.
  It would need a custom gazelle binary, and it would move a reviewed
  judgment ("this network use is acceptable") into an unreviewed generator.

### Recommendation: layered

1. **Now (this change): (b) + (e).** The static ratchet runs in
   `bazel test //...` and in `go test`. The ledger records every decision
   with a reason. Tags are generated from the ledger and checked by the sync
   job. The class 2 targets are tagged and the cheap class 1 fixes are made.
2. **Next: (d) on `oss-fork`.** A nightly `bazel test //...` there with
   `--noremote_accept_cached`, compared against `oss`, needs no infra
   change. Every failure is either a ledger entry to add or a fix to make.
3. **Then: (a) per-action network-off** on the `oss` tier through the
   platform-property plumbing above, with `requires-network` mapped to
   `network=allow`. Undeclared network use then fails in the PR that
   introduces it.
4. **Dates as declared inputs.** Dated ledgers keep the `external` tag until
   `waiverclock` reads "today" from a declared input under Bazel, e.g. a
   `@gc_today` repository rule keyed on `--repo_env=GC_TODAY` set by CI and
   the cherry wrapper. Only targets that depend on it then re-key daily.

## The prototype

- **Scanner and ledger check:** `internal/testpolicy/bazelhermetic`.
  - `TestScanFlagsDeliberatelyNonHermeticTest` proves every signal fires on
    a deliberately non-hermetic fixture and stays silent on a hermetic twin.
  - `TestCheckFailsUntilLeakyPackageIsLedgered` proves the ledger mechanics.
  - `TestRepositoryTestsAreHermeticOrLedgered` enforces against the real
    tree.
- **Ledger:** `test/bazel-hermeticity.toml`.
- **Tag sync:** `tools/bazel/hermetic_tags.py`, wired into `make bazel-sync`.

Evidence from 2026-10-04 on rbe-west:

1. A deliberately leaky test was dropped into `internal/pidutil`. It fetches
   `https://api.github.com/.../releases/latest` and asserts on
   `time.Since(time.Date(2026, ...))`. It passed remotely, and the rerun was
   served from cache:

   ```
   //internal/pidutil:pidutil_test                                          PASSED in 0.1s
   //internal/pidutil:pidutil_test                                 (cached) PASSED in 0.1s
   ```

2. `bazel test //internal/testpolicy/bazelhermetic:bazelhermetic_test` then
   failed:

   ```
   --- FAIL: TestRepositoryTestsAreHermeticOrLedgered (0.69s)
       scan_test.go:240: 2 Bazel hermeticity problem(s):
             internal/pidutil/zz_leak_test.go:10: external-url: http.Get("https://api.github.com/repos/gastownhall/gascity/releases/latest") reaches api.github.com
                 undeclared test input in internal/pidutil: make the test hermetic, or add the package to test/bazel-hermeticity.toml as a [[target]] with a cache-exempt tag (external, no-cache, no-remote-cache), or as a [[reviewed]] "external-url" entry explaining why the result cannot change
             internal/pidutil/zz_leak_test.go:15: wall-clock: time.Since(time.Date(2026, ...)) depends on the date the test runs
                 undeclared test input in internal/pidutil: ...
   ```

3. A `[[target]]` entry with `tags = ["external", "requires-network"]` was
   added for the package. `hermetic_tags.py --check` then reported drift for
   `internal/pidutil/BUILD.bazel`, and `hermetic_tags.py` wrote the tags.
   After that the check passed, and the leaky target re-executed on every
   invocation while the check itself was cached:

   ```
   //internal/testpolicy/bazelhermetic:bazelhermetic_test          (cached) PASSED in 2.3s
   //internal/pidutil:pidutil_test                                          PASSED in 4.2s
   Executed 1 out of 2 tests: 2 tests pass.
   ```

4. Removing the entry and re-running `hermetic_tags.py` stripped the tags
   again.

### Adding to the ledger

When `TestRepositoryTestsAreHermeticOrLedgered` fails, work through these in
order:

1. Make the test hermetic: declare the input as `data`, stub the seam, or
   inject the clock.
2. If the result can genuinely change without a code change, add a
   `[[target]]` with `external` (and `requires-network` if it needs the
   internet). Run `make bazel-sync` and commit the BUILD change.
3. If the signal is a false positive, add a `[[reviewed]]` entry. Its reason
   must say why the result cannot change. Stale reviewed entries fail the
   check, so the ledger cannot outlive the code it excuses.
