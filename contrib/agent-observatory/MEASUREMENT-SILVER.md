# Measurement note: the machine-built silver reference

Owner decision 2026-09-25 replaced human gold annotation with a **silver**
reference for the `primary_intent` gate. This note states the method and, more
importantly, what the resulting numbers do and do not mean.

## Why silver

The live `obs.db` projection had `primary_intent=unknown` for all 53 live Jev
classifications, so the M7 shadow policy had zero eligible rows and the M8
canary could not run. The cause was the classification **data scope**, not the
taxonomy: every live request carried `state_kind=session_metadata` (event/tool
counts only), and a metadata-only state cannot express a task's intent. Jev
answered `unknown` consistently because the rubric says to choose `unknown` when
the evidence does not support exactly one category.

The fix is the owner-approved transcript-text scope with Gas City framework
payloads stripped (`framework.py`), plus `queue-drain` defaulting to that scope.
Re-classifying the same 53 sessions in the text scope produced non-`unknown`
`primary_intent` labels (see the execution report for the measured counts).

## Method

`python3 -m agent_observatory silver-build`:

1. **Sample.** Candidate episodes come from a candidate CSV
   (`episode_id`, `group_key`, `provider`, `observed_at`). Sampling is
   deterministic for a seed and proportional across providers by largest
   remainder. A candidate is skipped and reported when its session is absent
   from the projection or has no task text after framework stripping.
2. **Strip.** Text is read through the collector's text state, which drops
   `[city] <agent> • <time>` role prompts, runtime-context snapshots,
   `<system-reminder>` wake/deferred reminders and skill payloads. The filter is
   versioned (`FRAMEWORK_FILTER_VERSION`) and recorded on every state.
3. **Judge.** A configurable set of independent judges labels `primary_intent`
   from the **same stripped text** with the **same taxonomy criteria**, at
   temperature 0 with JSON output. The default set is two gateway judges:
   - GLM 5.3 Flash — `uniblock-prod/fireworks-ai/glm-5p3-flash`
   - DeepSeek V4 Flash — `uniblock-prod/deepseek/deepseek-flash`

   `--judge` / `--judges-file` add or replace judges. Each judge is bound to a
   pluggable backend: `gateway` (the OpenAI-compatible Uniblock prod
   `/chat/completions` path above) or a subscription CLI — `codex-cli`
   (`codex exec -m gpt-6-luna`, read-only sandbox, ephemeral, JSON-schema
   constrained) for GPT 6 Luna, and `agy-cli`
   (`agy --model gemini-3.8-flash-medium --json-schema`) for Gemini 3.8 Flash.
   Subscription judges never touch a paid API key. Exact model slugs come from
   each CLI's own `--version`/model list, and the CLI version plus model is
   recorded with every label. The fixed prompt is versioned
   (`JUDGE_PROMPT_VERSION`). The gateway bearer key is read from an environment
   variable at call time and is never printed or committed; a stable user agent
   is required (the gateway rejects the default `Python-urllib` signature with
   HTTP 403, `error code: 1010`).
4. **Adjudicate.** Judges that all agree become an `adjudicated` silver label.
   Any disagreement becomes `disagreement`, is excluded from the primary score
   and is reported. Per-judge labels, confidences, model/backend ids, CLI
   version, prompt version, text hash and the `gold_set_hash` are recorded.
   Silver is kept strictly separate from Jev predictions
   (`annotator="silver-judges"`).
5. **Report.** `silver-build` reports every pairwise Cohen's kappa, Fleiss'
   kappa across the judge set, and each judge's label distribution and
   `unknown` rate. `silver-evaluate` reports Jev accuracy/macro-F1 against each
   judge alone, the majority label and the unanimous label. The trust verdict
   uses the **minimum** pairwise kappa, so every pair must clear the floor.
6. **Enforce.** The kappa floor and the minimum sample size are enforced, not
   merely reported. `silver-build` refuses to write `--out-gold` when kappa is
   below `0.6` (`KAPPA_TRUST_FLOOR`) or when the sample has fewer than
   `MIN_SILVER_SAMPLE_SIZE = 4` episodes, and exits nonzero; the agreement report
   is still written as evidence. A kappa over a handful of episodes is
   degenerate (one agreed episode yields kappa `1.0` by construction), so the
   sample floor is checked independently of kappa. `load_gold_set` (and therefore
   `silver-evaluate`) refuses a silver set marked `silver_trustworthy: false`,
   and `silver-evaluate` also refuses to claim a gate whose recomputed kappa is
   below the floor or whose sample is below the floor. Both refusals are lifted
   only by the explicit `--allow-untrusted` flag. The gold set records the marker
   top-level as `"silver": true` / `"silver_trustworthy": <bool>`.

