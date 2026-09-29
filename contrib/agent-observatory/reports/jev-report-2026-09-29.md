# Jev Observatory — 2026-09-29 live projection audit

Generated: 2026-09-29T17:20:18Z · Host: Ryzen · Schema: 6.

**Execution status: BLOCKED (partial evidence delivered; M7 acceptance inputs and repository/role attribution remain missing).**

This is an aggregate-only rendering from the live `~/.local/share/agent-observatory/obs.db`. No transcript text, session IDs, command text, or credentials are included. No routing, dispatch, deployment, or model-call changes were made.

## Evidence and migration

- Event window in the final projection: `2026-08-25T15:43:48.656000Z` through `2026-09-29T16:20:32.226000Z` (924,962 events; 7,893 sessions). The later tail includes evidence collected during this execution.
- Verified pre-migration backup: `obs.db.backup-20260929T160750Z.bak`; SHA-256 `0c2c43c7b871884a20192bd9bd677fe0fe9333fbc0627d23042fe2da62881e53`; integrity check `ok`. The schema-5 migration also wrote its versioned pre-schema-6 backup.
- Schema 5 → 6 migration preserved the evidence row counts, bound 7,865/7,881 classifications (16 remain unbound), and installed 3 model-pricing seed rows. Session-role coverage remained 0/7,893.
- Bounded re-ingest used `--max-source-bytes 400000000`: the completed full pass read 5,948,960,273 source bytes, imported 6,048 sources, left 1,734 unchanged, found 308 missing and 1 source error, inserted 63 events, skipped 448,622 duplicates and 87,298 conflicting payload identities, and inserted 29,759 session fingerprints. A later incremental pass inserted 9 events and 136 duplicates. Final event delta: +72; final `commit_sha` fingerprints: 62,877.
- The resumable `backfill-usage` pass used `--max-source-bytes 400000000` and completed with status `ok`: 7,739/8,045 sources read, 629,542 candidate events, 0 matched, 0 usage rows inserted, 629,542 unmatched, 306 missing sources, 0 source errors, 0 deferred, and 0 unmapped. The backfill itself inserted 0 rows; final `event_usage` is 295,420 rows (+30 from newly ingested events versus the baseline), so missing counters were not synthesized.

## M5 — merged-change registry and exposure

- `changes-sync` imported 1,984 merged PR records through the pre-run evidence high-water mark `2026-09-29T15:59:27Z`: 37 `hoomji/gascity` PRs and 1,947 `Uniblock-dev/Gateway-LLM` PRs. It inserted 1,984 changes, 1,984 merge activations, and 4,489 commit-parent nodes.
- Screening retained the full denominator: 157 `optimization`, 1,827 `unknown`; no PR was silently dropped. The 157 optimization candidates are all in Gateway-LLM; Gas City’s 37 are `unknown`.
- Exposure report: 157 optimization interventions; exposed 0, unexposed 0, unknown 157; persisted session/change exposure rows: 0.
- All 924,962 event rows still have NULL `repo` and `commit_sha`; the re-ingest’s 87,298 immutable-payload conflicts explain why the existing import path did not enrich those event records. Although 62,877 commit-SHA fingerprints were captured, exposure’s repo-scope join has no repository-bound sessions, so the 157 interventions remain unknown. Do not read this as evidence of non-exposure.

## M6 — impact

- Attribution grade: **unmeasurable** (`no_work_items`); accepted work items 0, eligible 0, censored 0. There is no accepted-work/cohort link in this projection, so no effect or cost-per-accepted-task claim is measurable.

## M7 — shadow policy

- **Not run; no recommendations were written.** A production `--catalog` and an input bundle carrying session identity, as-of/observed timestamps, and current routing are both required. Only a synthetic test-fixture catalog is available; the live projection has no session roles and no repository-bound session evidence, so inferring a production route catalog/current route would be fabrication.
- Recommendations by kind/eligibility, disagreement rate, and `temporal_leak_free` share: **N/A** (not run); recommendation table remains empty. This is an acceptance blocker, not a zero-disagreement result.

## Token and USD aggregates by intent × role × model

For each token field the displayed value is the sum of non-NULL measurements followed by `(known events; missing events)`. Missing is distinct from zero. USD is shown as unavailable when the effective provider/model price is absent; no zero-cost inference is made.

