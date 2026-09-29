# Agent Observatory (offline foundation slice)

A dependency-free Python 3 CLI and library for **normalized evidence import,
classification storage, and deterministic reporting** over Gas City agent
activity. It is the first bounded slice of the Jev-powered historical/live chat
analysis effort.

This is deliberately **not** the whole product and **not** production-ready:

- **Transport is explicit.** The request builder only emits a `/v1/systemone`
  request body; nothing is sent by `build-request`. The `classify` subcommand
  performs a live, bounded send (retries, budgets, circuit breaker) and records
  provenance; saved responses are otherwise validated offline.
- **Collection is explicit and bounded.** `collect` reads only the roots it is
  given (there is no implicit home-directory crawl), checkpoints every source,
  and stops on a kill switch. Nothing in Gas City waits on it.
- **No paid API calls, real transcript uploads, publishing, or merging.**
- **No orchestration truth.** Beads and events remain authoritative. The SQLite
  file is a local, rebuildable analytical projection.
- All policy and vendor specifics live here under `contrib/agent-observatory/`;
  no core Go SDK, runtime, config, or routing code is touched.

## Layout

```
contrib/agent-observatory/
  agent_observatory/
    __init__.py        public API and version
    __main__.py        python3 -m agent_observatory
    cli.py             argparse CLI
    contract.py        versioned normalized JSONL contract + strict validation
    canonical.py       canonical JSON, hashing, event/session snapshot identity
    store.py           SQLite schema versioning, transactional import, classifications
    migrations.py      explicit, backed-up SQLite schema upgrades
    commands.py        conservative, non-executing command categorization
    changes.py         M5 optimization change registry + conservative screen
    exposure.py        M5 commit/fingerprint exposure join + optimization ledger
    report.py          deterministic report JSON
    taxonomy.py        versioned question taxonomy loader (questions + facets)
    taxonomy/jev_taxonomy_v1.json   wire taxonomy (implemented intent subset)
    taxonomy/jev_taxonomy_v2.json   wire taxonomy + full evaluation facets
    jev.py             /v1/systemone request builder + saved-response validation
    framework.py       strips injected Gas City framework payloads from transcript text
    episodes.py        deterministic session -> task-episode segmentation
    annotations.py     versioned gold annotations (separate from predictions)
    evaluation.py      grouped temporal holdout, baselines, metrics, calibration
    silver.py          multi-judge machine-built silver reference (pluggable gateway/CLI backends)
    collapse.py        coarse-taxonomy re-score of a recorded multi-judge checkpoint
    collector.py       M4 checkpointed backfill, debounced collection, queue, status
    impact.py          M6 accepted-task impact reports, matched cohorts, uncertainty
    policy.py          M7 shadow policy recommendations (as-of, catalog-bounded)
    canary.py          M8 pre-registered, opt-in, seeded policy canary + kill switch
  examples/
    synthetic_events.jsonl   synthetic fixture (no real data)
    state.json               explicit sanitized state for the request builder
    response.json            synthetic saved Jev response
    demo.sh                  end-to-end CLI demo into a temp SQLite file
  tests/                     unittest suite
    fixtures/jev_smoke_contract.json  synthetic-only pinned real wire exchange
    fixtures/gold/gold_episodes_v1.json  pinned gold mechanics fixture (synthetic)
    fixtures/gold/predictions_jev_v1.json  model predictions for that fixture
    fixtures/impact/known_effect.json   matched replay: a real effect is visible
    fixtures/impact/no_effect.json      matched replay: no effect is claimed
    fixtures/impact/confounded.json     replay: no overlap, confounding detected
    fixtures/policy/catalog.json        current configured candidate catalog
    fixtures/policy/shadow_bundle.json  as-of classifications for shadow replay
    fixtures/canary/registration.json   pre-registered canary design (M8)
    fixtures/canary/units.json          canary units with observed outcomes
    fixtures/canary/units-unclassified.json  units that would spend live requests
    fixtures/silver/candidates.csv       candidate episodes for the silver sample
    fixtures/silver/recorded_judge_answers.json  recorded judge answers (no network)
    fixtures/silver/recorded_multi_judge_answers.json  recorded four-judge answers (no network)
    fixtures/collapse/recorded_judge_checkpoint.json  recorded four-judge checkpoint
    fixtures/collapse/recorded_judge_report.json  the same votes as a silver report
    fixtures/collapse/jev_predictions.json  Jev predictions for the collapse rescorer
  reports/real-obsdb-20260922.json  real historical report from a copy of obs.db
  MEASUREMENT-SILVER.md   what the silver gate does and does not establish
  README.md
```

## Quick start

```bash
cd contrib/agent-observatory
bash examples/demo.sh

# or manually
python3 -m agent_observatory import-jsonl --db /tmp/obs.db examples/synthetic_events.jsonl
python3 -m agent_observatory report --db /tmp/obs.db
```

Requires Python 3.11+ (stdlib only; tested on CPython 3.14). Arbitrary
fractional-second ISO-8601 parsing uses `datetime.fromisoformat` support added
in 3.11. No new pip dependencies.

## 1. Normalized JSONL import contract

Each non-empty line is one JSON object. `schema_version` is `"1.0"`.

| Field | Required | Type | Meaning |
| --- | --- | --- | --- |
| `schema_version` | yes | string | Contract version (`"1.0"`). |
| `city_id` | yes | string | City that observed the event. |
| `host_id` | yes | string | Host that observed the event. |
| `provider` | yes | string | Runtime/provider, e.g. `codex`, `claude`, `dsh`. |
| `session_id` | yes | string | Provider session id. |
| `event_id` | yes | string | Event id, unique within the canonical identity. |
| `timestamp` | yes | string | Timezone-aware ISO-8601; normalized to UTC microseconds. |
| `kind` | yes | string | Event kind (`command`, `tool_call`, `tool_result`, ...). |
| `title` | no | string | Human title. Never part of identity. |
| `text` | no | string | Observed text. Never part of identity; treated only as data. |
| `tool_name` | no | string | Tool/program name. |
| `tool_call_id` | no | string | Links a result to its invocation. |
| `command` | no | string | Command text. Never executed. |
| `exit_code` | no | integer | Observed exit code; missing means unknown. |
| `duration_ms` | no | integer | Observed duration; must be nonnegative. |
| `model` | no | string | Model that produced the event. |
| `repo` | no | string | Repository. |
| `commit_sha` | no | string | Commit. |
| `parent_session_id` | no | string | Parent session, if any. |
| `bead_id` | no | string | Related bead. |
| `formula_id` | no | string | Related formula. |
| `usage` | no | object/null | Nullable token counts. |