Budget: the judge-call ceiling is `2 x 150` (two judges over 150 episodes); the
tool refuses to exceed the configured `--max-judge-requests`.

## What the gate does and does not establish

- **It is an agreement measure, not ground truth.** A silver label is the label
  two LLMs produced, not the label a careful human would produce.
- **A Jev error shared by both judges is invisible.** If both judges and Jev
  make the same mistake, the silver accuracy is 1.0 for a wrong label.
- **Judge bias is shared.** Two models from the same gateway and prompt style
  may share systematic biases (for example, treating any tool-heavy session as
  `dependency_worktree_agent_ops`).
- **Kappa gates trust and is enforced; a minimum sample is required.** If
  Cohen's kappa is below `0.6`, the judges disagree too much for the silver set
  to be a trustworthy reference. Independently, a sample with fewer than
  `MIN_SILVER_SAMPLE_SIZE = 4` episodes is untrusted regardless of kappa because
  kappa over so few items is degenerate. The report says so, `silver-build`
  refuses to write the gold set and exits nonzero, and the loader/evaluator
  refuse to consume an untrusted silver file unless the caller passes
  `--allow-untrusted`. An untrusted set is never treated as ground truth by
  accident.
- **The comparison is descriptive.** These numbers describe classifier
  agreement. They do not establish orchestration benefit or causality.
- **Coverage is partial.** Candidates whose sessions are missing from the
  projection, or whose text is entirely injected framework payload, are skipped
  and reported rather than fabricated.

Any M7/M8 decision that cites these numbers must also cite the limitation that
the reference is machine agreement, and must keep `unknown`/low-confidence/rare
cases out of automatic routing as the evaluator's automation gate already does.

## Measured outcome, 2026-09-25

The first real run used 150 episodes sampled from the 262 candidates whose
transcripts are in the live projection (80 `claude-mayor`, 70
`dsh-minimal-deepseek-flash`; the 120 `dsh-minimal-glm-5.3-flash` candidates have
no transcript text in the projection — their collector root `~/.dsh-glm/sessions`
was never collected — so they were skipped and reported, not fabricated).

- Judge calls: 300 (150 per judge), 0 parse failures.
- Agreement: 112 `adjudicated`, 38 `disagreement` (agreement rate 0.747).
- **Cohen's kappa on the 150-episode run: 0.406**, below the 0.6 trust floor → **the silver set is not
  trustworthy and the gate was not claimed.** With the enforced gate,
  `silver-build` refuses to write this gold set and exits nonzero; reproducing
  the measured artifact requires the explicit `--allow-untrusted` opt-in, and
  any pre-enforcement file is treated as untrusted on load.
- Jev vs silver on the 110 agreed items that also had a Jev prediction:
  accuracy 0.636, macro-F1 0.511, 77/110 (70.0%) non-`unknown`; 2 agreed
  episodes had no stored Jev classification.
- The agreed labels were heavily skewed to `dependency_worktree_agent_ops`
  (97 of 112), which is consistent with the shared-judge-bias limitation above:
  tool-heavy agent sessions look alike to both judges even after framework
  payloads are stripped.

The root-cause re-run is reported separately in the execution report: the same
53 sessions that were `primary_intent=unknown` under the metadata scope produced
43 non-`unknown` labels under the stripped-text scope (34
`dependency_worktree_agent_ops`, 4 `pr_review`, 2 `research_docs`, 1 each
`bugfix`/`planning_spec`/`implementation`, 7 still `unknown`, 3 with no stored
text label). The classifier fix is confirmed; the two-judge silver gate is not
acceptable as a pass on this corpus.

## Measured outcome, 2026-09-26 (four judges)