| Intent | Role | Model | Events | Usage rows | Input tokens | Output tokens | Cache-read tokens | Cache-write tokens | Total tokens | USD (known only) |
|---|---|---|---:|---:|---:|---:|---:|---:|---:|---|
| adversarial_review | unknown | <synthetic> | 13 | 13 | 0 (13 known; 0 missing) | 0 (13 known; 0 missing) | 0 (13 known; 0 missing) | 0 (13 known; 0 missing) | 0 (13 known; 0 missing) | unpriced (13 unknown; 0 priced) |
| adversarial_review | unknown | claude-fable-5 | 175 | 85 | 170 (85 known; 90 missing) | 57736 (85 known; 90 missing) | 11802344 (85 known; 90 missing) | 175490 (85 known; 90 missing) | 12035740 (85 known; 90 missing) | unpriced (175 unknown; 0 priced) |
| adversarial_review | unknown | claude-opus-5 | 6,857 | 3,598 | 7196 (3598 known; 3259 missing) | 2550718 (3598 known; 3259 missing) | 470080211 (3598 known; 3259 missing) | 8386162 (3598 known; 3259 missing) | 481024287 (3598 known; 3259 missing) | unpriced (6857 unknown; 0 priced) |
| adversarial_review | unknown | claude-opus-5-5 | 197 | 108 | 216 (108 known; 89 missing) | 49729 (108 known; 89 missing) | 11354619 (108 known; 89 missing) | 126713 (108 known; 89 missing) | 11531277 (108 known; 89 missing) | unpriced (197 unknown; 0 priced) |
| adversarial_review | unknown | codex-auto-review | 24 | 12 | 345527 (12 known; 12 missing) | 1065 (12 known; 12 missing) | 265216 (12 known; 12 missing) | 0 (12 known; 12 missing) | 346592 (12 known; 12 missing) | unpriced (24 unknown; 0 priced) |
| adversarial_review | unknown | deepseek/deepseek-flash | 467 | 227 | 212342 (227 known; 240 missing) | 152158 (227 known; 240 missing) | 29419648 (227 known; 240 missing) | — (0 known; 467 missing) | 29784148 (227 known; 240 missing) | unpriced (467 unknown; 0 priced) |
| adversarial_review | unknown | gpt-5.6-luna | 1,877 | 644 | 79677800 (644 known; 1233 missing) | 540441 (644 known; 1233 missing) | 74414421 (644 known; 1233 missing) | 3815671 (644 known; 1233 missing) | 80546951 (644 known; 1233 missing) | unpriced (1877 unknown; 0 priced) |
| adversarial_review | unknown | gpt-5.6-sol | 2,146 | 721 | 47622789 (721 known; 1425 missing) | 248803 (721 known; 1425 missing) | 45382884 (721 known; 1425 missing) | 1070107 (721 known; 1425 missing) | 47871592 (721 known; 1425 missing) | unpriced (2146 unknown; 0 priced) |
| adversarial_review | unknown | gpt-6-sol | 167 | 55 | 4139379 (55 known; 112 missing) | 4283 (55 known; 112 missing) | 4061568 (55 known; 112 missing) | 0 (55 known; 112 missing) | 4143662 (55 known; 112 missing) | unpriced (167 unknown; 0 priced) |
| adversarial_review | unknown | unknown | 4,355 | 0 | — (0 known; 4355 missing) | — (0 known; 4355 missing) | — (0 known; 4355 missing) | — (0 known; 4355 missing) | — (0 known; 4355 missing) | unpriced (4355 unknown; 0 priced) |
| bugfix | unknown | <synthetic> | 164 | 164 | 0 (164 known; 0 missing) | 0 (164 known; 0 missing) | 0 (164 known; 0 missing) | 0 (164 known; 0 missing) | 0 (164 known; 0 missing) | unpriced (164 unknown; 0 priced) |
| bugfix | unknown | claude-fable-5 | 253 | 98 | 196 (98 known; 155 missing) | 76369 (98 known; 155 missing) | 9338592 (98 known; 155 missing) | 304087 (98 known; 155 missing) | 9719244 (98 known; 155 missing) | unpriced (253 unknown; 0 priced) |
| bugfix | unknown | claude-fable-5-1 | 572 | 304 | 10225 (304 known; 268 missing) | 226653 (304 known; 268 missing) | 35104291 (304 known; 268 missing) | 899219 (304 known; 268 missing) | 36240388 (304 known; 268 missing) | unpriced (572 unknown; 0 priced) |
| bugfix | unknown | claude-opus-5 | 41,378 | 22,820 | 45640 (22820 known; 18558 missing) | 13457972 (22820 known; 18558 missing) | 3514514347 (22820 known; 18558 missing) | 43731309 (22820 known; 18558 missing) | 3571749268 (22820 known; 18558 missing) | unpriced (41378 unknown; 0 priced) |
| bugfix | unknown | claude-opus-5-5 | 2,230 | 1,363 | 2726 (1363 known; 867 missing) | 551057 (1363 known; 867 missing) | 157783908 (1363 known; 867 missing) | 1746271 (1363 known; 867 missing) | 160083962 (1363 known; 867 missing) | unpriced (2230 unknown; 0 priced) |
| bugfix | unknown | claude-sonnet-5 | 3,830 | 2,095 | 4190 (2095 known; 1735 missing) | 962409 (2095 known; 1735 missing) | 273225100 (2095 known; 1735 missing) | 4098615 (2095 known; 1735 missing) | 278290314 (2095 known; 1735 missing) | unpriced (3830 unknown; 0 priced) |
| bugfix | unknown | codex-auto-review | 479 | 250 | 17795690 (250 known; 229 missing) | 21735 (250 known; 229 missing) | 16713216 (250 known; 229 missing) | 0 (250 known; 229 missing) | 17817425 (250 known; 229 missing) | unpriced (479 unknown; 0 priced) |
| bugfix | unknown | deepseek/deepseek-flash | 15,609 | 7,203 | 7017593 (7203 known; 8406 missing) | 5244416 (7203 known; 8406 missing) | 867563520 (7203 known; 8406 missing) | — (0 known; 15609 missing) | 879825529 (7203 known; 8406 missing) | unpriced (15609 unknown; 0 priced) |
| bugfix | unknown | gpt-5.5 | 284 | 63 | 6646313 (63 known; 221 missing) | 33832 (63 known; 221 missing) | 6420736 (63 known; 221 missing) | 0 (63 known; 221 missing) | 6689579 (63 known; 221 missing) | unpriced (284 unknown; 0 priced) |
| bugfix | unknown | gpt-5.6-luna | 76,926 | 24,919 | 3025332725 (24919 known; 52007 missing) | 8476931 (24919 known; 52007 missing) | 2941147453 (24919 known; 52007 missing) | 28008803 (24919 known; 52007 missing) | 3037256301 (24919 known; 52007 missing) | unpriced (76926 unknown; 0 priced) |
| bugfix | unknown | gpt-5.6-sol | 23,160 | 7,449 | 856850596 (7449 known; 15711 missing) | 1842433 (7449 known; 15711 missing) | 835461469 (7449 known; 15711 missing) | 14289040 (7449 known; 15711 missing) | 859298611 (7449 known; 15711 missing) | unpriced (23160 unknown; 0 priced) |
| bugfix | unknown | gpt-6-astra | 2,422 | 797 | 84287673 (797 known; 1625 missing) | 247002 (797 known; 1625 missing) | 81241088 (797 known; 1625 missing) | 0 (797 known; 1625 missing) | 84615022 (797 known; 1625 missing) | unpriced (2422 unknown; 0 priced) |
| bugfix | unknown | gpt-6-sol | 2,337 | 762 | 93739530 (762 known; 1575 missing) | 125200 (762 known; 1575 missing) | 92497664 (762 known; 1575 missing) | 0 (762 known; 1575 missing) | 93912308 (762 known; 1575 missing) | unpriced (2337 unknown; 0 priced) |
| bugfix | unknown | unknown | 39,736 | 0 | — (0 known; 39736 missing) | — (0 known; 39736 missing) | — (0 known; 39736 missing) | — (0 known; 39736 missing) | — (0 known; 39736 missing) | unpriced (39736 unknown; 0 priced) |
| dependency_worktree_agent_ops | unknown | <synthetic> | 214 | 214 | 0 (214 known; 0 missing) | 0 (214 known; 0 missing) | 0 (214 known; 0 missing) | 0 (214 known; 0 missing) | 0 (214 known; 0 missing) | unpriced (214 unknown; 0 priced) |
| dependency_worktree_agent_ops | unknown | claude-fable-5 | 1,310 | 692 | 1384 (692 known; 618 missing) | 312779 (692 known; 618 missing) | 120893059 (692 known; 618 missing) | 1757503 (692 known; 618 missing) | 122964725 (692 known; 618 missing) | unpriced (1310 unknown; 0 priced) |
| dependency_worktree_agent_ops | unknown | claude-fable-5-1 | 9,075 | 4,558 | 123752 (4558 known; 4517 missing) | 3644817 (4558 known; 4517 missing) | 557029706 (4558 known; 4517 missing) | 14252883 (4558 known; 4517 missing) | 575051158 (4558 known; 4517 missing) | unpriced (9075 unknown; 0 priced) |
| dependency_worktree_agent_ops | unknown | claude-opus-4-8 | 579 | 204 | 408 (204 known; 375 missing) | 258330 (204 known; 375 missing) | 48116620 (204 known; 375 missing) | 795664 (204 known; 375 missing) | 49171022 (204 known; 375 missing) | unpriced (579 unknown; 0 priced) |
| dependency_worktree_agent_ops | unknown | claude-opus-5 | 74,681 | 33,691 | 67382 (33691 known; 40990 missing) | 23352605 (33691 known; 40990 missing) | 4195479926 (33691 known; 40990 missing) | 75892175 (33691 known; 40990 missing) | 4294792088 (33691 known; 40990 missing) | unpriced (74681 unknown; 0 priced) |
| dependency_worktree_agent_ops | unknown | claude-opus-5-5 | 16,982 | 9,927 | 19890 (9927 known; 7055 missing) | 4073262 (9927 known; 7055 missing) | 1134036079 (9927 known; 7055 missing) | 14899509 (9927 known; 7055 missing) | 1153028740 (9927 known; 7055 missing) | unpriced (16982 unknown; 0 priced) |
| dependency_worktree_agent_ops | unknown | claude-sonnet-5 | 2,569 | 1,397 | 2794 (1397 known; 1172 missing) | 585333 (1397 known; 1172 missing) | 150465169 (1397 known; 1172 missing) | 4552582 (1397 known; 1172 missing) | 155605878 (1397 known; 1172 missing) | unpriced (2569 unknown; 0 priced) |
| dependency_worktree_agent_ops | unknown | codex-auto-review | 571 | 322 | 29685117 (322 known; 249 missing) | 28852 (322 known; 249 missing) | 28230656 (322 known; 249 missing) | 0 (322 known; 249 missing) | 29876304 (322 known; 249 missing) | unpriced (571 unknown; 0 priced) |
| dependency_worktree_agent_ops | unknown | deepseek/deepseek-flash | 18,045 | 8,248 | 8577035 (8248 known; 9797 missing) | 6596137 (8248 known; 9797 missing) | 872710656 (8248 known; 9797 missing) | — (0 known; 18045 missing) | 887883828 (8248 known; 9797 missing) | unpriced (18045 unknown; 0 priced) |
| dependency_worktree_agent_ops | unknown | gpt-5.5 | 489 | 84 | 8443749 (84 known; 405 missing) | 44095 (84 known; 405 missing) | 8087680 (84 known; 405 missing) | 0 (84 known; 405 missing) | 8509850 (84 known; 405 missing) | unpriced (489 unknown; 0 priced) |
| dependency_worktree_agent_ops | unknown | gpt-5.6-luna | 74,397 | 23,888 | 1847144977 (23888 known; 50509 missing) | 5630062 (23888 known; 50509 missing) | 1790381505 (23888 known; 50509 missing) | 3509483 (23888 known; 50509 missing) | 1853873653 (23888 known; 50509 missing) | unpriced (74397 unknown; 0 priced) |
| dependency_worktree_agent_ops | unknown | gpt-5.6-sol | 6,059 | 1,933 | 131893781 (1933 known; 4126 missing) | 385326 (1933 known; 4126 missing) | 127780224 (1933 known; 4126 missing) | 0 (1933 known; 4126 missing) | 132367616 (1933 known; 4126 missing) | unpriced (6059 unknown; 0 priced) |
| dependency_worktree_agent_ops | unknown | gpt-6-astra | 687 | 219 | 21975515 (219 known; 468 missing) | 47523 (219 known; 468 missing) | 21058688 (219 known; 468 missing) | 0 (219 known; 468 missing) | 22023038 (219 known; 468 missing) | unpriced (687 unknown; 0 priced) |
| dependency_worktree_agent_ops | unknown | gpt-6-sol | 684 | 221 | 28547050 (221 known; 463 missing) | 68695 (221 known; 463 missing) | 28168704 (221 known; 463 missing) | 0 (221 known; 463 missing) | 28615745 (221 known; 463 missing) | unpriced (684 unknown; 0 priced) |
| dependency_worktree_agent_ops | unknown | unknown | 72,438 | 1 | 0 (1 known; 72437 missing) | 0 (1 known; 72437 missing) | 0 (1 known; 72437 missing) | 0 (1 known; 72437 missing) | 83827 (1 known; 72437 missing) | unpriced (72438 unknown; 0 priced) |
| implementation | unknown | <synthetic> | 11 | 11 | 0 (11 known; 0 missing) | 0 (11 known; 0 missing) | 0 (11 known; 0 missing) | 0 (11 known; 0 missing) | 0 (11 known; 0 missing) | unpriced (11 unknown; 0 priced) |
| implementation | unknown | claude-fable-5-1 | 262 | 121 | 6362 (121 known; 141 missing) | 150485 (121 known; 141 missing) | 14653740 (121 known; 141 missing) | 484052 (121 known; 141 missing) | 15294639 (121 known; 141 missing) | unpriced (262 unknown; 0 priced) |
| implementation | unknown | claude-haiku-4-5-20251001 | 40 | 20 | 182 (20 known; 20 missing) | 3783 (20 known; 20 missing) | 734664 (20 known; 20 missing) | 35562 (20 known; 20 missing) | 774191 (20 known; 20 missing) | unpriced (40 unknown; 0 priced) |
| implementation | unknown | claude-opus-4-8 | 161 | 61 | 122 (61 known; 100 missing) | 67882 (61 known; 100 missing) | 5888825 (61 known; 100 missing) | 114454 (61 known; 100 missing) | 6071283 (61 known; 100 missing) | unpriced (161 unknown; 0 priced) |
| implementation | unknown | claude-opus-5 | 14,028 | 8,122 | 16244 (8122 known; 5906 missing) | 5049035 (8122 known; 5906 missing) | 1383687330 (8122 known; 5906 missing) | 15195643 (8122 known; 5906 missing) | 1403948252 (8122 known; 5906 missing) | unpriced (14028 unknown; 0 priced) |
| implementation | unknown | claude-opus-5-5 | 199 | 99 | 198 (99 known; 100 missing) | 84988 (99 known; 100 missing) | 10012961 (99 known; 100 missing) | 288285 (99 known; 100 missing) | 10386432 (99 known; 100 missing) | unpriced (199 unknown; 0 priced) |
| implementation | unknown | claude-sonnet-5 | 416 | 227 | 454 (227 known; 189 missing) | 64147 (227 known; 189 missing) | 25089330 (227 known; 189 missing) | 519081 (227 known; 189 missing) | 25673012 (227 known; 189 missing) | unpriced (416 unknown; 0 priced) |
| implementation | unknown | codex-auto-review | 3,326 | 1,749 | 133038261 (1749 known; 1577 missing) | 137208 (1749 known; 1577 missing) | 127808256 (1749 known; 1577 missing) | 0 (1749 known; 1577 missing) | 133175469 (1749 known; 1577 missing) | unpriced (3326 unknown; 0 priced) |
| implementation | unknown | deepseek/deepseek-flash | 9,426 | 4,392 | 4405772 (4392 known; 5034 missing) | 3105537 (4392 known; 5034 missing) | 665958656 (4392 known; 5034 missing) | — (0 known; 9426 missing) | 673469965 (4392 known; 5034 missing) | unpriced (9426 unknown; 0 priced) |
| implementation | unknown | gpt-5.5 | 588 | 104 | 13665088 (104 known; 484 missing) | 45121 (104 known; 484 missing) | 13227648 (104 known; 484 missing) | 0 (104 known; 484 missing) | 13732653 (104 known; 484 missing) | unpriced (588 unknown; 0 priced) |
| implementation | unknown | gpt-5.6-luna | 22,216 | 7,702 | 993575548 (7702 known; 14514 missing) | 2666729 (7702 known; 14514 missing) | 932165592 (7702 known; 14514 missing) | 55633994 (7702 known; 14514 missing) | 998041637 (7702 known; 14514 missing) | unpriced (22216 unknown; 0 priced) |
| implementation | unknown | gpt-5.6-sol | 2,306 | 742 | 76130441 (742 known; 1564 missing) | 188788 (742 known; 1564 missing) | 74214364 (742 known; 1564 missing) | 590495 (742 known; 1564 missing) | 76361114 (742 known; 1564 missing) | unpriced (2306 unknown; 0 priced) |
| implementation | unknown | gpt-6-astra | 2,162 | 709 | 78254081 (709 known; 1453 missing) | 207634 (709 known; 1453 missing) | 76522112 (709 known; 1453 missing) | 0 (709 known; 1453 missing) | 78488456 (709 known; 1453 missing) | unpriced (2162 unknown; 0 priced) |
| implementation | unknown | gpt-6-luna | 107 | 36 | 1773295 (36 known; 71 missing) | 10191 (36 known; 71 missing) | 1657856 (36 known; 71 missing) | 0 (36 known; 71 missing) | 1783486 (36 known; 71 missing) | unpriced (107 unknown; 0 priced) |
| implementation | unknown | gpt-6-sol | 4,967 | 1,652 | 213742011 (1652 known; 3315 missing) | 387749 (1652 known; 3315 missing) | 211177856 (1652 known; 3315 missing) | 0 (1652 known; 3315 missing) | 214275576 (1652 known; 3315 missing) | unpriced (4967 unknown; 0 priced) |
| implementation | unknown | unknown | 16,419 | 1 | 0 (1 known; 16418 missing) | 0 (1 known; 16418 missing) | 0 (1 known; 16418 missing) | 0 (1 known; 16418 missing) | 69842 (1 known; 16418 missing) | unpriced (16419 unknown; 0 priced) |
| planning_spec | unknown | claude-fable-5 | 240 | 104 | 208 (104 known; 136 missing) | 103148 (104 known; 136 missing) | 13026416 (104 known; 136 missing) | 461890 (104 known; 136 missing) | 13591662 (104 known; 136 missing) | unpriced (240 unknown; 0 priced) |
| planning_spec | unknown | claude-fable-5-1 | 251 | 123 | 3398 (123 known; 128 missing) | 143481 (123 known; 128 missing) | 14033239 (123 known; 128 missing) | 522263 (123 known; 128 missing) | 14702381 (123 known; 128 missing) | unpriced (251 unknown; 0 priced) |
| planning_spec | unknown | claude-opus-5 | 1,445 | 740 | 1480 (740 known; 705 missing) | 515157 (740 known; 705 missing) | 77671750 (740 known; 705 missing) | 1821817 (740 known; 705 missing) | 80010204 (740 known; 705 missing) | unpriced (1445 unknown; 0 priced) |
| planning_spec | unknown | claude-opus-5-5 | 155 | 80 | 162 (80 known; 75 missing) | 59301 (80 known; 75 missing) | 9814032 (80 known; 75 missing) | 205621 (80 known; 75 missing) | 10079116 (80 known; 75 missing) | unpriced (155 unknown; 0 priced) |
| planning_spec | unknown | gpt-5.6-sol | 1,315 | 426 | 57811669 (426 known; 889 missing) | 110646 (426 known; 889 missing) | 56728320 (426 known; 889 missing) | 0 (426 known; 889 missing) | 57980505 (426 known; 889 missing) | unpriced (1315 unknown; 0 priced) |
| planning_spec | unknown | gpt-6-astra | 179 | 57 | 6678790 (57 known; 122 missing) | 34143 (57 known; 122 missing) | 6519424 (57 known; 122 missing) | 0 (57 known; 122 missing) | 6712933 (57 known; 122 missing) | unpriced (179 unknown; 0 priced) |
| planning_spec | unknown | unknown | 1,348 | 0 | — (0 known; 1348 missing) | — (0 known; 1348 missing) | — (0 known; 1348 missing) | — (0 known; 1348 missing) | — (0 known; 1348 missing) | unpriced (1348 unknown; 0 priced) |
| pr_review | unknown | <synthetic> | 90 | 90 | 0 (90 known; 0 missing) | 0 (90 known; 0 missing) | 0 (90 known; 0 missing) | 0 (90 known; 0 missing) | 0 (90 known; 0 missing) | unpriced (90 unknown; 0 priced) |
| pr_review | unknown | claude-fable-5 | 1,198 | 602 | 1204 (602 known; 596 missing) | 361252 (602 known; 596 missing) | 155898408 (602 known; 596 missing) | 1276569 (602 known; 596 missing) | 157537433 (602 known; 596 missing) | unpriced (1198 unknown; 0 priced) |
| pr_review | unknown | claude-fable-5-1 | 1,001 | 516 | 14750 (516 known; 485 missing) | 420881 (516 known; 485 missing) | 65325943 (516 known; 485 missing) | 1325332 (516 known; 485 missing) | 67086906 (516 known; 485 missing) | unpriced (1001 unknown; 0 priced) |
| pr_review | unknown | claude-opus-4-8 | 319 | 98 | 196 (98 known; 221 missing) | 185602 (98 known; 221 missing) | 20891955 (98 known; 221 missing) | 303268 (98 known; 221 missing) | 21381021 (98 known; 221 missing) | unpriced (319 unknown; 0 priced) |
| pr_review | unknown | claude-opus-5 | 60,480 | 31,379 | 62758 (31379 known; 29101 missing) | 21059058 (31379 known; 29101 missing) | 3966179976 (31379 known; 29101 missing) | 68053797 (31379 known; 29101 missing) | 4055355589 (31379 known; 29101 missing) | unpriced (60480 unknown; 0 priced) |
| pr_review | unknown | claude-opus-5-5 | 4,513 | 2,678 | 5356 (2678 known; 1835 missing) | 1145150 (2678 known; 1835 missing) | 301138572 (2678 known; 1835 missing) | 3804314 (2678 known; 1835 missing) | 306093392 (2678 known; 1835 missing) | unpriced (4513 unknown; 0 priced) |
| pr_review | unknown | claude-sonnet-5 | 1,308 | 697 | 1394 (697 known; 611 missing) | 352476 (697 known; 611 missing) | 67053690 (697 known; 611 missing) | 1745895 (697 known; 611 missing) | 69153455 (697 known; 611 missing) | unpriced (1308 unknown; 0 priced) |
| pr_review | unknown | codex-auto-review | 141 | 74 | 5402285 (74 known; 67 missing) | 6867 (74 known; 67 missing) | 5079552 (74 known; 67 missing) | 0 (74 known; 67 missing) | 5409152 (74 known; 67 missing) | unpriced (141 unknown; 0 priced) |
| pr_review | unknown | deepseek/deepseek-flash | 8,973 | 4,128 | 4224174 (4128 known; 4845 missing) | 3030339 (4128 known; 4845 missing) | 445270784 (4128 known; 4845 missing) | — (0 known; 8973 missing) | 452525297 (4128 known; 4845 missing) | unpriced (8973 unknown; 0 priced) |
| pr_review | unknown | gpt-5.5 | 123 | 24 | 925704 (24 known; 99 missing) | 12621 (24 known; 99 missing) | 782336 (24 known; 99 missing) | 0 (24 known; 99 missing) | 938325 (24 known; 99 missing) | unpriced (123 unknown; 0 priced) |
| pr_review | unknown | gpt-5.6-luna | 39,356 | 12,768 | 1390471704 (12768 known; 26588 missing) | 4806534 (12768 known; 26588 missing) | 1344936365 (12768 known; 26588 missing) | 3338038 (12768 known; 26588 missing) | 1397146045 (12768 known; 26588 missing) | unpriced (39356 unknown; 0 priced) |
| pr_review | unknown | gpt-5.6-sol | 6,468 | 2,114 | 156080021 (2114 known; 4354 missing) | 597831 (2114 known; 4354 missing) | 148264320 (2114 known; 4354 missing) | 2064578 (2114 known; 4354 missing) | 156720266 (2114 known; 4354 missing) | unpriced (6468 unknown; 0 priced) |
| pr_review | unknown | gpt-6-astra | 664 | 210 | 20181061 (210 known; 454 missing) | 54601 (210 known; 454 missing) | 19592064 (210 known; 454 missing) | 0 (210 known; 454 missing) | 20235662 (210 known; 454 missing) | unpriced (664 unknown; 0 priced) |
| pr_review | unknown | gpt-6-sol | 364 | 124 | 7994429 (124 known; 240 missing) | 22328 (124 known; 240 missing) | 7543168 (124 known; 240 missing) | 0 (124 known; 240 missing) | 8016757 (124 known; 240 missing) | unpriced (364 unknown; 0 priced) |
| pr_review | unknown | unknown | 44,944 | 1 | 0 (1 known; 44943 missing) | 0 (1 known; 44943 missing) | 0 (1 known; 44943 missing) | 0 (1 known; 44943 missing) | 71177 (1 known; 44943 missing) | unpriced (44944 unknown; 0 priced) |
| research_docs | unknown | <synthetic> | 12 | 12 | 0 (12 known; 0 missing) | 0 (12 known; 0 missing) | 0 (12 known; 0 missing) | 0 (12 known; 0 missing) | 0 (12 known; 0 missing) | unpriced (12 unknown; 0 priced) |
| research_docs | unknown | claude-fable-5 | 240 | 108 | 216 (108 known; 132 missing) | 96652 (108 known; 132 missing) | 19426928 (108 known; 132 missing) | 411460 (108 known; 132 missing) | 19935256 (108 known; 132 missing) | unpriced (240 unknown; 0 priced) |
| research_docs | unknown | claude-fable-5-1 | 1,098 | 477 | 21162 (477 known; 621 missing) | 566370 (477 known; 621 missing) | 76092479 (477 known; 621 missing) | 2093111 (477 known; 621 missing) | 78773122 (477 known; 621 missing) | unpriced (1098 unknown; 0 priced) |
| research_docs | unknown | claude-opus-5 | 10,012 | 4,896 | 9792 (4896 known; 5116 missing) | 3335123 (4896 known; 5116 missing) | 605570139 (4896 known; 5116 missing) | 10268527 (4896 known; 5116 missing) | 619183581 (4896 known; 5116 missing) | unpriced (10012 unknown; 0 priced) |
| research_docs | unknown | claude-opus-5-5 | 1,322 | 704 | 1410 (704 known; 618 missing) | 379179 (704 known; 618 missing) | 81898647 (704 known; 618 missing) | 1443421 (704 known; 618 missing) | 83722657 (704 known; 618 missing) | unpriced (1322 unknown; 0 priced) |
| research_docs | unknown | claude-sonnet-5 | 460 | 234 | 468 (234 known; 226 missing) | 113026 (234 known; 226 missing) | 19225925 (234 known; 226 missing) | 1274960 (234 known; 226 missing) | 20614379 (234 known; 226 missing) | unpriced (460 unknown; 0 priced) |
| research_docs | unknown | codex-auto-review | 1,341 | 692 | 44652098 (692 known; 649 missing) | 58605 (692 known; 649 missing) | 42318848 (692 known; 649 missing) | 0 (692 known; 649 missing) | 44710703 (692 known; 649 missing) | unpriced (1341 unknown; 0 priced) |
| research_docs | unknown | deepseek/deepseek-flash | 15,044 | 7,030 | 7188411 (7030 known; 8014 missing) | 4330963 (7030 known; 8014 missing) | 1103669504 (7030 known; 8014 missing) | — (0 known; 15044 missing) | 1115188878 (7030 known; 8014 missing) | unpriced (15044 unknown; 0 priced) |
| research_docs | unknown | gpt-5.1 | 3 | 0 | — (0 known; 3 missing) | — (0 known; 3 missing) | — (0 known; 3 missing) | — (0 known; 3 missing) | — (0 known; 3 missing) | unpriced (3 unknown; 0 priced) |
| research_docs | unknown | gpt-5.6-luna | 3,756 | 1,281 | 149912912 (1281 known; 2475 missing) | 584699 (1281 known; 2475 missing) | 145028047 (1281 known; 2475 missing) | 3053590 (1281 known; 2475 missing) | 150582626 (1281 known; 2475 missing) | unpriced (3756 unknown; 0 priced) |
| research_docs | unknown | gpt-5.6-sol | 822 | 267 | 31418811 (267 known; 555 missing) | 65367 (267 known; 555 missing) | 30436352 (267 known; 555 missing) | 0 (267 known; 555 missing) | 31507530 (267 known; 555 missing) | unpriced (822 unknown; 0 priced) |
| research_docs | unknown | gpt-6-astra | 974 | 306 | 36836578 (306 known; 668 missing) | 127632 (306 known; 668 missing) | 35932288 (306 known; 668 missing) | 0 (306 known; 668 missing) | 36981905 (306 known; 668 missing) | unpriced (974 unknown; 0 priced) |
| research_docs | unknown | gpt-6-luna | 78 | 25 | 955723 (25 known; 53 missing) | 3380 (25 known; 53 missing) | 821504 (25 known; 53 missing) | 0 (25 known; 53 missing) | 959103 (25 known; 53 missing) | unpriced (78 unknown; 0 priced) |
| research_docs | unknown | gpt-6-sol | 4,055 | 1,317 | 151049288 (1317 known; 2738 missing) | 305672 (1317 known; 2738 missing) | 148190464 (1317 known; 2738 missing) | 0 (1317 known; 2738 missing) | 151449593 (1317 known; 2738 missing) | unpriced (4055 unknown; 0 priced) |
| research_docs | unknown | unknown | 16,713 | 0 | — (0 known; 16713 missing) | — (0 known; 16713 missing) | — (0 known; 16713 missing) | — (0 known; 16713 missing) | — (0 known; 16713 missing) | unpriced (16713 unknown; 0 priced) |
| test_lint_build_ci | unknown | <synthetic> | 10 | 10 | 0 (10 known; 0 missing) | 0 (10 known; 0 missing) | 0 (10 known; 0 missing) | 0 (10 known; 0 missing) | 0 (10 known; 0 missing) | unpriced (10 unknown; 0 priced) |
| test_lint_build_ci | unknown | claude-fable-5-1 | 247 | 122 | 3574 (122 known; 125 missing) | 111689 (122 known; 125 missing) | 14972012 (122 known; 125 missing) | 331020 (122 known; 125 missing) | 15418295 (122 known; 125 missing) | unpriced (247 unknown; 0 priced) |
| test_lint_build_ci | unknown | claude-opus-4-8 | 183 | 75 | 150 (75 known; 108 missing) | 65084 (75 known; 108 missing) | 8214492 (75 known; 108 missing) | 127595 (75 known; 108 missing) | 8407321 (75 known; 108 missing) | unpriced (183 unknown; 0 priced) |
| test_lint_build_ci | unknown | claude-opus-5 | 12,196 | 6,614 | 13228 (6614 known; 5582 missing) | 3262875 (6614 known; 5582 missing) | 731073364 (6614 known; 5582 missing) | 12471272 (6614 known; 5582 missing) | 746820739 (6614 known; 5582 missing) | unpriced (12196 unknown; 0 priced) |
| test_lint_build_ci | unknown | claude-opus-5-5 | 227 | 132 | 264 (132 known; 95 missing) | 47711 (132 known; 95 missing) | 15625080 (132 known; 95 missing) | 156311 (132 known; 95 missing) | 15829366 (132 known; 95 missing) | unpriced (227 unknown; 0 priced) |
| test_lint_build_ci | unknown | claude-sonnet-5 | 835 | 459 | 918 (459 known; 376 missing) | 201215 (459 known; 376 missing) | 60155171 (459 known; 376 missing) | 1040105 (459 known; 376 missing) | 61397409 (459 known; 376 missing) | unpriced (835 unknown; 0 priced) |
| test_lint_build_ci | unknown | codex-auto-review | 518 | 274 | 13505877 (274 known; 244 missing) | 24660 (274 known; 244 missing) | 12692992 (274 known; 244 missing) | 0 (274 known; 244 missing) | 13530537 (274 known; 244 missing) | unpriced (518 unknown; 0 priced) |
| test_lint_build_ci | unknown | deepseek/deepseek-flash | 6,153 | 2,863 | 2737145 (2863 known; 3290 missing) | 2053210 (2863 known; 3290 missing) | 267224960 (2863 known; 3290 missing) | — (0 known; 6153 missing) | 272015315 (2863 known; 3290 missing) | unpriced (6153 unknown; 0 priced) |
| test_lint_build_ci | unknown | gpt-5.5 | 229 | 44 | 3491351 (44 known; 185 missing) | 21868 (44 known; 185 missing) | 3333632 (44 known; 185 missing) | 0 (44 known; 185 missing) | 3513219 (44 known; 185 missing) | unpriced (229 unknown; 0 priced) |
| test_lint_build_ci | unknown | gpt-5.6-luna | 19,291 | 6,201 | 691807332 (6201 known; 13090 missing) | 1761783 (6201 known; 13090 missing) | 673974674 (6201 known; 13090 missing) | 209825 (6201 known; 13090 missing) | 694286403 (6201 known; 13090 missing) | unpriced (19291 unknown; 0 priced) |
| test_lint_build_ci | unknown | gpt-5.6-sol | 1,059 | 339 | 29384970 (339 known; 720 missing) | 74104 (339 known; 720 missing) | 28485737 (339 known; 720 missing) | 158496 (339 known; 720 missing) | 29459074 (339 known; 720 missing) | unpriced (1059 unknown; 0 priced) |
| test_lint_build_ci | unknown | gpt-6-astra | 177 | 57 | 5603096 (57 known; 120 missing) | 17340 (57 known; 120 missing) | 5464320 (57 known; 120 missing) | 0 (57 known; 120 missing) | 5620436 (57 known; 120 missing) | unpriced (177 unknown; 0 priced) |
| test_lint_build_ci | unknown | gpt-6-sol | 1,357 | 437 | 61365690 (437 known; 920 missing) | 92884 (437 known; 920 missing) | 60746368 (437 known; 920 missing) | 0 (437 known; 920 missing) | 61458574 (437 known; 920 missing) | unpriced (1357 unknown; 0 priced) |
| test_lint_build_ci | unknown | unknown | 11,772 | 0 | — (0 known; 11772 missing) | — (0 known; 11772 missing) | — (0 known; 11772 missing) | — (0 known; 11772 missing) | — (0 known; 11772 missing) | unpriced (11772 unknown; 0 priced) |
| unknown | unknown | <synthetic> | 52 | 52 | 0 (52 known; 0 missing) | 0 (52 known; 0 missing) | 0 (52 known; 0 missing) | 0 (52 known; 0 missing) | 0 (52 known; 0 missing) | unpriced (52 unknown; 0 priced) |
| unknown | unknown | claude-fable-5 | 599 | 332 | 664 (332 known; 267 missing) | 140784 (332 known; 267 missing) | 65498176 (332 known; 267 missing) | 448847 (332 known; 267 missing) | 66088471 (332 known; 267 missing) | unpriced (599 unknown; 0 priced) |
| unknown | unknown | claude-opus-4-8 | 999 | 383 | 766 (383 known; 616 missing) | 534960 (383 known; 616 missing) | 89107905 (383 known; 616 missing) | 1111097 (383 known; 616 missing) | 90754728 (383 known; 616 missing) | unpriced (999 unknown; 0 priced) |
| unknown | unknown | claude-opus-5 | 8,877 | 4,166 | 8332 (4166 known; 4711 missing) | 2972431 (4166 known; 4711 missing) | 522016156 (4166 known; 4711 missing) | 12671197 (4166 known; 4711 missing) | 537668116 (4166 known; 4711 missing) | unpriced (8877 unknown; 0 priced) |
| unknown | unknown | claude-opus-5-5 | 2,837 | 1,692 | 3386 (1692 known; 1145 missing) | 726895 (1692 known; 1145 missing) | 211103609 (1692 known; 1145 missing) | 2131174 (1692 known; 1145 missing) | 213965064 (1692 known; 1145 missing) | unpriced (2837 unknown; 0 priced) |
| unknown | unknown | claude-sonnet-5 | 215 | 95 | 190 (95 known; 120 missing) | 20594 (95 known; 120 missing) | 5880406 (95 known; 120 missing) | 639108 (95 known; 120 missing) | 6540298 (95 known; 120 missing) | unpriced (215 unknown; 0 priced) |
| unknown | unknown | codex-auto-review | 1,032 | 656 | 39037993 (656 known; 376 missing) | 52671 (656 known; 376 missing) | 37085184 (656 known; 376 missing) | 0 (656 known; 376 missing) | 39090664 (656 known; 376 missing) | unpriced (1032 unknown; 0 priced) |
| unknown | unknown | deepseek/deepseek-flash | 9,600 | 6,030 | 1805503 (6030 known; 3570 missing) | 5624122 (6030 known; 3570 missing) | 136882551 (6030 known; 3570 missing) | — (0 known; 9600 missing) | 144312176 (6030 known; 3570 missing) | unpriced (9600 unknown; 0 priced) |
| unknown | unknown | gpt-5.1 | 1 | 0 | — (0 known; 1 missing) | — (0 known; 1 missing) | — (0 known; 1 missing) | — (0 known; 1 missing) | — (0 known; 1 missing) | unpriced (1 unknown; 0 priced) |
| unknown | unknown | gpt-5.5 | 4 | 2 | 45628 (2 known; 2 missing) | 3476 (2 known; 2 missing) | 8960 (2 known; 2 missing) | 0 (2 known; 2 missing) | 49104 (2 known; 2 missing) | unpriced (4 unknown; 0 priced) |
| unknown | unknown | gpt-5.6-luna | 8,572 | 2,725 | 140889142 (2725 known; 5847 missing) | 556113 (2725 known; 5847 missing) | 133209132 (2725 known; 5847 missing) | 184772 (2725 known; 5847 missing) | 141469136 (2725 known; 5847 missing) | unpriced (8572 unknown; 0 priced) |
| unknown | unknown | gpt-5.6-sol | 4,911 | 1,973 | 217650871 (1973 known; 2938 missing) | 200024 (1973 known; 2938 missing) | 211277473 (1973 known; 2938 missing) | 4486390 (1973 known; 2938 missing) | 218264790 (1973 known; 2938 missing) | unpriced (4911 unknown; 0 priced) |
| unknown | unknown | gpt-5.6-terra | 2 | 1 | 13927 (1 known; 1 missing) | 7 (1 known; 1 missing) | 0 (1 known; 1 missing) | 13924 (1 known; 1 missing) | 13934 (1 known; 1 missing) | unpriced (2 unknown; 0 priced) |
| unknown | unknown | gpt-6-astra | 254 | 80 | 6934627 (80 known; 174 missing) | 23107 (80 known; 174 missing) | 6669440 (80 known; 174 missing) | 0 (80 known; 174 missing) | 6957734 (80 known; 174 missing) | unpriced (254 unknown; 0 priced) |
| unknown | unknown | gpt-6-sol | 384 | 125 | 16022756 (125 known; 259 missing) | 48430 (125 known; 259 missing) | 15697280 (125 known; 259 missing) | 0 (125 known; 259 missing) | 16097120 (125 known; 259 missing) | unpriced (384 unknown; 0 priced) |
| unknown | unknown | unknown | 20,492 | 2 | 0 (2 known; 20490 missing) | 0 (2 known; 20490 missing) | 0 (2 known; 20490 missing) | 0 (2 known; 20490 missing) | 140776 (2 known; 20490 missing) | unpriced (20492 unknown; 0 priced) |