`usage` accepts `input_tokens`, `output_tokens`, `cache_read_tokens`,
`cache_write_tokens`, `total_tokens`; each is a nonnegative number (integer or
float, so a provider's fractional counter is not discarded) or null.
**Missing is distinct from zero**: absent usage stays NULL, and
`{"input_tokens": 0}` stores `0`.

### Evidence semantics

Records are **observed evidence**. A tool request (`tool_call`) does not prove
the tool executed, and does not prove success. Only a result event with an
`exit_code` carries an observed outcome; a missing `exit_code` is reported as
`unknown`, never as success.

### Strictness and safety

- Required fields must be present, non-empty strings; `bool` is never accepted
  as an integer.
- **Timestamps must be timezone-aware** (an explicit offset or `Z`). Naive
  timestamps are rejected because their timezone is ambiguous. The stored
  `timestamp` is normalized to UTC at microsecond precision so plain string
  ordering equals chronological ordering across mixed offsets and mixed
  fractional widths; `duration_ms`, when present, must be nonnegative.
- Optional fields, when present and non-null, must have the declared type.
- **Unknown keys are dropped** and stay NULL in the projection. There is no
  column for secrets or raw chain-of-thought, and none is stored by default.
- **Canonical event identity is `city_id/host_id/provider/session_id/event_id`**
  -- never `title` or `text`. The same event id under a different provider or
  host is a different event.
- Source `source_path`, `source_sha256`, and 1-based `source_line` are preserved
  for every event. The exact input `timestamp` is retained separately as
  `observed_timestamp` provenance; it is not part of the payload hash.
- Identity components are unrestricted strings, so reports and CLI records use a
  collision-free JSON-array encoding of the identity tuple (for example
  `["city-a","host-a","codex","session-1"]`) rather than a delimiter join.
- Instruction-like text inside `text` or payloads is stored and reported only as
  data; it never becomes instructions to any model.

## 2. SQLite projection

- Schema version is stored in `PRAGMA user_version` and `schema_meta`. Opening a
  store with an unknown/future version raises `SchemaVersionError`. The current
  version is **5**; version 3 excluded identity fields from the payload hash (so a
  version-2 projection must be rebuilt rather than reused), version 4 added the
  M5 change/exposure registry, and version 5 adds the M7 shadow
  `recommendations` projection.
- Upgrade a schema-4 projection explicitly with
  `python3 -m agent_observatory migrate --db /path/to/obs.db`. The command refuses
  every version except a valid v4 database, writes a timestamped
  `<db>.v4-pre-schema5-<UTC timestamp>.bak` beside the original first, then adds
  the schema-5 table/index and updates both version markers in one transaction.
  It verifies that row counts in `events`, `sessions`, `classifications`, and
  `classification_answers` match the backup and remain unchanged. Normal store
  opens never migrate an existing database implicitly.
- Import is **per-file atomic**: the whole file is parsed and type-checked
  before writing, and all writes happen in one transaction. A malformed or
  truncated line reports `path:line` and commits nothing. Records are split on
  **LF only** (a trailing CR is stripped), so U+2028/U+2029/U+0085 characters
  that are legal unescaped inside JSON strings never split a record or shift
  reported line numbers.
- Import is **idempotent**: re-importing an identical file is skipped, and
  appending to a file imports only new events. Repeated files never duplicate
  events or usage.
- A **conflicting reuse of a canonical event identity** (same identity,
  different payload) is a **record-level skip**: the conflicting record is not
  written, `skipped_conflicts` is incremented, and a note naming the identity and
  source line is returned. The rest of the file still imports, so one reused
  native id cannot erase a whole session's evidence.
- Each `sessions` row records `first_timestamp` as the **minimum** observed
  timestamp; importing an earlier event later lowers it.
- Tables: `events`, `event_usage`, `sessions`, `imported_files`, `jev_requests`,
  `classifications`, `classification_answers`.
- Classifications are **immutable**. They are keyed by subject (`event`/`session`
  snapshot hash), taxonomy version, question hash, and model version. Replaying
  an identical response deduplicates; a different response for the same key is
  rejected (`LabelConflictError`) rather than overwriting. The existence check
  and the insert run in one `BEGIN IMMEDIATE` transaction, so a concurrent
  UNIQUE collision is folded into the same dedupe/conflict outcomes. Complete
  probability distributions and response provenance are stored.

### Subject snapshot hashes

`event` snapshots hash the canonical identity plus the event payload hash;
`session` snapshots hash the session identity plus its ordered event payload
hashes. Both are deterministic. The **payload hash covers normalized content
fields only**: canonical identity (`city_id`/`host_id`/`provider`/`session_id`/
`event_id`) is matched separately and is covered by the event snapshot hash.

## 3. Deterministic report

`report` emits JSON with sorted keys and no timestamps, so runs over the same
projection are byte-identical. It contains:

- `coverage`: `sessions`, `events`, `by_provider`, `by_kind`, and
  `missing_fields` NULL counts.
- `tool_counts`: **invocation-only** counts per `tool_name`. A result event that
  repeats a tool name never adds a count. An invocation with no `tool_name` is
  counted under `unknown`, so `tool_counts` and `observed_outcomes` describe the
  same set of invocations.
- `command_categories`: counts for every category across distinct invocations.
  Result events and arbitrary prose records are never promoted to invocations,
  even when they contain a command. Recognition is conservative and
  non-executing; compound commands may have multiple categories; anything
  unrecognized falls back to `unknown`. Categories: `search`, `read`, `edit`,
  `test`, `lint`, `typecheck`, `build`, `package`, `git`, `worktree`, `ci`,
  `review`, `dispatch`, `wait`, `unknown`.
- `observed_outcomes`: `success`/`failure`/`unknown` by category and in total,
  counted once per invocation. An invocation's outcome is paired to a result by
  `(session, tool_call_id)`. Invocations with no `tool_call_id` are keyed by
  their own event id in a **separate namespace**, so a result whose
  `tool_call_id` happens to equal an invocation's event id cannot pair with it;
  a result with no `tool_call_id` likewise cannot pair. An invocation still
  inherits its own categories even when the result carries no command. A
  request-only event stays `unknown` even if it carries a claimed exit code; a
  directly observed `command` may use its own exit code. Missing `exit_code` is
  `unknown`, never success. Orphan result events (no matching invocation) are
  still observed once, but never become invocations.
- `test`: `invocations` and `results` (`passed`/`failed`/`unknown`) are reported
  separately. Duplicate or conflicting result events never inflate invocations;
  conflicting exit codes for one invocation stay `unknown`.
- `session_steps`: per collision-free session identity key, ordered `sequence` of
  kinds and `transitions` counts.

The report includes explicit notes that counts are observational and do not
establish effectiveness, causality, or cost.

## 4. Jev request builder

`build-request` produces a `POST /v1/systemone` body from an **explicitly
supplied sanitized state JSON file**. The wire shape follows the real TypeSafe
API reference:

```json
{
  "model": "jev-1.13.0",
  "state": { "...": "caller-supplied only" },
  "questions": {
    "primary_intent": {
      "type": "choice",
      "instructions": "Classify the primary requested software task ...",
      "criteria": {
        "bugfix": "Correct an existing defect.",
        "implementation": "Add new behavior ...",
        "unknown": "Insufficient evidence ..."
      }
    },
    "requests_test": {
      "type": "noul",
      "instructions": "What is the probability that ...?",
      "criteria": { "true": "...", "false": "..." }
    }
  }
}
```

- There is **no top-level `instructions` field**, no per-question `description`,
  and no `options` list. Question `type` is lowercase `choice`/`noul`; a choice
  question's `criteria` keys are its options, and each question carries its own
  `instructions`.
- `primary_intent` is a `choice` whose criteria include an `unknown` option and
  the categories `bugfix`, `implementation`, `pr_review`, `adversarial_review`,
  `planning_spec`, `test_lint_build_ci`, `dependency_worktree_agent_ops`,
  `research_docs`.
- `scope` (`small`/`large`/`unknown`) is judged **independently of intent**.
- Overlapping labels are `noul` yes/no probability questions with explicit
  `true`/`false` criteria (a probability, not an unanchored degree score).
- Taxonomy definitions live in the versioned `taxonomy/jev_taxonomy_v1.json`
  (taxonomy version `1.1.0`). The question hash covers the taxonomy version,
  model, and every effective instruction and criterion, so any rubric change
  produces a new classification key.
- **Question map keys are identifiers, not model instructions**; instructions
  are carried in each question's `instructions` value.
- The serialized body is capped at **24 KB of UTF-8** (`REQUEST_BYTE_CAP`).
  Exceeding it raises `RequestByteCapExceeded` instead of silently truncating.
  This is a **byte safety cap, not tokenizer proof**.
- The builder never reads imported event text. An empty state with no caller
  input yields an empty `state`, so untrusted transcript text cannot leak into a
  request implicitly.

## 5. Saved response validation

`import-response` validates a saved response against the stored request:

- `model` must match; `usage.input_tokens`/`output_tokens` must be nonnegative
  integers.
- Question ids and lowercase types must match exactly (no missing/extra answers).
- `choice` answers: the `choice` field must be one of the question's criteria
  keys; `probabilities` must cover every criterion with finite values in `[0,1]`
  summing to ~1; `confidence` finite in `[0,1]`. The previously invented
  `value` field is not accepted.
- `noul` answers: exactly one finite `noul` probability in `[0,1]` and **no
  confidence field**.
- Each answer object accepts only its wire keys (`type` plus `choice`/
  `confidence`/`probabilities` for choice, or `noul` for noul). Any **unknown
  key is rejected** with a `ContractError` naming it; it is never silently
  dropped, so two responses with identical answers but different junk keys can
  never be mistaken for a label conflict.
- Non-finite JSON (`NaN`/`Infinity`) is rejected.
- Nothing is fabricated: missing labels, missing confidences, and live calls are
  all errors, not defaults. `tests/fixtures/jev_smoke_contract.json` pins a
  synthetic-only real exchange so the wire shape cannot silently regress.

## 6. Live transport credential

`classify` reads the bearer token from the runtime environment only (never from
argv, the repository, or a config file):

| Source | Precedence | Notes |
| --- | --- | --- |
| `TYPESAFE_API_KEY` | 1 (preferred) | Primary variable name. |
| `JEV_API_KEY` | 2 (alias) | Accepted for backward compatibility. |
| `JEV_KEY_FILE` | 3 | Path to a key file. |

A key file is decoded as `utf-8-sig` (a UTF-8 BOM is stripped per line) and
parsed under a strict contract; the first accepted line wins:

- `[export ]NAME=value` or `NAME: value` where `NAME` is `KEY`, `API_KEY`,
  `APIKEY`, `SECRET`, `TOKEN`, `TYPESAFE_API_KEY`, `JEV_API_KEY`, or any name
  ending in `KEY`, and `value` is a single `[A-Za-z0-9_.-]{16,}` token after
  stripping surrounding quotes/backticks;
- `Bearer <token>`;
- a single bare token line.

Blank lines and `#` comments are skipped. Prose, labels, note pointers
(`API key: (see vault)`), and markdown labels are **never** returned as a
credential: if no accepted line exists, `load_credential` raises a
`CredentialError` naming every skipped line number.

Other transport rules worth knowing: `timeout_seconds` is capped at 300 s;
usage is validated before it is charged and only the validated integer token
fields are exposed; response bodies are read in bounded chunks on both the 2xx
and error paths; the circuit breaker persists its state and reserves a single
half-open probe.

## 7. Evaluation: facets, episodes, gold labels, calibration

The classifier is only useful if it is measured. This slice adds the evaluation
machinery; it does **not** claim classifier quality from the synthetic fixture
and it never treats a model label as execution authority.

### Facets

`taxonomy/jev_taxonomy_v2.json` declares the full `taxonomy.md` facet vocabulary
alongside the wire questions: `primary_intent` (the implemented wire subset),
`secondary_activity`, `work_unit`, `scope`, `workflow`, `phase`, `target`,
`disposition` and `bottleneck_hypothesis`. A facet is `one` (mutually exclusive)
or `many` (an independent label set). `Taxonomy.facet_hash()` pins the label
space separately from `question_hash()`, which stays the live wire shape. The new
schema is additive: v1 still loads, and the default `build-request` taxonomy is
unchanged.

### Episodes

`episodes.py` splits a session into task episodes at configured boundary kinds
(`user`, `user_request`, `user_excerpt`, `task`), never dropping an event. The
episode id is a hash of the session identity and first event id; a session with
no boundary is one episode. The lineage root (resolved through
`parent_session_id`) is the **continuation group** so resumed/subagent sessions
cannot leak across an evaluation split.

### Gold annotations

`annotations.py` loads a versioned gold set and validates every label against the
taxonomy facet cardinality. Annotations are stored in their own append-only
`gold_annotations` table, keyed by content: replay dedupes, and a correction is a
new row rather than an overwrite of the old label or any prediction. The content
key covers the labels, flags, annotator, adjudication and metadata, so a second
annotator's independent label or a metadata-only correction also appends a row.
Flags (`uncertain`, `injected`, `contested`, `rare`, `non_english`, `synthetic`,
`changed_intent`) keep ambiguous and adversarial cases report-only.

### Split and audit

`grouped_temporal_split` orders continuation groups by earliest observed time
(normalized to canonical UTC so mixed offsets order chronologically) and holds out
the latest fraction; whole groups stay together. `audit_split` reports
`leak_free`, `temporal_order_ok` and any violations. A report whose split is not
leak-free makes the CLI exit non-zero.

### Metrics, baselines, calibration and the routing gate

For the primary facet the report records per-class precision/recall/F1/support,
macro and micro F1, a confusion matrix, accuracy, coverage and abstentions. For
each many-valued facet it records per-label and micro/macro F1. Probabilistic
predictions also get a multiclass Brier score, reliability bins and expected
calibration error, plus Wilson lower bounds on holdout precision. Deterministic
`title_only` and `metadata_only` baselines are always scored on the same holdout
so a semantic model is compared against weak evidence, never an empty baseline.
`macro_f1` averages only over gold-labelled classes (a predicted-only class
appears in `per_class` with zero support but not in the macro average), and
`coverage` counts an `unknown` prediction as covered even though the automation
gate abstains on `unknown`.

The automation gate is conservative: rare classes (tuning support below
`--min-class-support`, including classes unseen in tuning), `unknown` labels,
low-confidence predictions, and any case flagged `injected`, `uncertain`,
`contested`, `changed_intent` or `rare` are **never** eligible to auto-route,
regardless of model confidence. The report records every abstention and its
reason, and asserts there are no gate violations.

### Silver reference (no hand labelling)

Owner decision 2026-09-25: the human gold set is replaced by a machine-built
**silver** reference (`silver.py`). `silver-build` samples candidate episodes
deterministically, stratified by provider, and asks a configurable set of
independent judges to label `primary_intent` from the **same stripped transcript
text** with the **same taxonomy criteria** at temperature 0 and JSON output. The
judge prompt is fixed and versioned (`JUDGE_PROMPT_VERSION`). The default set is
still the two Uniblock prod gateway judges — GLM 5.3 Flash
(`uniblock-prod/fireworks-ai/glm-5p3-flash`) and DeepSeek V4 Flash
(`uniblock-prod/deepseek/deepseek-flash`). `--judge JUDGE ...` (judge id, model
slug, or `backend:model`) and `--judges-file JUDGES.json` (a list of
`{judge_id, model, api_model, backend}`) add or replace judges. Each judge is
bound to a **pluggable backend**: `gateway` (OpenAI-compatible HTTP; the
historical path), `codex-cli` (the owner's Codex subscription via
`codex exec -m gpt-6-luna`, read-only sandbox, ephemeral) or `agy-cli` (the
owner's Antigravity subscription via
`agy --model gemini-3.8-flash-medium --json-schema`). Model names are never
guessed — they are resolved from each CLI's own `--version`/model list and
recorded per label.

Adjudication reuses the existing annotation vocabulary: judges that all agree make
an `adjudicated` silver label; any disagreement is recorded as `disagreement`,
excluded from the primary score and reported. Each episode records the per-judge
labels, confidences, model ids, backend, CLI version, prompt version and text
hash; the gold set keeps `annotator="silver-judges"` and is strictly separate
from Jev predictions. `silver-build` reports every pairwise Cohen's kappa,
Fleiss' kappa across the whole judge set, and each judge's label distribution and
`unknown` rate. `silver-evaluate` reports Jev accuracy/macro-F1 against each
judge alone, the majority label and the unanimous label. **Kappa below
`KAPPA_TRUST_FLOOR` (0.6) means the silver set is not trustworthy and the gate is
not claimed** — the trust value is the minimum pairwise kappa, so every judge
pair must clear the floor. A sample with fewer than `MIN_SILVER_SAMPLE_SIZE` (4)
episodes is also untrusted regardless of kappa, because kappa over a handful of
items is degenerate. Those floors are enforced, not just reported: `silver-build`
refuses to write the gold set and exits nonzero on a below-floor kappa or sample
(the agreement report is still written as evidence), and the loader/
`silver-evaluate` refuse to consume a silver set marked `silver_trustworthy:
false`. The explicit `--allow-untrusted` flag is the only override.

The limitation is explicit: silver labels measure agreement between the judge
set, not ground truth. A Jev error shared by every judge is invisible. See
`MEASUREMENT-SILVER.md`.

Reproducibility: the report pins `taxonomy_version`, `facet_hash`,
`question_hash`, `model`, `gold_set_version`, `gold_set_hash` and an
`evaluation_hash` over the split, provenance and every prediction hash, so a
model or question revision produces a different, replayable report. The pinned
`tests/fixtures/gold/` fixture proves the evaluator mechanics; the silver path
above is how real per-class quality is established without hand labelling.

### Collapse re-score of a multi-judge checkpoint

When more judges do not lift agreement, the taxonomy is the suspect. `collapse.py`
re-scores an already-recorded multi-judge checkpoint under a coarser
`primary_intent` mapping **without any judge call**: a versioned
`LabelCollapse` (original label -> collapsed label, one disambiguating
definition per collapsed label), pairwise confusion matrices, disagreement
episodes grouped by the labels that compete, pairwise Cohen's and Fleiss' kappa,
unanimous and 3-of-4 reference labels, and Jev accuracy/macro-F1 against those
references. `unknown` can be kept as a label or treated as an abstention
(complete-case per pair/reference); both are reported.

```
agent-observatory silver-collapse \
  --checkpoint judge_checkpoint_4judge.json \
  [--report silver_report.json] --jev-predictions jev_predictions.json \
  [--collapse primary_intent_collapse_v1] [--judge JUDGE ...] [--majority 3] \
  --out collapse_report.json [--require-clear]
```

The gate is the conservative minimum pairwise Cohen's kappa (matching
`kappa_over_judges`); Fleiss' kappa is secondary. `baseline` in the report is the
identity collapse, `diagnostics` holds the confusion/disagreement evidence, and
`clears_floor` is true only for a design that reaches `KAPPA_TRUST_FLOOR` on a
sample of at least `MIN_SILVER_SAMPLE_SIZE`. `build_collapsed_judge_prompt` builds
the fresh-sample prompt for a confirmation run, which is authorised only after a
collapse clears the floor. On the four-judge checkpoint the proposed
`primary_intent_collapse_v1` raises minimum pair kappa from 0.199 to 0.236
(0.317 -> 0.482 with `unknown` as abstain) and reaches Fleiss 0.554, so **no
design clears 0.6 and no fresh run was spent**. A search over every partition of
the nine labels into at least three classes of at most three source labels finds
no minimum pair kappa above 0.243 (label) or 0.482 (abstain). See
`MEASUREMENT-SILVER.md`.