The owner asked whether GPT 6 Luna and Gemini 3.8 Flash as additional judges lift
the gate. The same 150 episodes and the same fixed prompt were re-used; the
existing GLM/DeepSeek checkpoint was replayed, and only the two new judges were
called (150 each, zero recorded failures) through their subscription CLIs
(`codex-cli` GPT 6 Luna 0.155.1, `agy-cli` Gemini 3.8 Flash medium 1.2.11). No
paid gateway key was used for the new judges. The answer is no:

- Pairwise Cohen's kappa: GLM×DeepSeek **0.406**, GLM×GPT6 0.312,
  GLM×Gemini 0.199, DeepSeek×GPT6 0.364, DeepSeek×Gemini 0.221,
  GPT6×Gemini **0.406**. Minimum pairwise = 0.199.
- **Fleiss' kappa across all four judges: 0.306** (a separate statistic from the
  two-judge Cohen's kappa; both are far below the 0.6 floor).
- Per-judge `unknown` rate: DeepSeek 3.3%, GPT 6 Luna 8.7%, GLM 15.3%, Gemini
  3.8 Flash 16.7%. Every judge still skews to
  `dependency_worktree_agent_ops` (128/150, 129/150, 99/150, 119/150).
- Jev accuracy / macro-F1 against each reference: GLM 0.628/0.391,
  DeepSeek 0.527/0.366, GPT 6 Luna 0.527/0.317, Gemini 0.547/0.276; majority
  3-of-4 (126 episodes) 0.565/0.342; unanimous 4-of-4 (85 episodes)
  0.675/0.422. Two episodes had no stored Jev classification.
- Adding two stronger subscription judges did **not** increase agreement. No
  reference clears kappa >= 0.6, so the owner's no-pass-below-0.6 rule stands and
  the gold gate is not claimed.

**These are the 150-episode run's numbers**, from the recorded checkpoint
`judge_checkpoint_4judge.json`, not the small checked-in test fixture. The
mechanics fixture `tests/fixtures/silver/recorded_multi_judge_answers.json` has
only 8 episodes and necessarily different values (two-judge GLM×DeepSeek `0.6`,
minimum pairwise `0.3333`, Fleiss `0.528`); it exists to exercise the multi-judge
math without network or subprocess calls. The fixture's own values are pinned by
`test_fixture_pins_its_own_kappa_values_not_the_150_episode_run` so a fixture
edit cannot silently rewrite this measurement note.

**Method note.** The #34 merge changed framework stripping to
`FRAMEWORK_FILTER_VERSION` 1.1.0, so rebuilding from the live projection under
the current branch yields different episode text. To keep all four judges on the
identical documents the checkpoint was built from, the two-judge sample was
reconstructed with the pre-#34 code and verified hash-for-hash against the saved
`sample150.json` (262 eligible, 150 sampled, 0 text-hash mismatches); the judge
prompt is byte-identical between the two code states. Artifacts:
`/home/coolhenrylinux/reports/jev-xb17jn/4judge/`.

## Reproducing

```bash
python3 -m agent_observatory silver-build \
  --candidates observatory/gold_candidates_400.csv \
  --db obs.db --sample-size 150 --seed silver-v1 \
  --out-gold silver_gold_150.json --out-report silver_report_150.json \
  --jev-predictions-out jev_predictions_150.json \
  --checkpoint judge_checkpoint.json --max-judge-requests 300
python3 -m agent_observatory silver-evaluate \
  --gold silver_gold_150.json --predictions jev_predictions_150.json \
  --out silver_eval_150.json
```

`--checkpoint` makes the 300-call run resumable; a partial build replays the
answers already on disk instead of re-spending them.

## Collapse re-score: the label taxonomy is not the whole story, 2026-09-26

Four judges (GLM 5.3 Flash, DeepSeek V4 Flash, GPT 6 Luna, Gemini 3.8 Flash) on
the same 150 episodes produced pairwise Cohen's kappa 0.199-0.406 and Fleiss
0.306: adding judges did not create a trustworthy reference. `collapse.py`
re-scores the recorded checkpoint (`judge_checkpoint_4judge.json`) with **no new
judge call** to test whether a coarser `primary_intent` taxonomy fixes it.

- **Where the disagreement lives.** 65 of 150 episodes are disagreements. They
  are dominated by `dependency_worktree_agent_ops` vs `unknown` (40 episodes,
  134 of the judge-pair comparisons) and `dependency_worktree_agent_ops` vs
  `pr_review` (14 episodes, 43 comparisons); `bugfix` vs
  `dependency_worktree_agent_ops` is a distant third (7 episodes).