Total: 924,962 events; 295,420 events with usage rows; 0 priced events; 924,962 events with unknown cost.
Known token sums: input 11117064401 (295,420 known; 629,542 missing), output 153694156 (295,420 known; 629,542 missing), cache-read 34453790670 (295,420 known; 629,542 missing), cache-write 434822436 (255,299 known; 669,663 missing), total 35331701089 (295,420 known; 629,542 missing).
USD coverage: 0 priced events; all event costs remain unknown under the checked-in 3-row seed. The numeric known-USD sum is not presented as `$0` because no event has a known price.

## Table row counts (before → after)

The before snapshot is the verified schema-5 backup; after is the schema-6 live projection after M5/M6 and the bounded collection/backfill. New schema-6 tables have a before count of zero.

| Table | Before | After | Delta |
|---|---:|---:|---:|
| `change_activations` | 0 | 1,984 | +1,984 |
| `changes` | 0 | 1,984 | +1,984 |
| `classification_answers` | 55,167 | 55,167 | +0 |
| `classification_sessions` | 0 | 7,865 | +7,865 |
| `classifications` | 7,881 | 7,881 | +0 |
| `collector_queue` | 8,099 | 8,101 | +2 |
| `collector_runs` | 257 | 271 | +14 |
| `collector_sources` | 8,091 | 8,091 | +0 |
| `commit_parents` | 0 | 4,489 | +4,489 |
| `event_usage` | 295,390 | 295,420 | +30 |
| `events` | 924,890 | 924,962 | +72 |
| `exposures` | 0 | 0 | +0 |
| `gold_annotations` | 0 | 0 | +0 |
| `imported_files` | 8,256 | 15,633 | +7,377 |
| `jev_requests` | 7,884 | 7,884 | +0 |
| `model_pricing` | 0 | 3 | +3 |
| `recommendations` | 0 | 0 | +0 |
| `schema_meta` | 1 | 1 | +0 |
| `session_fingerprints` | 0 | 62,877 | +62,877 |
| `sessions` | 7,893 | 7,893 | +0 |
| `transport_circuit_state` | 1 | 1 | +0 |
| `transport_provenance` | 8,161 | 8,161 | +0 |

## Remaining acceptance gates

1. Provide a supported, immutable-safe backfill for event-level `repo`/`commit_sha` and a trusted session-to-role mapping; do not rewrite payload hashes or infer roles from message-role text.
2. Supply the approved M7 candidate catalog plus a time-stamped current-routing shadow bundle, then rerun shadow and report kind/eligibility counts, disagreement, and leak-free share.
3. Re-run M5 exposure after repository evidence is safely bound; until then unknown exposure is the only supported conclusion.