### Two-stage abstention split

The collapse work showed the dominant boundary is abstention calibration, so the
owner chose a reference design that separates it from the taxonomy. `two_stage.py`
splits `primary_intent` agreement into two independent, separately-scored
stages:

- **Stage 1 — evidence gate.** Each judge answers only `known` vs `unknown`:
  "is there enough evidence to name exactly one primary task?" Agreement is
  scored on its own (pairwise Cohen's and Fleiss' kappa).
- **Stage 2 — intent.** Only on episodes a majority called `known`, each judge
  picks one of the eight substantive labels; there is **no `unknown` option**.
  Agreement is scored on its own on the original labels and projected through
  `primary_intent_collapse_v1`.

```
agent-observatory silver-two-stage \
  (--checkpoint single_pass_checkpoint.json |
   --stage1-checkpoint stage1.json [--stage2-checkpoint stage2.json]) \
  --jev-predictions jev_predictions.json --out two_stage_report.json \
  [--judge JUDGE ...] [--majority 3] [--require-clear]

agent-observatory silver-two-stage-run --episodes-file episodes.json \
  --out-report two_stage_report.json \
  [--stage1-checkpoint stage1.json] [--stage2-checkpoint stage2.json] \
  [--jev-predictions jev_predictions.json] [--judge JUDGE ...] [--majority 3] \
  [--evidence-prompt-version V] [--intent-prompt-version V] [--require-clear]
```

