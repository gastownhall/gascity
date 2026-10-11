# Release gate: tmux held-name start preservation

**Verdict:** **PASS**

Bead: ga-pjp7jz. Review: ga-m4j60r, closed PASS. Build: ga-ttdp2u.
Reviewed source: `e5f2b10cffe19560246f1babb8b17af226dbcdc7`.
Approved carry: `c04a25be4942df98beb47fd46da7d235cad0339f`; literal restatement and gated source: `5f43bc75c670892b4092abfc198054c56e100711`.
Pinned base: `4b8c9234349e4e31cdb7b637060b421e868294e9`.
Canonical merge: `5f92ebe69bf9b7cdad360a27bbf924ac7984732c`; tested tree: `173ec6f0e38fbb3b8b22e8d67273b62c1e35683b`.
Mode: remote. Publication branch: `deploy/ga-pjp7jz-gate`.

| # | Criterion | Result and independent evidence |
|---|---|---|
| 1 | Review PASS present | PASS. Exact-source review ga-m4j60r; mayor authority gm-wisp-eqxfedf permits only the guarded local carry and one literal-only inventory restatement. Original red/green objects and patch IDs remain unchanged and reviewed source is an ancestor. |
| 2 | Acceptance criteria met | PASS. An ErrSessionExists refusal invalidates state cache and returns unchanged before teardown. Four new fake-executor roots cover legacy held-name shapes, FreshOnly duplicate-create refusal, matching/different-token cache invalidation, real own-create-failure cleanup and retained legacy pre_start order. All six owned roots and five unchanged FreshOnly regression roots actually PASS in full unit; all four owned tmux roots also PASS in full integration. |
| 3 | Tests pass | PASS. Fresh whole-repo make check, acceptance, full integration packages and prescribed integration smoke all complete on first uncached attempts: 272 configured targets /425 shard jobs PASS. Raw named results: 92,300 PASS /0 FAIL /278 non-owned SKIP. All104 process tests excluded by unit mode have fresh matching integration PASS, including TestTutorial01. No attribution or waiver. |
| 4 | No high-severity findings open | PASS. Live exact review has zero unresolved HIGH findings and no blocker/major/minor style or security findings. |
| 5 | Final branch is clean | PASS. Clean before gate-only commit; publication verifier requires exactly one gate path in the commit and a clean tree afterward. Normal commit and push hooks remain enabled. |
| 6 | Branch diverges cleanly from main | PASS. Original conflict only in scripts/runtime_tmux_manifest_test.go resolved under the explicit guarded-carry authority. Main alone and final source each execute all5 guard roots PASS. Normal canonical merge matches predicted tree exactly; whole-repo build/nogo1232 targets PASS. Final read-only merge-tree against newly fetched main `59532a7612e7554c0802987ce8d668052dba641f` also succeeds with tree `79a3bb2eb3b0f9df733987f2d6fedb3f1b5c9691`. |
| 7 | Single feature theme | PASS. One provider Start ownership-refusal behavior; four regression tests, BUILD registration and necessary tmux inventory/count changes. The accepted source citation ga-zp31jc is independently confirmed as this same feature and exact reviewed red/green pair. No unrelated theme or internal doc contamination. |