- **Proposed collapse (`primary_intent_collapse_v1`).** Merge
  `dependency_worktree_agent_ops` with the two review labels into
  `agent_ops_review`; keep every other label; keep `unknown` as a label, with a
  second variant that treats it as an abstention. On the recorded checkpoint the
  minimum pairwise Cohen's kappa moves 0.199 -> 0.236 (label policy) and
  0.317 -> 0.482 (abstain policy); Fleiss' kappa moves 0.306 -> 0.325 and
  0.444 -> 0.554. Jev against the unanimous reference moves from
  accuracy/macro-F1 0.675/0.422 to 0.779/0.494 (label) and 0.671/0.501 to
  0.777/0.624 (abstain); against the 3-of-4 reference it moves 0.565/0.342 to
  0.689/0.396 (label) and 0.546/0.360 to 0.678/0.428 (abstain).
- **No design clears 0.6.** The collapse is at the ceiling, not a tuning miss: a
  search over every partition of the nine labels into at least three classes of
  at most three source labels finds no minimum pair kappa above 0.243 (label
  policy) or 0.482 (abstain policy), and the proposed collapse is at that
  abstain ceiling (Fleiss 0.554). Only degenerate two-class partitions (for
  example `planning_spec` vs everything else after dropping abstentions) reach
  1.0, and those no longer classify intent. The owner's no-pass-below-0.6 rule
  stands, and the fresh `silver-v2` confirmation run authorised only above the
  floor was therefore **not spent**.
- **What this means.** The dominant boundary is abstention calibration
  (`dependency_worktree_agent_ops` vs `unknown`), which is why dropping unknown
  helps most; the remaining substantive ops/review boundary still caps the
  minimum pair kappa below 0.5. A passing gate needs a different reference
  design (for example an explicit abstention/`known` split, adjudication, or
  human review of the 65 contested episodes), not merely more judges or a
  narrower label set.

Reproduce with `agent-observatory silver-collapse` (see the README). The
artifacts are `silver_collapse_4judge_150.json`, `sensitivity.json` and
`summary.md` under `/home/coolhenrylinux/reports/jev-xb17jn/collapse/`.

## Measured outcome, 2026-09-27 (two-stage abstention split)

The collapse re-score concluded that a passing gate needs a different reference
design, "for example an explicit abstention/`known` split, adjudication, or human
review of the 65 contested episodes". The owner chose the explicit split on
2026-09-27: `two_stage.py` asks the **evidence gate** as its own question
(stage 1: `known` vs `unknown`) and asks the **intent** only of episodes a 3-of-4
majority called `known`, over the eight substantive labels with **no `unknown`
option** (stage 2). Each stage is scored on its own, and the gate is fail-closed:
a pass requires the conservative minimum pairwise Cohen's kappa to clear `0.6`
on **both** stages, on a large-enough sample whose judge pairs clear the shared
usable-overlap floor. No `--allow-untrusted` pass is claimable.

### Zero-call approximation (preview, no judge call)

`agent-observatory silver-two-stage --checkpoint judge_checkpoint_4judge.json`
derives stage 1 from the recorded `unknown` vs non-`unknown` votes and stage 2
from the non-`unknown` votes on the 131 episodes a 3-of-4 majority called
non-`unknown`. It makes no judge call, but it is only a preview: a judge that
abstained has no stage-2 answer, so stage-2 overlap is below a live run's (110 of
131 on the worst pair, under the 0.9 coverage floor).

| Stage | Label set | min pairwise Cohen | Fleiss | Trust reason |
| --- | --- | --- | --- | --- |
| 1 evidence (derived) | known / unknown | 0.153 | 0.240 | kappa below floor |
| 2 intent | original 8 labels | 0.317 | 0.444 | overlap below floor |
| 2 intent | `primary_intent_collapse_v1` | 0.482 | 0.554 | overlap below floor |

`clears_floor` is false. (Artifact:
`/home/coolhenrylinux/reports/jev-xb17jn/two-stage/approximation.json`.)

### Live run (four judges, same 150 episodes)

