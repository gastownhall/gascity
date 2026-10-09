---
title: "Bazel Remote Repo Contents Cache and Skycache on rbe-west"
---

| Field | Value |
|---|---|
| Status | Proposed |
| Date | 2026-10-09 |
| Author(s) | Claude |
| Program | ga-vnycm2 (PR CI to a 5-minute required gate) |
| Bead | ga-vnycm2.13 |
| Supersedes | N/A |

## Summary

Every CI lane starts a cold Bazel client, and that client spends 15-38 s in
loading and analysis before the first remote action runs. Most of that time
goes to fetching external repositories: copying ~280 Go modules and running
gazelle on them (`go_deps`), creating ~1650 npm repos, and extracting debs.
Bazel 9 has two features that could remove that time. Both write cache
entries from the client, and rbe-west's D2 policy allows action cache writes
from workers only.

| Feature | Verdict |
|---|---|
| **Remote repo contents cache** (`--experimental_remote_repo_contents_cache`) | **Recommended.** It halves lane analysis time. The prerequisites: Bazel 9.3.0, `--loading_phase_threads=64`, and a narrow write path. That write path is one writer identity, used only by main-push CI, whose action-cache writes go through a gate that admits repo-contents entries and nothing else. D2 stays unchanged for every other identity. |
| **Skycache** (`--experimental_remote_analysis_cache*`) | **Not possible in OSS Bazel.** Bazel 9.2.0, 9.3.0 and master ship only an in-memory backend, and `--experimental_remote_analysis_cache` is never read. Its entries also could not be verified (see below). Do not pursue it. |

