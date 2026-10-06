# Concurrent store panic tie-break regression

**Goal:** Keep the documented rule that, if several concurrent store-read workers panic, the lowest panicking index is the panic reported to the caller. Source: reviewer finding `ga-emvm9l` from `ga-0tsfr7`.

## Why this matters

The reviewed concurrent store-read change documents and implements the rule, but its two panic tests each have only one panicking worker. The reviewer changed the implementation to choose the highest index and all nine diff-owned tests and the focused selection still passed. The user impact is low because either panic denotes a bug, but the result is a documented diagnostic contract and its first-error-by-position sibling is tested.

The reviewed implementation is commit `8e2a42fe7223e1017b268d1f8eb7ac96a0f98654`. It was not on `origin/main` at the time of PM intake on 2026-10-06. Its review vehicle is `ga-0tsfr7`; the validator must verify landing before basing the regression test on the change.

## Work package and dependency

| Bead | Route | Acceptance |
| --- | --- | --- |
| `ga-99hxfi` | validator (`needs-tests`) | A deterministic multi-panic cmd/gc test passes on the reviewed implementation, fails when selection is changed to highest index or completion order, and leaves production code unchanged. |

This is one test-only package, so splitting it further would create overlapping validator work. It traces to `ga-emvm9l` and is blocked by the original review bead `ga-0tsfr7`. Review closure is only an initial gate: the validator must confirm the implementation is actually on `origin/main` before landing its test. If deployment creates a separate bead, carry that gate forward before work begins.

## Scope

No new behavior, design, or production implementation is requested. The test should exercise more than one panicking worker and verify both the recovered `workerPanic` value and its rendered panic text. The reviewer’s mutation evidence and existing test names are recorded on `ga-emvm9l`.