Prompt pair `two-stage-1.0.0` (`two-stage-evidence-1.0.0` +
`two-stage-intent-1.0.0`). The same 150 stripped episode documents as the
four-judge run, the same four judges and backends: GLM 5.3 Flash and DeepSeek V4
Flash through the gateway, GPT 6 Luna through `codex-cli` 0.155.1, Gemini 3.8
Flash through `agy-cli` 1.2.11. Stage 1 ran on all 150 episodes; stage 2 ran on
the 31 episodes a 3-of-4 majority called `known`. Artifacts:
`two_stage_report_150.json`, `stage1_checkpoint.json` and
`stage2_checkpoint.json` under `/home/coolhenrylinux/reports/jev-xb17jn/two-stage/`.

**Stage 1.** Asking the evidence question directly is a different measurement
from the derived labels: the judges abstain far more often. `known`/`unknown`
counts per judge: GLM 67/83, DeepSeek 40/110, GPT 6 Luna 56/94, Gemini 24/125
(one agy answer failed twice and is missing, 0.7% of that judge's stage-1 pairs,
within the coverage floor and never imputed). 60 episodes drew a unanimous
`unknown`; only 31 drew 3-of-4 `known`.

- Minimum pairwise Cohen's kappa **0.274** (GLM x GPT 6 Luna), Fleiss 0.395 —
  below the floor. All pairs: GLM x DeepSeek 0.593, DeepSeek x Gemini 0.570,
  DeepSeek x GPT 6 0.365, GPT 6 x Gemini 0.354, GLM x Gemini 0.323, GLM x GPT 6
  0.274.
- References: 77 unanimous, 56 majority, 17 none. Jev's own abstention decision
  scored against the 77 unanimous evidence references: accuracy 0.697, macro-F1
  0.677; against the 133 majority-or-better references: 0.583 / 0.575.
- **Stage 1 does not clear, so the two-stage gate cannot pass regardless of
  stage 2.**

**Stage 2.** On the 31 eligible episodes, intent agreement with no `unknown`
option:

- Original 8 labels: minimum pairwise Cohen's kappa **0.421** (GLM x GPT 6
  Luna), Fleiss 0.533.
- `primary_intent_collapse_v1`: minimum pairwise **0.549** (GLM x GPT 6 Luna),
  Fleiss 0.648. Four of the six pairs clear 0.6 — DeepSeek x Gemini 0.704,
  GPT 6 x Gemini 0.734, GLM x DeepSeek 0.668, GLM x Gemini 0.608 — but GLM x
  GPT 6 caps the minimum below the floor.
- Jev against the stage-2 references: original unanimous (16) 0.800 / 0.489,
  majority (28) 0.556 / 0.428; collapsed unanimous (22) 0.857 / 0.480, majority
  (30) 0.759 / 0.474.

**Gate: not claimed.** Stage 1's 0.274 and stage 2's 0.421 (0.549 collapsed) are
both below 0.6, so `clears_floor` and `clears_floor_all_scorings` are false.

### What the split establishes

- **The abstention split helps, but not enough.** On the same documents the
  dedicated evidence question raises stage-1 agreement from the derived 0.153 to
  0.274, and the stage-2 collapse raises the single-pass collapse abstain kappa
  from 0.482 to 0.549. Neither reaches 0.6, and the gate needs both.
- **Asking the question directly changes the answer.** The single-pass judges
  called 137-147 of 150 episodes non-`unknown`; asked whether there is enough
  evidence at all, they call only 24-67 `known` and 60 episodes draw a unanimous
  `unknown`. The boundary was never a stable label — it was an unstable
  confidence threshold, which is why mapping it from single-pass votes (0.153)
  understates how much the judges disagreed about it.
- **The remaining substantive boundary still caps stage 2.** Even with no
  `unknown` option and only 31 "easy" episodes, GLM and GPT 6 Luna disagree on
  intent (0.421 original, 0.549 collapsed). More judges or a lower floor are not
  the missing ingredient; adjudication or human review of the contested
  episodes is.

Judge calls spent: 600 stage-1 answers attempted (599 stored; the one agy failure
above) and 124 stage-2 answers (all stored). An early version of the operational
driver duplicated calls before its worker partitioning was fixed; the durable
counts above are the stored answers, and the run is resumable from the two raw
checkpoints.