Measured on Blacksmith runners (the lanes' own runner sizes), with the
`analysisPhaseTimeInMs` value that CI analytics reports:

| Lane (runner) | Main today (last 4 push runs) | Prototype, as-is (9.2.0) | Prototype, 9.3.0 + repo contents cache + `--loading_phase_threads=64` (3 runs) |
|---|---|---|---|
| unit (8 vCPU) | 16.1-28.1 s | 24.5 s | **10.1-10.5 s** |
| acceptance (8 vCPU) | 15.0-20.6 s | 14.3 s | **9.1-9.4 s** |
| integration-packages (4 vCPU) | 25.1-38.3 s | 29.2 s | **14.8-17.1 s** |
| integration-smoke (4 vCPU) | 23.5-34.3 s | 26.1 s | **11.7-13.4 s** |

The 8 vCPU lanes reach the ~10 s target. The 4 vCPU lanes halve, but stay
above it; on 8 vCPU they would land near 10 s.

## Background: where cold-client time goes

Each lane runs `bazel test --config=ci ...` on a fresh Blacksmith VM. Only
the content-addressed `--repository_cache` is restored from the runner
cache. The extracted repo contents cache is deliberately off (`--repo_contents_cache=`
in `setup-bazel/write-bazelrc.sh`): its extracted repos would come back from
the runner cache unverified. So every lane re-runs every repository rule:
extraction, `fetch_repo`, gazelle BUILD generation, npm repo creation.
ga-vnycm2.8 removed the network part (gazelle's tools come from the
repository cache) and moved the critical lanes to bigger runners. The rest
is CPU and I/O on the client.

## Skycache: not available, and not verifiable

### What exists in Bazel 9.2/9.3

- **Flags.** `RemoteAnalysisCachingOptions` defines
  `--experimental_remote_analysis_cache_mode=upload|download|dump_upload_manifest_only|off`,
  `--experimental_remote_analysis_cache` ("URL for the backend"),
  `--experimental_analysis_cache_service`,
  `--experimental_remote_analysis_write_proxy` (a
  `SkycacheStorageWriteProxyService`) and tuning knobs. Activation also
  needs `PROJECT.scl` active directories.
- **No persistent backend.** In OSS Bazel the services come from
  `SerializationModule.getAnalysisCachingServicesSupplier()`. In 9.2.0,
  9.3.0 and master, that method returns
  `InMemoryRemoteAnalysisCachingServicesSupplier`, an in-process
  `FingerprintValueStore` marked `// TODO: b/358347099 - use a persistent store`.
  No OSS code reads `remoteAnalysisCache`.
- **Google-internal protocols.** The storage, AnalysisCacheService and
  write-proxy protocols have no public server implementation. NativeLink
  implements none of them.
- **Cost of using it anyway.** We would need a forked Bazel binary with a
  custom `BlazeModule` and a backend we write ourselves.

### Why its entries could not be trusted anyway

- **Unverifiable values.** Skycache values are serialized Skyframe nodes:
  configured targets, actions, command lines and their keys. Nothing on the
  reader can check a value against its key without redoing the analysis it
  replaces.
- **Blast radius of one poisoned entry.** It injects an arbitrary action
  graph: any command, any test, any output, on every worker and every
  reader.
- **Untrusted input to an internal codec.** The bytes are deserialized by
  Bazel's internal codec registry, which was never designed for hostile
  input.
- **Not like the repo contents cache.** There, a poisoned entry can only
  substitute the source tree of one external repository (next section).

**Recommendation:** close the Skycache line. Revisit only if upstream ships an
OSS backend with an integrity story.

## Remote repo contents cache

### How it works (Bazel 9.2/9.3 source, `RemoteRepoContentsCacheImpl`)

- **Opt-in and eligibility.** `--experimental_remote_repo_contents_cache` is
  a startup option. Only *reproducible* repository rules are cached (those
  returning `repo_metadata(reproducible = True)`), and not `local` ones. In
  the prototype, all but 9-16 of the repos `//...` needs qualified (3690
  entries written).
- **Storage format.** A repo is stored as the ActionResult of a synthetic,
  never-executed action:
  - Command: one argument, a per-release UUID (9.2.0 `0336b325-...`, 9.3.0
    `06a53d89-...`).
  - Outputs: `.recorded_inputs` (the marker file) and `repo_contents` (the
    tree).
  - Input root: empty.
  - Salt: the repo's predeclared-inputs hash.
- **Rules that read inputs at fetch time.** These chain through
  *intermediate* entries whose stdout lists recorded inputs. The reader
  evaluates their current values and rolls the hash forward.
- **Cache hit.** Bazel downloads the marker and the Tree proto only, and
  injects the file metadata into an in-memory overlay filesystem. It reads
  file contents only when Bazel itself reads them (BUILD and .bzl files) or
  a *local* action needs them.
  - Remote actions get the files as inputs by digest. They are already in
    the farm's CAS, so they are never downloaded or uploaded.
  - In the prototype's remote-execution check, `rules_go`'s `go_sdk` (270 MB) was
    the largest repo materialized on the client. Most `go_deps` repos
    existed on disk only as `REPO.bazel`.
- **Gating.** Reads are gated by `--remote_accept_cached`. Writes are gated
  by `--remote_upload_local_results`, which CI sets to false, and by D2 on
  the server.

### Integrity: what is and is not self-verifying

- **Self-verifying.** The CAS blobs (file contents and the Tree proto) are
  content-addressed, and rbe-west's CAS is wrapped in
  `verify{verify_hash, verify_size}`.
- **Not self-verifying.** The mapping from key (predeclared hash) to Tree is
  an action-cache entry. A reader cannot check that the Tree is what the
  repo rule would produce without re-running the rule. That is the work the
  cache exists to skip.
  - `http_archive`'s sha256 covers the *archive*, not the extracted tree
    plus generated BUILD files.
  - `go_repository`'s `sum` covers the module zip, not gazelle's BUILD files.
- **Blast radius.** A malicious writer could map an honest key to a
  malicious tree, for example a Go module with an extra `init()`, or a
  BUILD file that adds a dependency. That tree would then be compiled into
  binaries and tests for every reader.
- **Compared with Skycache.** That is smaller than Skycache's blast radius,
  and it hits the same place a malicious upstream release would.
  - Entries can only be *source trees* for repos the reader already
    declared.
  - No reader deserializes anything beyond a marker file and a Tree proto.
- **Consequence.** The writer must be trusted. The design limits *who* can
  write, and *what* that writer can write.

### Write path design

```text
main push (bazel.yml)                                  PR / fork runs
  rrc-seed job (no tests run)                           lanes (readers)
  OIDC token  ──>  rbe mint  ──>  30-min cert            existing certs
                     CN=rbe-rrc-writer,O=gascity                │
        │                                                      │
        ▼                                                      ▼
  Caddy :443 ── subject == rbe-rrc-writer ──┐          Caddy :443 (unchanged)
     GetCapabilities      -> :50053 (scheduler-oss)       GetActionResult (AC_OSS)
     CAS / ByteStream     -> :50065 (oss-only verified CAS)
     ActionCache/*        -> rrc-gate :50066 ──> :50056 (AC_OSS, oss only)
     anything else        -> PERMISSION_DENIED (gRPC 7)
```

1. **A writer identity used only by main-push CI.**
   - **Preferred (D14 in `rbe-unified-topology-design.md`).** The bazel.yml
     `rrc-seed` job trades its GitHub OIDC token for a short-lived client
     certificate `CN=rbe-rrc-writer,O=gascity`. The mint issues it only for
     these claims:
     - `repository == gastownhall/gascity` (and beads, with its own CN for
       audit);
     - `ref == refs/heads/main`;
     - `event_name == push`;
     - `job_workflow_ref == gastownhall/gascity/.github/workflows/bazel.yml@refs/heads/main`.

     PR, fork and Dependabot runs cannot obtain it. Fork runs get no OIDC
     token, and a same-repo PR's token carries `refs/pull/N/merge`.
   - **Stopgap.** A GitHub environment `rbe-rrc-writer` restricted to the
     `main` branch, holding a long-lived writer cert, revocable through
     `/etc/caddy/rbe-west/revoked.caddy`. It needs less server code, but the
     credential is long-lived.
2. **A gate that scopes the writer to repo-contents entries.**
   - `rrc-gate` is a ~250-line Go ActionCache front, prototyped on the infra
     branch `rbe/rrc-gate-prototype`, in `nativelink-cas/rrc-gate/`.
   - **Where it checks.** For each `UpdateActionResult`, it reads the Action
     named by the action digest from the verified CAS and re-checks the
     sha256 itself.
   - **What it admits.**
     - Action: empty input root, no platform, no timeout, cacheable, salted.
     - Command: exactly one UUID argument; output paths exactly
       `.recorded_inputs` and `repo_contents`; no environment.
     - ActionResult: either the final shape (that file plus that directory)
       or the intermediate shape (stdout only); exit 0; no symlinks; no
       stderr.
   - **Why it holds.** A real build or test action has a different Command,
     so its key cannot pass. Forging a real action's result would need a
     sha256 preimage. D2 ("only workers write execution results") therefore
     holds for every identity, the writer included.
   - **Release-proof.** The check is structural, so a Bazel release that
     rotates the UUID needs no gate change.
3. **Readers change nothing on the server.** Same-repo PR and main lanes
   already read `AC_OSS` with their existing certificates.
   - Fork lanes on instance `oss` (fork-rw) read it through the fork edge.
   - The anonymous rbe-cache could serve it read-only later.
4. **No test code runs with the writer credential.** `rrc-seed` runs
   `bazel build --nobuild` over the union of the lanes' target patterns,
   once per lane config. Only repository rules and Starlark from `main`
   execute there: gazelle, `fetch_repo`, pnpm-lock parsing, extraction. That
   is the same code every lane already runs on the runner.
5. **Audit.** Caddy's `rbe_west_ac_writes` log already records every
   `UpdateActionResult` with the certificate CN. The gate logs every
   admitted and refused write. `check_entries.py` audits an AC offline with
   the same rule.

**Residual risks (for owner decision)**

| # | Risk | Mitigation |
|---|---|---|
| R1 | A compromised `main` (a merged malicious PR, or a compromised BCR/Go/npm dependency that runs in a repo rule) poisons repo trees for every reader | The same compromise already controls what `main`'s lanes build. The new part is that the poison sits in the cache rather than in git. Mitigations: short-lived OIDC certs; the audit log; and a nightly `rrc-verify` job (follow-up). That job fetches cold with no cache and compares each repo's tree digest with the cached final entry. A mismatch means poisoning, or a rule falsely marked reproducible |
| R2 | A rule marked `reproducible` that is not (for example, it branches on host OS without recording it) serves a Linux tree to another host | Readers are Linux x86_64 CI lanes only. The startup flag goes in setup-bazel's CI rc, never in the committed `.bazelrc`. Developer machines and macOS/Windows jobs never read |
| R3 | Cross-repo sharing: gascity's writer can fill entries that beads reads (the same rule and attributes give the same key) | Both are gastownhall-owned main branches. Per-repo CNs keep the audit separable. If isolation is wanted, salt the keys per repo (not possible without a Bazel flag) or run a separate AC instance per repo (needs a scheduler instance per repo) |
| R4 | Bazel bugs in an experimental feature | #30218: a CAS blob missing during materialization fails the command, and the rule is not re-run. AC_OSS's `completeness_checking` already refuses entries with missing blobs at lookup time, and the CAS is large, so this is rare. CI's `--experimental_remote_cache_eviction_retries=0` makes it a visible lane failure. #31197: `--rewind_lost_inputs` crashes; CI does not use it. #31005: repos read by extensions are fully materialized; measured fine at 1 ms RTT. Fixed in 9.3.0: #30905 (serialized overlay reads), #31048 (cross-command invalidation), #30223 (local actions). Track #31456 (stabilization) |
| R5 | Server load: each cold lane read issues ~3700 AC lookups (completeness-checked) plus the Tree and BUILD/.bzl blob reads | About 18 CPU-s of NativeLink per cold unit-lane read (local NativeLink 1.7.1, filesystem stores). About 60-70 CPU-s per 4-lane PR run, in bursts. Watch core CPU during a canary |
| R6 | The cache is unreachable or slow | With an unreachable cache, the prototype fell back to fetching but took 467 s (per-repo retries). In CI the cache is the executor endpoint, so the lane is down anyway. At 100 ms RTT, reads take about 2x the 1 ms numbers |

### Client prerequisites (all key-neutral)

- **Bazel 9.3.0.** Under 9.2.0 the overlay filesystem serializes
  `getInputStream` (fixed by bazelbuild/bazel#30905, in 9.3.0). In the
  prototype on the shared cherry host, 9.2.0 with the cache took 128-130 s
  of analysis at 44 ms RTT against 32 s without it. 9.3.0 alone also helps:
  unit 24.5 s to 18.8 s.
- **`--loading_phase_threads=64`.** Each cache hit is several sequential
  round trips, and ~3700 repos are resolved through Skyframe's loading
  threads, which default to the CPU count. Without it, cached reads are
  *slower* than fetching: unit 27.1 s, integration-smoke 55.3 s. Without the
  cache, the flag changes nothing measurable (fetching is CPU-bound).
- **Key neutrality.** Neither flag changes action keys. In the prototype's
  remote-execution check, actions that ran with repo contents from the cache
  were all remote cache hits for a run without it (2693/2693). Both flags
  belong with the rc's transport flags (`scripts/bazel_key_parity_test.go`
  classification).

## Prototype and measurements

Everything ran against throwaway NativeLink 1.7.1 instances configured like
rbe-west's instance `oss`: a `verify` CAS and a `completeness_checking` AC.
**Nothing on rbe-west was written or reconfigured.**

1. **Blacksmith, per lane.** Branch `ci/bazel-rrc-proto`, workflow
   `rrc-proto.yml` (throwaway, never merged), run
   https://github.com/gastownhall/gascity/actions/runs/37967840371.
   - Setup:
     - NativeLink on the runner, behind a userspace delay proxy.
     - The real runner cache (`--repository_cache`, LLVM archive) restored
       as in bazel.yml.
     - A cold output base per run, `bazel test --nobuild` with each lane's
       command.
   - The runner's TCP connect RTT to `rbe-west.ops.gascity.com:443` was
     **1 ms**: Blacksmith and rbe-west are co-located. So "rttM" below is
     the real CI condition.

| Lane | 9.2.0 as-is | 9.3.0 | 9.3.0 + lpt64 | write (9.3.0) | read, lpt64, RTT 1 ms (x3) | read, no lpt64 | read, lpt64, RTT 0 | read, lpt64, RTT 100 ms |
|---|---|---|---|---|---|---|---|---|
| unit, 8 vCPU | 24.5 | 18.8 | 17.0 / 20.3 | 30.7 | 10.5 / 10.1 / 10.4 | 27.1 | 10.5 | 21.6 |
| acceptance, 8 vCPU | 14.3 | 15.5 | 13.2 / 15.4 | (hit) | 9.1 / 9.2 / 9.4 | 12.6 | 8.5 | 24.4 |
| integration-packages, 4 vCPU | 29.2 | 27.7 | 24.6 / 26.4 | 55.7 | 15.1 / 14.8 / 17.1 | 48.6 | 16.3 | 30.2 |
| integration-smoke, 4 vCPU | 26.1 | 23.8 | 23.5 / 22.1 | (hit) | 13.4 / 12.9 / 11.7 | 55.3 | 13.5 | 31.1 |

   Notes on the table:
   - All values are `analysisPhaseTimeInMs`, in seconds.
   - The full client command adds about 4-6 s (JVM start, module
     resolution, the non-reproducible `bazel_gazelle_go_repository_cache`
     at ~3 s, the python toolchain at ~1.5 s). Unit's wall time went from
     29.8 s to about 17 s.
   - Repository rules executed per read: 9-16, against 300-600 cold.
   - Writer cost: +11-28 s on the job that seeds. The CAS held 3.9 GB and
     the AC 3690 entries for unit plus acceptance; the other two lanes add
     almost nothing.

2. **Gate, end to end (cherry).** Bazel 9.3.0 wrote through Caddy and
   `rrc-gate` into a fresh NativeLink, mirroring the proposed route.
   - All 3690 repo-contents writes were admitted.
   - A locally executed genrule uploaded with `--remote_upload_local_results`
     was refused: `PERMISSION_DENIED: rrc-gate: not a repo contents cache entry: input root ... is not the empty directory`.
     The build still succeeded, with a warning.
   - A fresh reader then hit every entry: 12.5 s analysis, against ~32-39 s
     cold on the same host.
   - `go test` covers 13 refusal cases: input root, salt, timeout, command
     env/args, instance, absent action, exit code, extra or renamed outputs,
     symlinks, empty intermediate, stderr, and a blob that does not match
     its digest.
3. **Remote execution with cached repos (cherry, local NativeLink
   scheduler and worker).**
   - Command: `bazel test //internal/config:config //internal/citylayout:citylayout_test`
     with `--remote_download_minimal`.
   - 2693 remote actions ran on sources that never reached the client, and
     the test passed.
   - A run without the cache was then 2693/2693 remote cache hits
     (key parity).

## Rollout (each step needs owner approval; nothing here is deployed)

1. **Bazel 9.3.0 bump (gascity, then beads).**
   - Files: `.bazelversion`; the setup-bazel sha pin
     `9.3.0/amd64 d302d22e...`, arm64 from the release; a
     `MODULE.bazel.lock` refresh (9.3.0 rewrites it).
   - Has value on its own, and is a hard prerequisite.
   - Expect a cold action cache for one cycle if any builtin rule's command
     line changed.
2. **Server (rbe-west core). Owner deploys.**
   1. Writer identity. Preferred: an OIDC path in `rbe-fork-mint` (D14) for
      the claims above, issuing 30-minute `CN=rbe-rrc-writer,O=gascity`
      leaves (per repo: `rbe-rrc-writer-beads`). Stopgap: one leaf in a
      `main`-only GitHub environment.
   2. Build and install `rrc-gate` as a systemd unit:
      - user `rrc-gate`;
      - `127.0.0.1:50066`, `-ac 127.0.0.1:50056 -cas 127.0.0.1:50065 -instance oss`;
      - `ProtectSystem=strict`, no network but loopback.
   3. Caddyfile: a `@rrc_writer` subject block before `@oss_worker`'s
      fallthrough:
      - GetCapabilities to `:50053`;
      - CAS and ByteStream to `:50065`;
      - `/build.bazel.remote.execution.v2.ActionCache/*` to `:50066`;
      - everything else gRPC 7.

      Add it to `rbe_config_lint.py trusted-edge` and to the selftest. The
      selftest checks that a repo-contents write is admitted, a real action
      write is refused, and Execute is refused.
   4. Nothing else changes: no NativeLink config, no listener, no D2 rule
      for any other certificate.
   5. Rollback: delete the Caddy block (`systemctl reload caddy`) and stop
      the unit. Entries already written expire under AC_OSS's LRU. Readers
      fall back to fetching.
3. **CI (gascity PR).**
   - Add an `rrc-seed` job to `bazel.yml` on `push` to `main` (needs
     `id-token: write`). It runs `bazel build --nobuild` for each lane's
     config with `--remote_upload_local_results` and the writer cert, in
     parallel with the lanes.
   - setup-bazel's `write-bazelrc.sh` adds
     `startup --experimental_remote_repo_contents_cache` and
     `common --loading_phase_threads=64` in remote modes only.
   - Classify both in `bazel_key_parity_test.go`.
   - Canary first: add the flags to the unit lane only, compare analytics,
     then all lanes. Then beads.
4. **Follow-ups.**
   - `rrc-verify` nightly (R1).
   - Fork-rw and the anonymous cache as readers.
   - Making `bazel_gazelle_go_repository_cache` (~3 s, non-reproducible)
     cacheable or unnecessary.

Expected after steps 1-3, from the prototype:
- unit and acceptance analysis about 9-11 s;
- integration-packages and integration-smoke about 12-17 s on 4 vCPU.

That cuts about 5-15 s from every lane's critical path, on every PR.
