---
title: "Assignee Routing Gate & Doctor Visibility"
---

| Field | Value |
|---|---|
| Status | Proposed |
| Date | 2026-09-16 |
| Author(s) | Claude |
| Issue | — |
| Supersedes | — |

An assignee is a routing instruction, but nothing has ever checked that it
routes anywhere. This doc covers the gate that makes an unroutable assignee a
write-time error, the shapes it must refuse without refusing legitimate ones,
and the two places `gc doctor` was reporting health it had not established.

## Problem

`bd update <id> --assignee <name>` accepts any string. Nothing resolves that
string against the agents and named sessions the city actually declares. A
typo, a renamed agent, or a target that was never configured produces a bead
that looks owned and is never picked up by anything.

The scheduler cannot catch it afterwards. `buildDesiredState` walks
`namedSpecs` and asks, for each configured spec, which beads are assigned to
it. A bead whose assignee matches no spec is simply never visited: it
contributes no demand, and it raises no error. The question is only ever asked
in the direction that cannot expose the gap.

`gc doctor` did not close it either, and in two distinct ways:

1. It had two different notions of "unfinished work" — one used by the
   reconciler report and one used by the assignee report — so a bead could be
   counted as live by one and finished by the other.
2. A config that failed to load at all produced a doctor run that reported on
   what it had managed to read, rather than reporting that it had not read the
   config. An unreadable source rendering as a clean result reads as health.

## Design

### The gate

A write-time check on the `gc bd` seam. `checkBdAssigneeArgs` extracts every
assignee value from the argument vector, resolves each against a roster built
from the live config, and fails the command when any value resolves to nothing:

```
assignee "gascity-wroker" matches no configured agent or named session, so
nothing would ever pick this work up.
  Assign to a configured target, add the missing one to city.toml, or set
  GC_ALLOW_UNRESOLVED_ASSIGNEE=1 to write it anyway
```

Three properties matter more than the check itself.

**It fails open when it cannot know.** If the roster comes back empty — no
agents and no named sessions resolved from config — the gate warns and permits
the write. An empty roster means the gate has no basis to judge, not that every
assignee is wrong. Refusing on no information would make an unrelated config
problem look like a bad assignee.

**It has a named escape hatch.** `GC_ALLOW_UNRESOLVED_ASSIGNEE=1` writes
anyway. Legitimate flows assign to targets that do not exist yet, and a gate
with no documented way past it gets bypassed in ways nobody can audit.

**It anchors on the actual subcommand, and it runs first.** Assignees arrive
four ways: `--assignee x`, `-a x`, `--assignee=x`/`-a=x`, the attached short
form `-aX`, and the second operand of `bd assign <id> <who>`. The flag forms
are scoped by `bdByIDSubcommand`, which walks bd's *global* flag grammar to
find the token that must be the subcommand, then resolves it — this is the
same subcommand locator the by-ID door uses, so the two mechanisms cannot
disagree about where the verb sits. The positional form is found separately by
`positionalAssignArg`, which locates the literal token `assign` at that same
index (via `firstBdSubcommandCandidateIndex`) rather than by taking the first
non-flag argument as the subcommand, and must still tell that occurrence apart
from the same word appearing as a flag *value* (`bd list --label assign foo
bar`). The token immediately before `assign` only donates it as a value when
that token is itself a value-consuming global flag — checking merely "does the
preceding token start with a dash" is wrong, because bd's global *boolean*
flags (`--json`, `-q`, `--global`, ...) take no value and sit directly before
the verb, so `bd --json assign` and `bd -q assign` both reach the real
subcommand. The gate also steps over valued flags placed after the subcommand
(including `--format`) so their values are not mistaken for operands. Both the
value-flag and boolean-flag manifests are sourced from `internal/bdflags`
rather than hand-copied, so they cannot silently drift from what `bd` actually
accepts. Where a spelling is still unrecognized the result is a missed check
rather than a refused write, which is the right way round: a false refusal on
an unrelated command teaches operators to bypass the gate.

