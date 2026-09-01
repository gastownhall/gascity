---
title: Gas City Identity Separator Contract — v1
description: Authoritative specification for the qualified-identity encoding shared by gascity and beads.
---

| Field | Value |
|---|---|
| Status | Authoritative specification |
| Last verified | 2026-08-31 |
| Primary implementation | `internal/agent/session_name.go` |
| Mints identities | gascity |
| Compares identities | beads |
| Concept model | [How Gas City Works](/getting-started/how-gas-city-works) — the Agent (WHO) primitive |

## What this is

gascity mints **qualified agent identities** — `rig/agent` and `city.agent`
combinations — and renders them into tmux-safe session name strings. beads
stores and compares those same strings in its own records (session-name
metadata, routing fields) without minting them itself. Two independent
codebases have to agree, byte for byte, on how the structural separators `/`
and `.` get encoded, or a qualified identity that round-trips through beads
and back can silently turn into a different identity than the one gascity
minted.

This contract exists so that agreement is written down once, instead of
re-derived by reading `internal/agent/session_name.go` from two different
repos and hoping both readings match.

## 1. The separator alphabet

A qualified identity is built from name segments joined by structural
separators. Exactly two characters are reserved as **positional separators**
in a raw (unencoded) qualified identity:

- `/` — the rig/agent boundary (`rig/agent`)
- `.` — the city/agent boundary on an imported identity (`city.agent`)

These two characters are always positional, never part of a name segment. A
literal `-` or `_`, by contrast, is an ordinary character a name segment MAY
contain: gascity's own agent-name validation permits both
(`internal/config/config.go:27`, enforced at `:4015` — a name must match
`[a-zA-Z0-9][a-zA-Z0-9_-]*`, so `hello-world` and `builder-1` are valid agent
names in their own right, not `/`-delimited pairs).

Encoding (§2) represents each positional separator as a doubled pair rather
than touching `-` or `_` on their own: a literal `-`, doubled, stands in for
`/` (as `--`); a literal `_`, doubled, stands in for `.` (as `__`). A single
`-` or single `_` is never itself a positional separator — only the exact
two-byte sequences `--` and `__` carry structural meaning under this
contract.

## 2. The two-axis encoding table

| Structural meaning | Raw character | Encoded form |
|---|---|---|
| Rig/agent boundary | `/` | `--` |
| City/agent boundary (imported identity) | `.` | `__` |

The two axes are kept distinct on purpose: `/` and `.` encode to different
two-character sequences (`--` vs `__`) so that the two boundaries stay
separable. A canonicalizer implementing this rule MUST NOT collapse `--` and
`__` to the same normal form — doing so can silently merge two distinct
minted identities into one. For example, the encoded names
`hello-world--polecat` and `hello_world__polecat` both reduce to
`hello_world_polecat` under a canonicalizer that collapses any run of `.`,
`_`, or `-` to a single `_`, even though they decode to the structurally
distinct identities `hello-world/polecat` and `hello_world.polecat`.

**Decoding is best-effort, not lossless.** A name segment may itself contain
`--` or `__`: `builder--1` is a legal agent name under the §1 regex, so
`rig/builder--1` encodes to `rig--builder--1` and decodes back to the
*different* identity `rig/builder/1`. The encode direction is total; the
decode direction guesses. Do not build an exact round-trip on this table —
treat an encoded session name as a display and lookup key, and carry the raw
qualified identity alongside it wherever you need to recover it exactly.

## 3. Who mints, who compares

**gascity** is the only side that **mints** qualified identities. It
composes `rig/agent` and `city.agent` strings when constructing an agent's
qualified name, and it is the only codebase that calls the encode direction
(§4) to produce a tmux-safe session name from one.

**beads** never mints and exposes no general-purpose equivalent of gascity's
encode or best-effort decode functions. With gascity's
`BD_CURRENT_VERSION` from `deps.env` (currently v1.1.1-0.20260831020517-d530cddfa64b),
beads instead uses `issueops.ActorMatches` (beads repo,
`internal/storage/issueops/identity.go`, backed by the unexported
`canonicalActor`; package-local-duplicated as
`validation.ActorMatches`/`CanonicalActor` in `internal/validation/issue.go`)
for assignee/actor comparison. That comparison-only canonicalizer recognizes
both axes: an exact `--` run becomes `/`, a raw `/` passes through unchanged,
and every other run of `.`, `_`, or `-` (including `__`) collapses to `_`.
Consequently, `gastown--mayor` compares equal to `gastown/mayor` but remains
distinct from `gastown__mayor`; the slash and dot axes no longer widen to the
same actor identity.

This is deliberately not a claim of lossless decoding. The ambiguity in §2
still applies to legal name segments containing `--` or `__`; beads only uses
the rule to compare actor spellings. Its pinned source also carries a parity
test between the storage and validation copies so their separator behavior
cannot drift silently.

## 4. Source of truth

The tables above describe the code; they do not replace it. The
authoritative implementation is `internal/agent/session_name.go`:

- `sessionNameQualifiedReplacer` (`internal/agent/session_name.go:16-19`) —
  the encode direction (§2, raw → encoded).
- `sessionNameQualifiedReverseReplacer` (`internal/agent/session_name.go:21-24`) —
  the **best-effort** decode direction (§2, encoded → raw); see the
  decoding caveat in §2.

If this document and `internal/agent/session_name.go` ever disagree, the
code wins. File a correction against this doc rather than reimplementing
the table anywhere else.

## 5. Worked examples

| Qualified identity | Encoded session name |
|---|---|
| `mayor` | `mayor` |
| `hello-world/polecat` | `hello-world--polecat` |
| `gastown.mayor` | `gastown__mayor` |