test_cmd_scope: full-suite
test_cmd: make check (bazel test //...); bazel test --config=acceptance //test/acceptance:acceptance_test //test/acceptance:acceptance_solo_tests; bazel test --config=integration //test:integration_packages; bazel test --config=integration-smoke //test/integration:integration_test
Common: --config=ci --config=fork-cache --nocache_test_results --jobs=4 --remote_download_outputs=all --rewind_lost_inputs. Private isolated bd1.3.1, Dolt2.2, ICU74, Podman socket and tmux3.7c verified before load wait. Agent host uses the documented anonymous cache/local-miss mode; no RBE claim.

| Lane | Targets PASS | Shard jobs PASS | Raw PASS | FAIL | SKIP |
|---|---:|---:|---:|---:|---:|
| Unit /make check | 238 | 318 | 55,208 | 0 | 211 |
| Acceptance | 7 | 24 | 410 | 0 | 13 |
| Integration packages | 26 | 69 | 36,666 | 0 | 53 |
| Integration smoke | 1 | 14 | 16 | 0 | 1 |

diff_tests_executed: all6 owned roots actualPASS, no ownedSKIP/FAIL; all11 required roots PASS.
waiver_ref: none
fix_carrying: none
attribution: none
heavy_mode: none (computed classifier; unchanged module pins)
ci_lane_run: n/a — no CI config diff.
policy_lane: PASS, 11 independently executed FAST policy targets and required shell guards, hooks check, lint and CI policy; each phase gets a fresh view with the canonical first-parent base pinned.
drift_lane: PASS, all6 generated-artifact targets plus make bazel-sync and empty tracked/untracked status. Standard OpenAPI gate uses a regular copy of pinned-base openapi.json; actual TestCommittedSpecAgainstBase and TestGateAgainstOasdiff PASS with no SKIP. Total18 FAST uncached target runs PASS before runtime/load/full suite.
Required CI leaf accounting from pinned workflow: runner-policy and changes LOCAL-PASS; credential-provider-windows and pack-gate NOT-TRIGGERED; no uncovered/deferred required leaf. No actual GitHub CI result is claimed; its checks still gate merge.

load_evidence: `LOAD_GATE_SUMMARY threshold=15 waited_seconds=1800 wait_timed_out=1 run_start_load=37.89 run_max_load=82.58 run_mean_load=44.50 run_readings=66 wait_first_load=52.28 wait_max_load=52.28 wait_mean_load=27.48 wait_readings=61 read_errors=0`
The ordinary threshold15/max1800 wrapper exhausted its bounded wait and continued under its documented default policy; this is an actual recorded load result, not a load exemption. The heavy-path exception does not apply.

Skip integrity: 278 raw rows, each with exact source site, reason, log/line, source context, unchanged-site and shared-hunk check plus complete tag-specific module import closure and causal argument. All211 unit skips accepted only after matching all104 unit-mode process exclusions to real integration PASS. Other skips are unchanged OS/explicit opt-in/helper/optional tool-service/fixture or unconditional characterization guards; reached tmux/cmd guards execute before provider Start. All53 integration skip files are byte-identical to pinned main; smoke's unconditional old conformance skip executes before setup. The opt-in tmux dogfood is still SKIP; no actual passing dogfood is claimed. New fakes add no TestMain/init/global registration. The manifest changes only the four owned untagged test names, no existing member/tag removal.

Guarded carry proof: main588/433untagged/155integration-only, shards98x6; merged592/437/155, shards99,99,99,99,98,98. Delta exactly the four reviewed untagged roots, no removal/tag drift. The separate restatement changes only three count-literal lines. Main-side resolution only in the authorized file. Normal hooks, original-source ancestry and unchanged reviewed patch IDs independently verified. No self-rebase/force push.

test_log_dir: `/var/tmp/gc-heavy-gate/runs/ga-pjp7jz-current.c3/logs`
Raw Bazel logs/XML/BEP and every artifact SHA are retained under `/var/tmp/gc-heavy-gate/runs/ga-pjp7jz-current.c3/bazel-artifacts` and per-lane artifacts manifests; named inventories and mechanical skip proofs are alongside them.
Independent evidence root: `/var/tmp/deploy-ga-pjp7jz-current.gl4j1ay3`. FULL_SUITE_VERIFIED.json, UNIT_SKIP_CAUSAL_VERIFIED.json, ACCEPTANCE_SKIP_CAUSAL_VERIFIED.json, INTEGRATION_PACKAGES_SKIP_CAUSAL_VERIFIED.json, INTEGRATION_SMOKE_SKIP_CAUSAL_VERIFIED.json, IMPORT_REACH_VERIFIED.json, SOURCE_CRITERIA_VERIFIED.json, C6_VERIFIED.json, CARRY_AND_RESTATEMENT_VERIFIED.json and FINAL_MANIFEST_GUARD_VERIFIED.json. FAST_VERIFIED.json and ACCOUNTING_VERIFIED.json are in their corresponding detached run directories. All prior setup-only failed logs remain preserved; no test rerun or failure attribution was used.

Scope limits: Refs #7170, Layer1of2 only; manager killExistingOrphans remains Layer2. Legacy pre_start order and FreshOnly remain. No per-attempt nonce or recycle lost-create-race fix. R3b refresh back-off × cache invalidation remains unchecked. This gate does not claim the entire issue is fixed.