**It normalizes bd's subcommand aliases before resolving them.** `bd`
registers seven alias pairs (each discoverable via `bd <verb> --help`'s
"Aliases:" section). Four are in scope here: `create`/`new`, `close`/`done`,
`show`/`view`, and `mol`/`protomolecule`. The other three --
`find-duplicates`/`find-dups`, `heartbeat`/`hb`, and `status`/`stats` -- are
deliberately absent from `bdSubcommandAliases`: `bdflags` keys none of their
canonical verbs, and none of the three accepts `--assignee`, so normalizing
them would resolve nothing and gate nothing. The count is from sweeping all
118 top-level verbs of bd 1.3.0-rc.1 for an "Aliases:" section, not from
reading back the entries the table already carried. `bdflags` keys every
manifest under the canonical verb only and
performs no alias normalization itself. A reader that resolves a verb via
`bdflags.Known` without normalizing first silently drops the alias — and
`create` is the feature's primary write verb, so a bare `bd new --assignee
ghost` would otherwise walk straight past the gate. `bdNormalizeSubcommandAlias`
is the one place this normalization happens, and both `bdByIDSubcommand` (used
by the flag-form extraction here and by the by-ID door) and
`bdRigQualifiedMetadataRefusal` normalize through it, so the alias cannot drift
out of sync between readers again.

For the compound two-token verbs (`mol pour` / `protomolecule pour`),
normalization has to happen *before* the compound key is composed, not after:
`bdByIDSubcommand` used to build the key `"<token> <next>"` from the raw argv
token, so `protomolecule pour` produced `"protomolecule pour"`, which
`bdflags.Known` has never heard of (only `"mol pour"` is registered) — the
write fell through unresolved and the gate never saw it. The fix normalizes
the first token through `bdNormalizeSubcommandAlias` before composing the
compound key. `bd_subcommand_aliases_freshness_test.go` (`//go:build
integration`) guards the alias table itself the same way
`internal/bdflags`'s freshness test guards the flag manifest: it shells the
real installed `bd`'s `--help` per canonical verb and fails if the live CLI
declares an alias the table doesn't carry, skipping if `bd` isn't on `PATH`.
Its domain is the table, not `bd`: it builds one subtest per canonical verb
already present in `bdSubcommandAliases`, so it catches an alias bd ADDS to a
verb the table knows and cannot catch a verb missing from the table
altogether. Deleting an entry deletes its own subtest and the guard still
passes -- measured by doing it, not inferred. Closing that hole means
enumerating bd's verbs from `bd --help` rather than from our own map, which is
deliberately out of scope here because none of the three unlisted pairs can
reach this gate.

Ordering is load-bearing and was established by a test failure rather than by
design. `gc bd` already carries a pre-flight exact-ID guard that resolves bead
IDs through the store before forwarding a mutation. That guard's `store.Get`
can fall back to shelling the real `bd` binary when the native store is
unavailable — so with the assignee gate running second, a write that was about
to be refused for an unroutable assignee had *already invoked `bd`* as a side
effect. The assignee gate is a pure config-and-arguments check with no store
access, so it runs before the ID guard. A gate that refuses after the side
effect has happened is not a gate.

Note what this does *not* do: it does not add `assign` to the ID guard's own
subcommand set. That guard answers a different question (did a fuzzy ID
resolve to the wrong bead) and already has its own flag-scanning machinery.
The two mechanisms are orthogonal and both are live.

### Which spellings resolve, and which deliberately do not

The roster carries three kinds of entry: exact identity names, pool stems (so
that `worker-2` resolves against a declared `worker` pool), and the bead-ID
prefixes the city and its rigs declare.

The subtle part is that assignees are written several legitimate ways. The file
store records an owner path-qualified (`research/dr-huhn`) where the bd store
records it bare. A rig-qualified or absolute-path spelling of a runtime
identity is still that identity. So the roster expands **path** spellings —
the name as written, and the trailing segment after the last `/` — and checks
each against the declared names.

It deliberately does **not** expand the **dot** spelling. The identity checks
that run over path candidates match on *shape*, not against a configured name.
Expanding on `.` would mean any typo carrying a bead-ID-shaped tail after a dot
(`sjarmak.gc-818bx`) passes as a session that never existed. The name lookups
are safe to widen because they match only names the config actually declares; a
spelling that is not a declared name still resolves to nothing. The
shape-matching lookups are not, so they stay narrow.

That asymmetry is the whole of the dot-spelling fix, and it is the reason the
two candidate expansions are separate functions rather than one.

Work is also assigned under the runtime session-name spelling
(`internal/agent.SessionNameFor`), which encodes `/` as `--` and `.` as `__`
so a rig-qualified agent like `repo/polecat-4` runs as session `repo--polecat-4`.
The name lookups decode that spelling back with
`agent.UnsanitizeQualifiedNameFromSession` before re-running the path
candidates over it, so it inherits the same asymmetry: fed only into the name
lookups, never into the shape-matching checks, for the same reason the dot
spelling is excluded from those.

### Doctor visibility

Two changes, both about not reporting health that was never established.

`buildDesiredState` calls `reportUnroutableAssignees` so the unmatched case is
at least *representable*: the question gets asked in the direction that can
expose a bead owned by a target that does not exist.

The unfinished-work predicate is unified so the reconciler report and the
assignee report agree on which beads are live, and a config that fails to load
is reported as a failure to load rather than as a clean run over a partial
read.

`assigneeResolvesCheck` also scans a rig it should skip: it read `rig.Suspended`
directly, the field `internal/config` documents callers must never branch on.
A rig suspended the modern way (via the runtime suspension-state store rather
than the static config bit) was still scanned while its store was not serving,
which parks the check on a permanent yellow. It now calls
`suspensionstate.EffectiveRigSuspended`, matching the sibling doctor checks
(`doctor_work_option_metadata.go`, `doctor_pool_idle_routed_work_check.go`).

## Alternatives considered

**Validate at the scheduler instead of at write time.** Rejected: the
scheduler's loop is driven by configured specs, so the unroutable bead is
structurally invisible there. Fixing it at that layer means inverting the loop
for a check that belongs at the point of entry, and the bead has already been
written and has already looked owned for however long.

**Warn instead of fail.** Rejected for the default path. The failure this
prevents is silent and open-ended — work that looks assigned and is never
done — and a warning on a command whose output nobody reads is the same as no
check. The escape hatch covers the legitimate cases that would otherwise argue
for a warning.

**Resolve assignees against the bead store rather than config.** Rejected: the
store records what was written, including the typo. Config is the only source
that says what could ever pick work up.

## Open questions

- The roster treats `human` as reserved: a fixed sentinel meaning a person is
  the target, not a role, so it is not a routable identity by construction and
  cannot be a hardcoded role name. Every other target — including a seat like
  `mayor` — is pure config and must resolve through the roster like any other
  configured agent, per the zero-hardcoded-roles invariant (AGENTS.md).
  Whether other cross-cutting sentinels deserve `human`'s treatment is
  unsettled, and adding one is a config question rather than a code one.
- The gate covers the `gc bd` seam. A direct `bd` invocation that bypasses `gc`
  is not gated, by construction. Whether that seam should move is out of scope
  here.
- `gc agent-script`'s `bd_update` action invokes the raw `bd` binary via
  `runBDForBead` without ever calling `checkBdAssigneeArgs`, so `gc` itself has
  a write path around its own gate. Closing it needs `config.City` threaded
  into `agentScriptContext`; tracked as a follow-up rather than folded into
  this round (gc-rmvve6).