`--checkpoint` is the **zero-call approximation**: stage 1 is the recorded
`unknown` vs non-`unknown` decision, and stage 2 uses the non-`unknown` votes on
episodes a majority called non-`unknown`. It is a preview only, because a judge
that abstained has no stage-2 answer, so its stage-2 overlap is lower than a live
run's. `--stage1-checkpoint`/`--stage2-checkpoint` score recorded live answers,
and `silver-two-stage-run` drives the live pass through the same judge backends
as `silver-build` (the two stages use separate checkpoints so a stage-2 call
never replays a cached stage-1 answer). The gate is fail-closed: a pass requires
the conservative minimum pairwise Cohen's kappa to clear `KAPPA_TRUST_FLOOR` on a
large-enough sample whose judge pairs clear the usable-overlap floor, for stage 1
**and** the original stage-2 labels; `clears_floor_all_scorings` additionally
requires the collapsed projection. No `--allow-untrusted` pass is claimable. Jev
is scored against the stage-1 abstention reference and each stage-2 reference
(accuracy / macro-F1). The prompt pair is versioned
(`TWO_STAGE_PROMPT_VERSION` plus the stage-specific versions). The measured
outcome is in `MEASUREMENT-SILVER.md`.

## 8. Optimization change and exposure registry

`changes-sync` imports an explicit, versioned **change bundle** (there is no live
PR crawler and no network access). Every screened change is retained, whether or
not it looks like an optimization, so the screening denominator is never
silently reduced.

- **Screening.** Changes are screened against the versioned optimization
  categories from `implementation-plan.md` (`package_resolution`,
  `dependency_footprint`, `test_selection`, `test_fixtures_parallelism`,
  `lint_typecheck`, `build_cache_bundle`, `ci_runner_cache`,
  `worktree_lifecycle`, `agent_runtime`, `host_io_network`,
  `telemetry_overhead`). The screen is deliberately conservative: a category is
  assigned only with retained path/keyword evidence, and a change with no
  category and no positive non-optimization evidence stays `unknown`. Both
  `non_optimization` and `unknown` remain in the denominator; only
  `optimization` changes become registered interventions. Non-PR intervention
  kinds (`config`/`model`/`runtime`/`prompt`/`toolchain`/`pack`/`host`) default
  to their natural category because they are interventions by construction;
  reviewed `classification`/`category_hint` still overrides.
- **Immutable records.** A PR is identified by repository and number; a non-PR
  intervention (config, model, runtime, prompt, toolchain, pack, host) by its
  immutable `artifact_digest`. Re-importing identical content deduplicates;
  re-importing the same identity with different content is refused
  (`RegistryConflictError`) rather than silently overwritten.
- **Activations.** A change can have several activation records (merge, deploy,
  config toggle, model/runtime/prompt switch, package/toolchain upgrade, host
  tuning), each with an optional scope `target`, validity interval, and a
  fingerprint (`commit_sha`, `artifact_digest`, `config_digest`, `model`,
  `toolchain`, `package`, `host`). A merged PR without an explicit activation
  gets an implicit merge activation from its merge commit, so the registry is
  self-sufficient while still requiring observed evidence.

`exposure` joins registered changes to observed session evidence -- `commit_sha`
and `model` values from the projection plus explicitly supplied
`session_fingerprints` -- and emits the deterministic optimization ledger. The
join never treats a merge as exposure:

- `exposed` requires positive evidence: an exact observed commit, commit
  ancestry from the change to an observed commit, or an observed
  artifact/config/model fingerprint inside the activation's validity interval.
- `unexposed` requires a decision: a known observed commit that provably does
  not contain the change, or a session that predates activation / follows
  deactivation, or an observed conflicting fingerprint.
- anything else is `unknown`. An incomplete commit graph, a session with no
  commit evidence, and an unobserved fingerprint are all ambiguous, never
  success. A pre-merge worktree (commit is an ancestor of the merge) is
  `unexposed`; a delayed deployment (activation still pending) or a session
  that predates `activated_at` is `unexposed` only while no direct fingerprint
  contradicts the window. When the session's own fingerprint matches the
  change, the window and the evidence disagree, so the evidence cannot decide:
  exposure is `unknown` (`evidence_conflicts_window`), never a silent
  `unexposed` that under-counts real use.

