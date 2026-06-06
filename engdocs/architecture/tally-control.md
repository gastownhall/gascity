# Tally Control Step

The `tally` control step aggregates vote outputs from voter fragments spawned
by an `on_complete` fanout, reducing N parallel results into a single
pass/fail outcome.

## Formula syntax

```toml
[[steps]]
id    = "ask"
title = "Collect votes"

[steps.on_complete]
for_each = "output.voters"
bond     = "mol-voter"

[steps.tally]
vote_field = "answer"       # dot-separated path into voter's gc.output_json
mode       = "majority"     # majority | unanimous | any-pass
```

`tally` requires `on_complete` to be set on the same step. Validation rejects
a `tally` block without `on_complete`.

### `mode` values

| Mode | Pass condition |
|------|----------------|
| `majority` (default) | Most common vote value has > 50 % share |
| `unanimous` | Every voter produces the same value |
| `any-pass` | At least one voter's `gc.outcome` is `"pass"` |

### `vote_field`

Dot-separated JSON path into each voter's `gc.output_json` (e.g. `answer`,
`result.verdict`). When empty, the raw `gc.outcome` string (`"pass"` /
`"fail"`) of each voter bead is used.

## Graph injection

`ApplyGraphControls` injects two control steps for any step that has both
`on_complete` and `tally`:

```
ask ──► ask-fanout ──► ask-tally
                           │
                       downstream steps
```

1. `{step.ID}-fanout` — existing fanout control (unchanged)
2. `{step.ID}-tally` — new tally control, `Needs: [step.ID+"-fanout"]`

All downstream `Needs` / `DependsOn` refs to `step.ID` are rewritten to
`step.ID + "-tally"`, so downstream work waits for the tally outcome rather
than the raw source step.

## Runtime semantics

`processTallyControl` in `internal/dispatch/tally.go`:

1. Looks up the fanout bead via `gc.control_for` + `-fanout` step ref.
2. Lists the fanout's `"blocks"` deps — these are the voter sink beads.
3. Calls `extractVote` on each voter to read the vote value.
4. Calls `tallyVotes` with the collected votes and the configured mode.
5. Writes `gc.tally_result` (the winning value or a diagnostic string) and
   `gc.outcome` (`"pass"` / `"fail"`) to the tally bead metadata.
6. Closes the tally bead.

The tally bead is only reachable after its fanout bead closes, which itself
waits for all voter fragments. The voter beads must set `gc.output_json`
before closing when `vote_field` is non-empty.

## Implementation index

| Component | Location |
|-----------|----------|
| `TallySpec` struct, `Step.Tally` field, validation | `internal/formula/types.go` |
| Tally control injection, ref rewriting | `internal/formula/graph.go` |
| `processTallyControl`, `extractVote`, `tallyVotes` | `internal/dispatch/tally.go` |
| `ProcessControl` switch entry | `internal/dispatch/runtime.go` |
| Graph injection tests | `internal/formula/graph_test.go` |
| Dispatch unit + integration tests | `internal/dispatch/tally_test.go` |
