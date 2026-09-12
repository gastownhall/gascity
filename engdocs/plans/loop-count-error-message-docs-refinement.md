# loop.count Error Message and Docs Refinement Plan

Owner: `gascity/pm`
Created: 2026-06-24
Root bead: `ga-wnuc8r`
Source: designer handoff for #3705, following the closed broad package `ga-sdv68f`

## Goal

Tighten the completed loop.count error UX so formula authors understand both
the integer-literal constraint and the brace syntax difference between template
placeholders and range variables.

The prior PM package `ga-sdv68f` already delivered the broad parser error,
test, and docs note. This follow-up captures the new designer review items:
the single-brace/double-brace contrast, the stable `1..{n}` test anchor, and a
same-page docs cross-reference to range loops.

Tracker import was a no-op in this session because no visible tracker import
helper was installed.

## Children

| ID | Target | Purpose | Depends on |
| --- | --- | --- | --- |
| `ga-wnuc8r.1` | `gascity/builder` | Verify loop.count string errors pin the single-brace range example. | - |
| `ga-wnuc8r.2` | `gascity/builder` | Update the loop.count parse error to explain integer literals and brace syntax. | `ga-wnuc8r.1` |
| `ga-wnuc8r.3` | `gascity/builder` | Add a docs cross-reference from the count note to range loops. | `ga-wnuc8r.2` |

All child beads are labeled `ready-to-build` and `source:actual-pm`, with
`gc.routed_to` set to `gascity/builder`.

## Acceptance Rollup

The package is complete when:

- A regression test for `count = "{{cups}}"` asserts the error includes
  `integer literal`, `range`, and `1..{n}` without locking the entire prose.
- The parse error tells users that `loop.count` must be an integer literal,
  rejects quoted strings for count, and points to
  `range = "1..{n}"` with `var = "n"` for variable-driven iteration.
- The error explicitly contrasts single-brace `{n}` range variables with
  double-brace `{{n}}` template placeholders.
- Valid integer counts continue to parse unchanged.
- `docs/tutorials/05-formulas.md` keeps the existing Mintlify `<Note>` near the
  count example and adds a same-page link to the real Range loops heading.

## Dependency Graph

```text
ga-wnuc8r.1
  -> ga-wnuc8r.2
      -> ga-wnuc8r.3
```

The sequence preserves the local TDD preference: test anchor first, behavior
second, docs after final wording is available.

## Out of Scope

- Do not change `LoopSpec.Count`.
- Do not change the TOML alias count field type.
- Do not change loop validation sentinel behavior.
- Do not add OpenAPI, schema, or formula runtime behavior changes.
- Do not route this back to design; the design handoff is complete.