Each ledger entry carries `baseline` and `price` as `present`/`unknown`, and
`missingness` counts unknown baselines and prices. They are never synthesized;
effect estimates and cost accounting are M6, not this slice.

Bundle contract (schema version `1.0`):

```json
{
  "schema_version": "1.0",
  "changes": [
    {"repo": "gascity", "kind": "pr", "pr": 6, "title": "perf: cache the build",
     "merge_sha": "<sha>", "merged_at": "2026-09-21T10:00:00Z",
     "changed_paths": ["Makefile"], "baseline": {"metric": "test_s", "value": 12.0, "unit": "s"}}
  ],
  "activations": [
    {"change_ref": {"repo": "gascity", "kind": "pr", "pr": 6}, "mechanism": "deploy",
     "target": "gateway-llm", "activated_at": "2026-09-22T00:00:00Z",
     "fingerprint": {"type": "commit_sha", "value": "<sha>"}}
  ],
  "commit_graph": {"commits": [{"sha": "<sha>", "parents": ["<parent>"]}]},
  "session_fingerprints": [
    {"session": ["city", "host", "codex", "session-1"], "type": "config_digest",
     "value": "<digest>", "observed_at": "2026-09-22T00:30:00Z"}
  ]
}
```

## 9. Collection: backfill, live tailing and the classification queue

`collect` turns the M1 adapters into a checkpointed collector over explicit
roots. One pass discovers sources, imports what changed through the idempotent
store import, and queues each changed session's snapshot for classification.
`collect --watch` repeats the pass every `--interval` seconds. Agents never wait
on it and dispatch never reads it.

Every scoped source keeps one row in `collector_sources` with a status and a
reason:

| Status | Meaning |
| --- | --- |
| `imported` | Read this pass; the reason names `new`/`appended`/`rewritten`, the generation and new/duplicate event counts. |
| `unchanged` | Size and mtime match the checkpoint; not re-read. |
| `debounced` | Modified within `--debounce-seconds`; read on a later pass. A file that never goes quiet is read anyway after `--max-debounce-seconds` since the pending change was first seen; a replacement or truncation at the same path starts a fresh clock. |
| `deferred` | A per-run source/byte cap, the per-source cap or the projection storage cap was reached. The work waits; it is not dropped. A source deferred because of its own size is retried when its stat changes or the cap is raised, so an oversized `.zstd` stream is not re-read and re-decompressed every pass. |
| `error` | The adapter or import refused the source; the reason carries `path:line`. |
| `unreadable` | The file or directory could not be read or listed. |
| `unsupported` | A known provider without an adapter (OpenCode, pi). |
| `missing` | Seen before, gone now (rotation or deletion); its events stay. |

Files that no adapter recognizes (locks, notes, tool-result sidecars) are
counted per pass by suffix in `ignored_files`, not dropped silently.

Replay safety: a source is re-read only when its stat changes. Append keeps the
generation and rewrite increments it (the M1 rules). Imports are idempotent, so
if a crash lands between the import and the checkpoint write, the next pass
re-imports as duplicates and the counts stay the same. A failed import writes no
checkpoint and the source is retried. A per-projection advisory lock
(`<realpath(DB)>.collector.lock`) keeps two collectors from interleaving, and
`queue-drain` takes its own `<realpath(DB)>.drain.lock` so two overlapping drains
cannot select and pay for the same pending rows. Both locks are keyed on the
database real path, so a symlink and its target share one lock.

Bounds:

- `--max-sources` / `--max-bytes` cap the work per pass. The first changed
  source always proceeds, so a byte cap cannot wedge the backlog.
- `--max-source-bytes` defers any single file above the cap (default 256 MiB).
  The adapters parse a whole file in memory, and peak RSS runs several times the
  file size (a 305 MB Codex transcript peaked at 2.5 GB). For a `*.zstd` source
  the cap bounds the **decompressed** size: the reader stops at the cap instead
  of expanding a small compressed stream into unbounded memory.
- `--max-db-bytes` defers imports once the projection reaches the cap.
- Normalized JSONL is spooled to `DB.collector-spool/<source_id>.jsonl` and
  deleted right after each import. Imported events therefore carry the spool
  path as `source_path`. `collector_sources.source_id` maps it back to the
  original transcript's realpath.

Kill switch: `collector-switch --db DB off` creates `DB.collector-disabled`
(override the path with `--kill-switch`), and `OBSERVATORY_COLLECTOR_DISABLED=1`
does the same from the environment. `collect`, `collect --watch` (checked before
every pass) and `queue-drain` then do nothing and report `disabled`. The switch
never touches the city.

### Classification queue

`collector_queue` keeps one row per `(session, snapshot)`. A newer snapshot of
the same session marks the older pending row `superseded`, and re-queuing an
unchanged snapshot does nothing. `queue-drain` classifies due items through the
bounded M3 transport and **requires `--max-requests`** as the per-run spend
ceiling:

- `classified` → `done`, with the classification id.
- `budget_exhausted`, `circuit_open`, `credential_error` or `model_drift` → the
  drain stops and the item stays `pending` with no attempt charged. These say
  nothing about the subject.
- A request that cannot be built (a `RequestError`, including a byte-cap
  violation) → that item is charged an attempt and the drain continues with the
  next due item, so one unbuildable subject cannot wedge the queue.
- A transport error → the drain stops and the item is charged an attempt. A
  permanently broken transport therefore parks the item after
  `--max-item-attempts` instead of replaying it forever.
- Any other failure → it is retried after `--retry-backoff` seconds, doubling
  each time. After `--max-item-attempts` failures the item moves to `unknown`
  and keeps its last failure. Nothing is fabricated.

`queue-drain` sends **redacted transcript text** by default. The owner approved
transcript text to Jev on 2026-09-25 after the live projection showed that a
metadata-only state cannot express task intent: all 53 live classifications were
`primary_intent=unknown` because the state carried only event/tool counts. Pass
`--metadata-state` to opt back into the structured-metadata scope (event-kind
counts, tool invocation counts, command-category counts, models, duration and
bead/formula/parent flags). The text scope reuses the same bounded transport and
requires `--max-requests`:

- Every excerpt is run through the adapter credential redactor again (key
  assignments, bearer tokens, well-known token shapes, email addresses and
  home-directory paths), so re-sending already-redacted projection text cannot
  resurrect a secret.
- **Injected framework payloads are stripped before the state is built.** Gas
  City role prompts (`[city] <agent> • <time>`), runtime-context snapshots,
  `<system-reminder>` wake/deferred reminders and skill payloads ride on
  `role=user` records but carry no task provenance. `framework.py`
  (`FRAMEWORK_FILTER_VERSION`) drops a whole payload that starts as framework
  text and excises embedded reminder/skill blocks from an otherwise real
  message, so the classifier sees the work rather than the harness.
- Each remaining event's text is excerpted deterministically from the head at
  `DEFAULT_TEXT_EXCERPT_BYTES` (1536) UTF-8 bytes, with the dropped tail
  summarized by byte length and digest. The whole request is then fitted to the
  transport's 24 KB `REQUEST_BYTE_CAP` by dropping trailing excerpts.
- The stored request metadata records what happened under `text_mode`:
  `excerpt_bytes`, `excerpted_events`, `injected_events_dropped`,
  `framework_filter_version`, `excerpt_strategy` and `request_dropped_excerpts`.
- The subject snapshot is namespaced for text mode, so a text classification can
  never collide with the metadata classification of the same session. Metadata
  mode keeps the raw session snapshot, leaving existing classifications valid.
- `--include-done` also re-classifies already-`done` rows in the new data scope
  (the text-namespaced subject is stored separately; the old metadata
  classification is retained, never overwritten).
- `--sessions-file` restricts a drain to an explicit JSON list of session
  identities or `session:[...]` group keys, so a named sample can be
  re-classified without paying for unrelated queue rows.

