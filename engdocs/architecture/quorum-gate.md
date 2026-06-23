# Quorum Gate for Formula Steps

A quorum gate opens when a fixed number of dynamically-bonded children close,
rather than requiring all children (like `all-children`) or just one
(like `any-children`). It is set on a step's `waits_for` field using the
`quorum(N)` syntax.

## Use Case

The canonical example is consensus or peer-review workflows where you want a
majority decision without waiting for every participant:

```toml
[[steps]]
id      = "review"
title   = "Peer review — {{feature}}"
# agents bond children here at runtime via on_complete

[[steps]]
id         = "merge"
title      = "Merge approved work"
needs      = ["review"]
waits_for  = "quorum(2)"   # unblock once any 2 review children close
```

When three reviewers are dispatched as children of `review`, the `merge` step
unblocks as soon as the second one closes — the third can still finish, but it
no longer holds up the workflow.

## Syntax

```
waits_for = "quorum(N)"
```

`N` must be a positive integer (`N >= 1`). Gas City rejects the formula at
validation time if `N` is zero, negative, or non-numeric.

## How It Works

1. At compile time, `waits_for: "quorum(3)"` emits the label `gate:quorum(3)` on
   the step's bead and records a `waits-for` dependency with metadata
   `{"gate": "quorum(3)"}`.
2. At runtime, when a child bead closes, the runtime checks the open count
   against the quorum threshold. When the count reaches N, the gate bead
   closes and the downstream step is released.
3. Children that close after the quorum is reached are not blocked — they
   continue to completion normally, but the parent workflow no longer waits
   for them.

## Comparison with Other Gate Types

| `waits_for` value | Gate opens when…                          |
|---|---|
| `all-children`    | every dynamically-bonded child closes     |
| `any-children`    | the first child closes                    |
| `quorum(N)`       | the N-th child closes (N must be >= 1)    |
| `children-of(id)` | all children of the named step close      |

## Validation Rules

- `N` must be a positive integer: `quorum(1)` through `quorum(999)` are valid.
- `quorum(0)`, `quorum(-1)`, `quorum()`, and `quorum(abc)` are rejected at
  `bd cook` / `bd pour` time with a descriptive error.
- The quorum threshold is static — it is baked into the formula, not evaluated
  at runtime. Use a formula variable if you need a configurable threshold:

  ```toml
  [vars]
  min_approvals = "2"

  [[steps]]
  id        = "gate"
  waits_for = "quorum({{min_approvals}})"
  ```

## Implementation

| Artifact | Purpose |
|---|---|
| `internal/formula/types.go` | `WaitsForSpec.Quorum` field, `ParseWaitsFor` quorum branch, `validateWaitsFor` quorum branch |
| `internal/formula/parser_test.go` | `TestParseWaitsForQuorum`, `TestValidate_WaitsForQuorum` |

`ParseWaitsFor("quorum(3)")` returns `&WaitsForSpec{Gate: "quorum(3)", Quorum: 3}`.
The `Gate` string is used verbatim as the label suffix and dep metadata value,
so no changes to `compile.go` are required.