`collect-status` reports coverage (`current / scoped`), per-provider status
counts, lagging sources with their lag and reason, queue counts, the oldest
pending age, the `unknown` items with their failures, and the last pass.

## 10. Accepted-task impact reports (M6)

`measurement.md` makes the work item / accepted task the primary unit and names
the ways a plausible-looking before/after report is wrong. `impact.py` turns an
explicit, versioned **impact bundle** plus optional projection evidence into a
deterministic report that keeps those failure modes visible.

The bundle (`schema_version` `"1.0"`) carries:

- `work_items`: the eligible assigned tasks. Each records pre-treatment
  covariates (`repo`, `task_class`, `scope`, `provider`, `model`, `harness`,
  `effort`, `host`, `baseline_complexity`, `workload`, `cache_state`,
  `concurrency`), `ready_at`/`accepted_at`/`acceptance_kind`, an `outcome`
  (`accepted`/`rejected`/`abandoned`/`in_progress`/`unknown`), a `cohort`
  (`treatment`/`control`/`unknown`), `attempts`, and `quality`.
- `attempts`: observed execution/review/fix/retry intervals (`phase` is
  `active`/`queue`/`idle`/`human_review`), nullable `usage` and `price`, and
  optional quality fields.
- `classifier_overhead`: request/token/latency/retry/cache-hits rows, optionally
  attributed to a work item.
- `evidence`: `randomized`/`assignment_logged` and `parallel_pre_trends`, which
  bound the strongest attribution grade the comparison can earn.

Accounting rules the report enforces:

- **Denominator.** Every eligible assigned task stays in; failed and abandoned
  runs lower the success rate instead of disappearing.
- **Cost.** Total measured attempt cost includes retries and reviews, not just
  the successful last attempt, and is divided by the accepted-task count. A
  missing price or missing usage makes the item's cost unknown; an explicit
  `cost_usd: 0`, or zero tokens with a known price, is a measured zero.
- **Zero accepted tasks** makes cost per accepted task undefined (with the
  reason recorded), never zero. An empty eligible set makes the success rate
  undefined too.
- **Censoring.** An incomplete task is right-censored and retained; its
  acceptance time is never recorded as zero. An open attempt has an unknown
  interval and is reported as such.
- **Time.** `time_to_accepted` is `accepted_at - ready_at`; active execution is
  the union of observed active intervals (queue/idle/human-review separately),
  and a global union plus a parallel-overlap figure keep concurrent work from
  being double-counted or summed as wall time.
- **Costs.** Classifier requests/tokens/latency are reported as their own
  overhead, alongside rather than inside task cost.
- **Comparisons.** Treatment and control are exact-matched on the pre-treatment
  covariates; matched units are the same set for every outcome. The report
  records cohort `n`, overlap, exclusions, covariate imbalance (total-variation
  distance), and a deterministic stratified-bootstrap interval for the matched
  difference. A zero baseline makes the relative change undefined.
- **Attribution.** The grade is `controlled` (logged/randomized assignment),
  `quasi_experimental` (parallel pre-trends), `matched_observational`,
  `descriptive` or `unmeasurable`; residual imbalance downgrades a strong grade.
  Semantic labels and chronology alone never establish causality, and the
  conclusion is worded as association unless the assignment supports more.

When only `--db` is given, the command emits the descriptive half that a real
projection can support (token totals, missing-vs-zero usage, duration
distribution, classifier overhead, collector backlog) and reports the
accepted-task section as unmeasurable rather than fabricating work items,
acceptance or cohorts. `reports/real-obsdb-20260922.json` is that report over a
read-only copy of the local projection.

## 11. Shadow orchestration recommendations (M7)

`requirements.md` R4 asks for orchestration recommendations that are **advisory
only**. `policy.py` maps validated classifications to the **current configured
catalog** of formulas/providers/skills/context bundles/test plans and compares
the result to the existing routing without changing it. The command is
`shadow`; it never writes routing, dispatch, configuration or execution state.
Two explicit, versioned inputs are required:

- `--catalog`: the current allowed candidate set. Each candidate has an
  immutable `candidate_id`, a `kind`, `enabled`, a `priority`, the
  `capabilities` it provides, and `constraints` (`intents`, `providers`,
  `repos`, `hosts`, `scopes`, `forbidden_flags`, `min_confidence`). The catalog's
  `defaults` map is the existing default policy per kind.
- `--input`: a bundle of validated classifications plus their **as-of**
  provenance. Each record carries `as_of` (the decision time), `observed_at`
  (when the classification was produced), the primary `intent`/`scope`, optional
  `probabilities`, `confidence`, `flags`, `required_capabilities`, `provider`,
  `repo`, `host`, named `features` (each with `available_at`), the current
  `current_routing` decision, and the future `outcome`/`outcome_observed_at`.

Recommendation rules:

- **Catalog-bounded.** Only a candidate present in the catalog and `enabled` can
  be named. An intent is served only by a candidate whose `constraints.intents`
  includes it; a `required_capabilities` entry must be provided by the
  candidate's `capabilities`; provider/repo/host/scope and `min_confidence`
  constraints are checked too. A disabled or failing candidate appears in
  `rejected_candidates` with its reason (`disabled`, `intent_mismatch`,
  `capability_mismatch`, `provider_mismatch`, ...); it is never substituted.
  Among allowed candidates the highest `priority` wins, ties break
  lexicographically, so replay is deterministic. **Only current allowed
  candidates are ever recommended.**
- **As-of / strict causality.** `as_of_features` includes only named features
  whose `available_at <= as_of`; later features are listed in
  `excluded_future_features`. The completion `outcome` is never a decision
  input. If the classification itself was observed after `as_of`, the record is
  not leak-free and every kind falls back with
  `as_of_before_classification`. The report's `shadow.decision_inputs_hash`
  covers only as-of inputs, so two records differing solely in a future outcome
  share it.
- **Abstention → fallback.** Injected, uncertain, contested, changed-intent and
  rare cases, `unknown`/missing intents, missing/low `confidence` (below
  `--confidence-threshold`), and — when configured — high entropy abstain with a
  named reason. An abstention or an empty allowed set falls back to the existing
  policy: the record's `current_routing` candidate, else the catalog default,
  recorded as `fallback_candidate` plus `fallback_path`
  (`current_routing`/`catalog_default`/`unavailable`). The fallback keeps the
  policy that would actually run even when it does not satisfy a newly declared
  capability; silently substituting another candidate would be an applied
  recommendation.
- **Shadow disagreement.** Each recommendation records `decision`
  (`recommended`/`agree`/`fallback`), `eligibility`
  (`eligible`/`abstained`/`ineligible`), `eligibility_reason`, `reason`,
  `confidence`, `uncertainty` (`1 - confidence`), `entropy_bits`,
  `current_candidate`, `current_candidate_allowed`, the selected and alternative
  candidates, and `disagreement`. The `shadow` block reports per-kind totals,
  `agreement_rate`, the explicit disagreement list,
  `fallback_reasons`, and `executes_changes: false`.

`shadow --db DB` persists every recommendation append-only in the
`recommendations` table, keyed by its content hash, with queryable eligibility,
confidence/uncertainty, recommended/current/fallback candidates, fallback path,
disagreement and leak-free columns. Replaying an identical run deduplicates; a
changed catalog or classification is retained as new, versioned evidence. The
report is deterministic (`report_hash` over the whole content) and the command
exits non-zero only on invalid input, never to signal a disagreement.

The M7 acceptance gate is the offline replay: `tests/test_policy.py` covers the
catalog/capability constraints, the as-of/no-leak cases, injection and
uncertainty abstention, fallback paths, disagreement reporting, deterministic
replay and persistence, while `tests/fixtures/policy/` provides a reusable
catalog plus shadow bundle.

## 12. Controlled reversible policy canary (M8)

`requirements.md` R4 requires every enabled policy version to carry exposure
logs, budget limits, stop conditions and a reversible rollback.
`canary.py` turns one M7 shadow disagreement into a bounded, pre-registered
experiment. It is deliberately conservative:

- **Opt-in; the prior policy is the default.** `canary` only applies a
  treatment when `--enable` is passed *and* the kill switch is not engaged.
  Without `--enable` (or with a present `--kill-switch` file) every unit keeps
  the prior policy; the planned assignment is still recorded for audit.
- **Pre-registered before the run.** `canary-register --input SPEC.json`
  validates and freezes the policy, treatment/prior candidate, seed, sample
  size, guardrails and request cap into a registration artifact. The canary
  run consumes that artifact; `registration_hash` ties the report to it. The
  registration's `evidence` block carries the M7 disagreement rows that
  justify the selected policy.
- **Seeded, balanced, matched assignment.** Each unit is assigned by a sha256
  over `(seed, stratum, unit_id)` and permuted inside its stratum, so control
  and treatment are balanced without a random-number generator and a stratum
  of two keeps both arms. The seed and every per-unit `assignment_hash` are
  logged.
- **Bounded spend.** `--max-requests` caps live Jev classification requests per
  run; the owner-approved hard ceiling is `MAX_ALLOWED_REQUESTS` (50) and the
  run never exceeds the registration cap. Units beyond the cap stay on the
  prior policy and are listed as `deferred_units`.
- **Tested kill switch.** Engaging the switch restores the prior policy for
  every unit on the next run without rewriting the planned assignment.
- **No unsupported claim.** The report carries exposure, control/treatment
  quality, cost, acceptance and the primary-outcome net effect with an
  uncertainty interval. `improvement_claim` is `none` unless the
  pre-registered sample size and every guardrail pass and the interval excludes
  zero in the beneficial direction; a missing outcome is unknown, never zero.

A unit is eligible only when the M7 recommender returns the registered
treatment candidate for the policy kind and the classification is usable
(known intent, not injected/uncertain/contested/rare, confidence at or above
the registration threshold, and within the request cap). Ineligible units stay
on the prior policy.

The canary is advisory evidence. `canary.executes_changes` is always `false`:
the module writes no routing, dispatch or configuration. Applying a treatment
inside live fleet workflows is a separate, owner-authorized integration and is
out of scope for this slice.

## 13. Read-only bead-store (Dolt) adapter

`adapters/beads.py` reads the Gas City bead store held in Dolt without going
through `collect`: a Dolt database is one logical source but many sessions, so it
cannot be discovered from the filesystem like a transcript. The adapter shells
out to `gc dolt sql` in the city directory (`--city` defaults to
`/home/coolhenrylinux/city`, database `gl`), reads the CSV result as a stream and
never issues a writing statement. `assert_read_only` refuses anything that is not
`USE`/`SELECT`, and `normalize_ref` validates the interpolated ref against a
conservative Dolt-ref character set, so a ref can neither inject SQL nor widen
the query.

Mapping into the import contract:

- **one bead -> one episode/session** (`session_id = "<bead-id>@<ref>"`), so the
  store segments exactly one episode per bead;
- the episode body is the bead `title`, `description` and `notes` plus every
  `comment` in time order, each emitted as a `note` event (the only non-tool kind
  the text state carries);
- `status`, `assignee`, `priority`, `issue_type` and labels ride on the first
  event as a `[beads] status=... assignee=... labels=...` header, so Jev sees the
  bead's state as well as its prose;
- every text value passes through `adapters/redaction.py` (`redact_and_bound`)
  before it can reach disk or the wire; credentials, bearer tokens, email
  addresses and home-directory paths are removed first.

Two scopes are supported with the same reader:

- `main` — the live branch;
- `remotes/origin/main` — the pre-compaction remote-tracking ref, read locally
  without a fetch.

```python
from agent_observatory.adapters import (
    AdapterContext, read_beads, fetch_bead_fields, diff_bead_snapshots,
)
from agent_observatory.inventory import records_to_jsonl

context = AdapterContext(city_id="coolhenry", host_id="ryzen", repo="gascity")
result = read_beads("remotes/origin/main", context=context, max_bytes=256 * 1024 * 1024)
open("beads-remote.jsonl", "w", encoding="utf-8").write(records_to_jsonl(result.records))

# Field-level ref diff (bead ids, field names and differing metadata values only).
local = fetch_bead_fields("main")
remote = fetch_bead_fields("remotes/origin/main")
for item in diff_bead_snapshots(local, remote):
    print(item["bead_id"], item["change"], item["changed_fields"])
```

`title`, `description` and `notes` are read **as of the selected ref**: they are
snapshots, not an edit history, so an intermediate edit the ref no longer carries
is not recoverable from the diff. Field values are redacted and bounded by the
same redactor the event path uses before they enter the snapshot map, so a
secret in a description or notes can never appear in the diff output.

`--max-source-bytes` is honoured the way the file adapters honour it: the
subprocess stdout is read in chunks and the read is aborted (and the child
killed) the moment the logical source passes the cap, so a small query cannot
materialize an unbounded result. `BeadsAdapter` is registered in
`adapters/__init__.py` as a directly addressable provider (`ADAPTERS["beads"]`,
`SUPPORTED_PROVIDERS`) but is deliberately kept out of `_ORDERED_ADAPTERS`, so
file discovery and `adapter_for_path` are unchanged. Tests in
`tests/adapters/test_beads_adapter.py` drive it entirely from a synthetic CSV
fixture and an injected runner; no live Dolt server is needed in CI.

## CLI reference

```
agent-observatory import-jsonl --db DB FILE [FILE ...]
agent-observatory report --db DB [--out FILE]
agent-observatory build-request --state STATE.json
    [--db DB] [--subject-kind event|session]
    [--session '["city","host","provider","session"]' | city|host|provider|session]
    [--event-id ID] [--snapshot-hash HASH] [--taxonomy PATH] [--out FILE]
agent-observatory import-response --db DB --response RESP.json --request-hash HASH
agent-observatory classify --db DB --state STATE.json
    [--subject-kind event|session]
    [--session '["city","host","provider","session"]' | city|host|provider|session]
    [--event-id ID] [--snapshot-hash HASH] [--taxonomy PATH]
    [--config CONFIG.json] [--timeout SECONDS] [--max-attempts N]
    [--max-requests N] [--max-tokens N] [--max-cost-usd USD]
    [--price-per-million-input-usd USD] [--price-per-million-output-usd USD]
    [--allow-model-drift] [--out FILE]
agent-observatory episodes --db DB [--out FILE]
agent-observatory annotate --db DB --gold GOLD.json [--taxonomy PATH]
agent-observatory evaluate --gold GOLD.json --predictions PRED.json
    [--taxonomy PATH] [--primary-facet FACET]
    [--multi-label-facet FACET ...]
    [--holdout-fraction F] [--confidence-threshold C]
    [--min-class-support N] [--calibration-bins N] [--out FILE]
agent-observatory changes-sync --db DB --input BUNDLE.json
agent-observatory exposure --db DB [--out FILE]
agent-observatory collect --db DB --root DIR [--root DIR ...] --city CITY --host HOST
    [--repo REPO] [--provider P] [--debounce-seconds S] [--max-debounce-seconds S]
    [--max-sources N] [--max-bytes N] [--max-source-bytes N] [--max-db-bytes N]
    [--kill-switch PATH] [--spool-dir DIR] [--no-enqueue]
    [--watch [--interval S] [--iterations N]]
agent-observatory collect-status --db DB [--kill-switch PATH] [--out FILE]
agent-observatory collector-switch --db DB on|off [--kill-switch PATH]
agent-observatory queue-drain --db DB --max-requests N [--max-items N]
    [--metadata-state] [--include-done] [--sessions-file SESSIONS.json]
    [--max-item-attempts N] [--retry-backoff S] [--kill-switch PATH]
    [transport options as for classify] [--out FILE]
agent-observatory silver-build --candidates CANDIDATES.csv --db DB
    --out-gold GOLD.json --out-report REPORT.json
    [--sample-size N] [--seed S] [--judge JUDGE ...] [--judges-file JUDGES.json]
    [--jev-predictions-out FILE]
    [--base-url URL] [--api-key-env ENV] [--max-judge-requests N]
    [--taxonomy PATH] [--excerpt-bytes N] [--text-bytes N] [--judge-text-bytes N]
    [--gold-set-version V] [--prompt-version V] [--judge-timeout S]
    [--cli-judge-timeout S] [--tolerate-judge-failures]
agent-observatory silver-evaluate --gold GOLD.json --predictions PRED.json
    [--taxonomy PATH] [--full-evaluator] [--out FILE]
agent-observatory silver-collapse (--checkpoint CKPT.json | --report REPORT.json)
    --jev-predictions PRED.json --out FILE
    [--collapse ID] [--judge JUDGE ...] [--majority N] [--taxonomy PATH]
    [--require-clear]
agent-observatory silver-two-stage (--checkpoint CKPT.json |
    --stage1-checkpoint S1.json [--stage2-checkpoint S2.json])
    --jev-predictions PRED.json --out FILE
    [--judge JUDGE ...] [--majority N] [--taxonomy PATH] [--require-clear]
agent-observatory silver-two-stage-run --episodes-file EPISODES.json
    --out-report REPORT.json [--jev-predictions PRED.json]
    [--stage1-checkpoint S1.json] [--stage2-checkpoint S2.json]
    [--judge JUDGE ...] [--judges-file JUDGES.json] [--majority N]
    [--evidence-prompt-version V] [--intent-prompt-version V]
    [--pair-prompt-version V] [--taxonomy PATH]
    [--base-url URL] [--api-key-env ENV] [--max-judge-requests N]
    [--judge-timeout S] [--cli-judge-timeout S] [--judge-text-bytes N]
    [--tolerate-judge-failures] [--require-clear]
agent-observatory impact [--input BUNDLE.json] [--db DB]
    [--primary-outcome OUTCOME] [--bootstrap-resamples N] [--out FILE]
agent-observatory shadow --catalog CATALOG.json --input BUNDLE.json
    [--db DB] [--confidence-threshold C] [--out FILE]
agent-observatory canary-register --input SPEC.json [--out REGISTRATION.json]
agent-observatory canary --registration REGISTRATION.json --catalog CATALOG.json
    --input BUNDLE.json [--enable] [--kill-switch PATH] [--max-requests N] [--out FILE]
agent-observatory --version
```

`episodes` emits deterministic annotation candidates from the projection;
`annotate` validates a gold set and appends it (append-only) to the projection;
`evaluate` writes the deterministic evaluation report and exits non-zero when the
split audit is not leak-free; `impact` writes the deterministic accepted-task
impact report (at least one of `--input`/`--db` is required). `silver-build`
writes the multi-judge silver gold set plus its agreement report (and optional Jev
predictions read from the projection); `silver-evaluate` writes the
Jev-vs-reference report; `silver-collapse` re-scores a recorded judge checkpoint
or silver report under the collapsed taxonomy (no judge call) and writes the
collapse report; `silver-two-stage` scores (or approximates) the two-stage
abstention split with no judge call and `silver-two-stage-run` drives the live
two-stage judge pass. `queue-drain` sends redacted transcript text unless
`--metadata-state` is passed.

When `--snapshot-hash` is supplied together with `--db` and `--session`, the
explicit value is cross-checked against the projection and a mismatch is refused
rather than classifying a phantom subject.

A missing or unreadable input/output path is reported on stderr as
`error: <path>: <reason>` with exit status 1, never a traceback. A local SQLite
failure (for example `--db /dev/null`) is reported the same way.

## Tests

```bash
python3 -m unittest discover -s contrib/agent-observatory/tests -v
```

The suite covers duplicate replay, same-session different providers/hosts and
delimiter-colliding identities, record-level conflict skips, truncated input and
LF-only line numbering (including records whose text contains
U+2028/U+2029/U+0085), sessions `first_timestamp` as a running minimum, the
classification UNIQUE race, nonzero vs unknown exit codes, invocation/result
pairing (including that a result call id can never pair with an invocation's
fallback event id), orphan and duplicate and conflicting results, request-only
vs directly observed outcomes, unnamed-tool invocations counted under `unknown`,
tokens missing
vs zero, timezone-aware timestamp normalization and chronological ordering across
offsets and fractional precision, the request byte cap, the real lowercase
Jev question/answer wire shape and unknown-answer-key rejection, the
missing-input-file CLI error contract, payload-hash identity exclusion, ambiguous
commands, instruction-like text treated as data, question-hash sensitivity to
instructions/criteria, and invalid/NaN responses. `examples/demo.sh` runs the CLI
end to end against the synthetic fixture; replay preserves event and
classification counts.

The evaluation tests add: v2 facet parsing/cardinality/hash stability, episode
segmentation (boundaries, work-anchor splitting, stable collision-free ids,
parent-lineage grouping, deterministic ordering), gold validation (unknown
labels, wrong cardinality, unknown flags, duplicate ids, taxonomy mismatch, naive
timestamps), append-only annotation storage distinct from predictions, exact
metric arithmetic, grouped temporal split and leak-free audit (including a
detected group-overlap violation), the automation gate (injected/uncertain/rare/
low-confidence/unknown never eligible), report reproducibility, and the
`evaluate` CLI.

The impact tests add: bundle validation/round-tripping, the known-effect /
no-effect / confounded replay fixtures, zero-accepted and empty-cohort semantics,
right-censoring and terminal runs in the denominator, missing-price vs
missing-usage vs explicit measured zero, zero-baseline relative change, interval
unioning and parallel-overlap, classifier-overhead rates and attribution, grade
assignment/downgrade, projection evidence with missing-vs-zero usage, and the
`impact` CLI.

The shadow-policy tests add: catalog and bundle validation, enabled/capability/
intent/provider/scope constraints, deterministic priority selection, the as-of
exclusion of future features and the future completion label, fallback to the
existing policy, injection/uncertainty/low-confidence/entropy abstention,
disagreement reporting, byte-stable replay, append-only content-hash
persistence, and the `shadow` CLI (including its clean-error contract).

The canary tests add: registration validation (unknown keys, bad schema,
policy kind, fraction, cap ceiling, outcome, sample size, naive timestamp),
bundle validation (duplicates, unknown keys, bad outcome types), seeded and
balanced assignment determinism, the opt-in prior-policy default, the tested
kill switch, the request cap (deferral, zero budget, registration clamp and
owner-ceiling refusal), eligibility (unknown/low-confidence/injected/wrong
candidate), exposure and guardrail reporting, effect measurement with and
without a supported claim, missing outcomes as unknown, and the
`canary-register`/`canary` CLI (including the clean-error contract).

## Limitations and next adapter requirements

- Import adapters for real provider formats (codex, claude, dsh) are **not**
  implemented; callers must supply already-normalized records. Each adapter must
  map provider events into this contract, compute stable event ids, and keep raw
  text out of default storage.
- Session snapshots grow with session length; large sessions may need chunked
  subjects before the 24 KB request cap matters.
- The byte cap approximates wire size, not model tokens; a tokenizer-aware cap is
  future work.
- Live transport implements bounded retries, rate limiting via budgets, and cost
  accounting (see section 6), but there is no provider-side rate-limit discovery
  beyond `Retry-After`, and the token/dollar ceilings can overshoot by at most
  one request because usage is only known after a response.
- Classification is append-only by key; re-labelling with a new taxonomy or
  model version is how labels evolve.
- The projection has no acceptance, work-link or cohort table yet, so the
  accepted-task impact section reports `unmeasurable` for real projection-only
  input. Supplying the explicit impact bundle is what makes a matched cohort
  measurable; an evidence-backed work-link/acceptance import is the next step.
  Classifier prices are also unset by default, so observed classifier cost stays
  unknown until a price is configured.
- The M8 canary is advisory: it assigns treatment, enforces the request cap and
  emits exposure/guardrail/net-effect evidence, but it does not apply a
  treatment inside live fleet routing. Wiring the accepted treatment into the
  fleet workflow is a separate owner-authorized integration.
- Scope docs are maintained separately by the Mayor in
  `city/plans/jev-agent-observatory`.
