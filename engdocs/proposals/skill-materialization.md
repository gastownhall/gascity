---
title: "Skill Materialization (v0.15.1 hotfix)"
---

## Context

PackV2's first skills slice (shipped in v0.15.0) added the directory convention
(`<pack>/skills/<name>/SKILL.md`, `agents/<name>/skills/`) and a list-only CLI
(`gc skill list`). Pack-defined skills are catalogued and visible, but they
**never reach the agent**: no symlinks are materialized into the agent's
provider-expected skill directory, and changes to skill content don't drain
the agent. This ships the remaining mechanism so skills actually work
end-to-end.

MCP's first slice stayed list-only for similar reasons, but MCP's delivery to
agents is provider-config JSON projection — a different mechanic. MCP
activation is **out of scope for v0.15.1** and lands on main afterwards.

### What exists today

- `Agent.SkillsDir`, `Agent.MCPDir`, `City.PackSkillsDir`, `City.PackMCPDir`
  runtime fields on the loaded config (`internal/config/config.go`).
- `gc skill list` and `gc mcp list` visibility commands (`cmd/gc/cmd_skill.go`,
  `cmd/gc/cmd_mcp.go`).
- `materializeSkillStubs` writes 7 `gc-<topic>` stub directories containing
  `SKILL.md` files with a `!` `gc skills <topic>` `` shell-escape
  (`cmd/gc/skill_stubs.go:16-31`). Call sites:
  `cmd/gc/cmd_start.go:491`, `cmd/gc/cmd_supervisor.go:1443-1444`, both
  gated on `cfg.Workspace.Provider == "claude"`.
- `runtime.CopyEntry{Probed, ContentHash}` content-based fingerprinting
  (`internal/runtime/runtime.go:211-233`, `internal/runtime/fingerprint.go`).
- `runtime.Config.FingerprintExtra map[string]string` already participates
  in `CoreFingerprint` (`internal/runtime/fingerprint.go:132-136`) and is
  the right carrier for "participates in fingerprint but isn't staged."
- `envFingerprintAllow` already includes `GC_SKILLS_DIR`
  (`internal/runtime/fingerprint.go:102`).
- `internal/hooks/hooks.go:80-96` enumerates **eight** provider cases:
  `claude`, `codex`, `gemini`, `opencode`, `copilot`, `cursor`, `pi`, `omp`.
- `runtime.CopyFiles` is staged **unconditionally** by every runtime
  (`internal/runtime/subprocess/subprocess.go:105-116`,
  `internal/runtime/tmux/adapter.go:105`,
  `internal/runtime/acp/acp.go:154`,
  `internal/runtime/k8s/pod.go:62-70`) via
  `overlay.CopyFileOrDir`, which follows symlinks (`os.Stat` at
  `internal/overlay/overlay.go:15`). There is no `Probed`-skips-copy gate —
  if you list a `CopyEntry`, the runtime copies it.

### What's missing

1. No mechanism materializes pack skills into the agent's provider dir.
2. Skill content changes don't participate in the agent fingerprint, so edits
   don't drain.
3. No cleanup on skill removal — the current stub materializer only writes.
4. `Agent.Skills` / `SharedSkills` filter lists exist in config but don't
   match the PackV2 design doc (`docs/packv2/doc-agent-v2.md:191`) which says
   every agent gets every city skill.

## Goals

1. Every agent gets every current-city-pack skill, materialized into its
   provider-specific skill directory at the correct scope root, before the
   agent starts.
2. Each agent additionally gets its own `agents/<name>/skills/` entries in
   the same sink; agent-local wins on name collision within a single agent.
3. Content changes to any materialized skill drain and restart the affected
   agents.
4. Skill removal produces no dangling symlinks.
5. No behavioral gap between local and worktree WorkDirs — pool instances
   get the same view as their non-pool peers.
6. Works for the four providers with confirmed skill-reading behavior.

## Non-goals (v0.15.1)

- **MCP activation.** Provider-config JSON projection for MCP servers stays
  deferred. `gc mcp list` remains list-only.
- **User-declared third-party imported-pack skill catalogs.** Only the
  current city pack's `skills/` and any **bootstrap implicit-import pack's**
  `skills/` (shipped with the `gc` binary, e.g., `core`) contribute. A
  user's `[imports.foo]` pointing at a third-party pack does **not** have
  its `skills/` walked in v0.15.1 (tracked in
  `docs/packv2/doc-pack-v2.md:59-60`). The materializer's source set is
  the union of these two categories; see "Skill source discovery" below
  for the enumeration rule.
- **Skill promotion workflows** (`gc skill promote …`) — design-noted in
  `docs/packv2/doc-agent-v2.md:207` as a later slice.
- **Fold-in of `maintenance` and `dolt` into `core`.** v0.15.1 ships the
  `core` bootstrap pack initially containing only the gc-topic stubs. Folding
  the other builtin packs into `core` lands on main after v0.15.1.
- **K8s / ACP runtime skill delivery.** Stage-2 is gated by runtime provider
  (see "Stage 2 runtime gate" below). K8s and ACP runtimes receive no skill
  materialization in v0.15.1 and log an informational line per session.
- **`copilot`, `cursor`, `pi`, `omp` providers.** These four providers are
  recognized by `internal/hooks/hooks.go:89-96` but receive no skill
  materialization in v0.15.1 — their skill-discovery conventions are not yet
  verified against current vendor docs. Their agents spawn without a skill
  sink; a single log line flags the skip at materialization time. Support is
  a follow-up once vendor paths are confirmed.

## Design

### Two-stage materialization

**Stage 1 — scope root.** At `gc start` and every supervisor tick, the
materializer walks every agent in the loaded config and, for each agent
whose provider has a registered skill directory (see "Vendor mapping"
below), creates symlinks under the agent's scope root:

- City-scoped agent → `<city>/.<vendor>/skills/<name>`
- Rig-scoped agent → `<rig>/.<vendor>/skills/<name>`

Each symlink targets the canonical source:
`<source-pack>/skills/<name>/` (shared-catalog entry from any pack
contributing skills; see "Skill source discovery" below) or
`<pack-dir>/agents/<name>/skills/<skill-name>/` (agent-local catalog).
Targets are absolute paths.

### Skill source discovery

Today's `internal/config/compose.go:143-150` only populates
`PackSkillsDir` from `<cityRoot>/skills`. The new materializer expands
this to the **union** of:

1. The current city pack's `<cityRoot>/skills/` (if present).
2. Each **bootstrap implicit-import pack's** `<resolved-pack-root>/skills/`
   (if present). Bootstrap implicit imports are the packs listed in
   `internal/bootstrap/bootstrap.go:BootstrapPacks` (today: `import`,
   `registry`; v0.15.1 adds `core`). Resolution is a two-step call:
   `config.ReadImplicitImports()` (`internal/config/implicit.go:26-52`)
   returns the map of `ImplicitImport{Source, Version, Commit}` entries
   from `~/.gc/implicit-import.toml`, and for each entry the unexported
   `resolveImplicitImport` helper (`internal/config/implicit.go:77-88`)
   returns an `Import` whose `Source` field is the resolved filesystem
   path via `config.GlobalRepoCachePath(gcHome, source, commit)`
   (`internal/config/implicit.go:90-93`). Resolved roots live at
   `<gcHome>/cache/repos/<GlobalRepoCacheDirName(source, commit)>/` —
   keyed on source URL + commit hash, not on pack name. The
   materializer iterates the `ReadImplicitImports` result, resolves each
   to its cache path, and appends the `skills/` subdirectory to the
   union if it exists. (A small helper in the materializer reproduces
   the resolve step rather than exporting `resolveImplicitImport`; the
   function is a one-liner and has no caller outside the compose path
   today.)
3. Each agent's own `<source-pack>/agents/<name>/skills/` catalog (via
   the existing `Agent.SkillsDir` runtime field).

The materializer enumerates all three and produces the union as the
desired symlink set for each agent. City-pack entries and implicit-bootstrap
entries are "shared catalog" (universal across agents of the matching
vendor); the agent-local entries are only for that one agent.

User-declared third-party imports' `skills/` are **not** enumerated in
v0.15.1 (per the non-goal). The mechanism to include them later is
additive — extending the enumeration to walk `ResolvedImports` — without
changing the materialization or fingerprint surface.

**Shared-catalog name collisions between sources** (e.g., city pack has
`plan/` and `core` also has `plan/`) resolve by precedence: city pack
wins over bootstrap implicit packs. The materializer logs a debug line
noting the shadowed source.

For non-pool agents whose session `WorkDir` equals the scope root, stage 1
is sufficient.

**Stage 2 — session WorkDir.** For pool instances (or any agent whose
resolved `WorkDir` differs from the scope root), each session receives a
`PreStart` entry injected by `BuildDesiredState`:

```
gc internal materialize-skills --agent <qualified-name> --workdir <path>
```

The entry is **appended** to the existing `PreStart` list so any
user-configured setup (worktree creation, env prep) runs first. The
materializer has no work to do if `<workdir>` does not yet exist at
invocation time, so it must run after worktree creation.

#### Stage 2 runtime gate

Stage 2 PreStart injection runs only for runtimes that execute PreStart on
the host filesystem with access to the host's `gc` binary and the host's
skill source tree. The current runtimes and their gating:

| Runtime      | Stage 2 eligible? | Reason                                           |
|--------------|-------------------|--------------------------------------------------|
| `subprocess` | yes               | PreStart runs locally before the subprocess spawn |
| `tmux`       | yes               | PreStart runs on the host                        |
| `acp`        | no                | Out of scope for v0.15.1                         |
| `k8s`        | no                | PreStart runs inside the pod; `gc` binary and host skill paths are not available there |

`BuildDesiredState` checks the agent's resolved runtime provider and skips
the PreStart injection when the runtime is ineligible. When skipped, the
agent logs one informational line per session spawn indicating stage 2 was
omitted. Remote-runtime skill delivery (k8s/ACP) is tracked as a follow-up
and will likely use a different mechanism (content-copy into the pod's
workdir, or a sidecar init step).

### Vendor mapping

| Provider   | Skill sink           | v0.15.1 status    |
|------------|----------------------|-------------------|
| `claude`   | `.claude/skills/`    | materialize       |
| `codex`    | `.codex/skills/`     | materialize       |
| `gemini`   | `.gemini/skills/`    | materialize       |
| `opencode` | `.opencode/skills/`  | materialize       |
| `copilot`  | —                    | skip (no sink)    |
| `cursor`   | —                    | skip (no sink)    |
| `pi`       | —                    | skip (no sink)    |
| `omp`      | —                    | skip (no sink)    |

Implemented as a map keyed on `agent.Provider`; providers without an entry
get no materialization. Each `materialize` path should be re-verified
against the vendor's current CLI docs during implementation — if a vendor's
primary path differs (e.g., Codex reads `.agents/skills/` primarily), swap
the map entry before merge. The mechanism doesn't care.

### Per-skill symlinks (granularity)

One symlink per skill, not a folder-level symlink. A folder symlink would
conflict with other contents in the sink. Per-skill symlinks compose
cleanly with sibling content.

A symlink looks like:

```
<workdir>/.claude/skills/plan -> <city>/skills/plan
```

Target is an absolute path for diagnostic clarity. Relative targets may
come later if relocatable worktrees become a requirement.

### No attachment filtering

Per `docs/packv2/doc-agent-v2.md:191`:

> An agent gets city-wide skills + its own skills. Agent-specific wins on
> name collision.

Every agent's materialization set = (entire city catalog) ∪
(that agent's `agents/<name>/skills/`). No filtering by attachment list.

The existing attachment-list surfaces (introduced in v0.15.0, commits
`7572464a` and `710bd3b5`) are dead code relative to the design doc and
are removed outright. The full deletion surface:

| Location                                   | Symbols / fields                                            |
|--------------------------------------------|-------------------------------------------------------------|
| `internal/config/config.go:1402-1405`      | `Agent.Skills`, `Agent.MCP`                                 |
| `internal/config/config.go:1438-1443`      | `Agent.SharedSkills`, `Agent.SharedMCP` (runtime-only)      |
| `internal/config/config.go:1273, 1276`     | `AgentDefaults.Skills`, `AgentDefaults.MCP`                 |
| `internal/config/config.go:1833-1852`      | `applyAgentSharedAttachmentDefaults`                        |
| `internal/config/config.go:1854-1891`      | `mergeAgentDefaults` — reads `src.Skills` / `src.MCP` to merge defaults across pack/city layers; entries must be removed along with the fields |
| `internal/config/patch.go:57-64`           | `AgentPatch.Skills`, `AgentPatch.MCP`, `AgentPatch.SkillsAppend`, `AgentPatch.MCPAppend` |
| `internal/config/patch.go:258-269`         | Apply paths for the four patch fields                       |
| `internal/config/config.go:398-401`        | `AgentOverride.Skills`, `AgentOverride.MCP`                 |
| `internal/config/config.go:428-431`        | `AgentOverride.SkillsAppend`, `AgentOverride.MCPAppend`     |
| `internal/config/pack.go` apply function   | Override apply paths for all four `Skills`/`MCP`/`SkillsAppend`/`MCPAppend` |
| `internal/migrate/migrate.go:83-85`        | Migrate-struct `Skills` / `MCP` fields                      |
| `internal/migrate/migrate.go:658-706`      | Migrate read + zero-value check for `Skills` / `MCP`        |
| `internal/config/field_sync_test.go:39-41, 66-68, 196-199` | Test expectations for `SharedSkills`, `SkillsAppend`, `MCPAppend` parity |
| `cmd/gc/pool.go:264-279` (deep-copy)       | Pool deep-copy entries for the four fields                  |
| `cmd/gc/cmd_skill.go:97-107`               | `attachmentSet` / `filterEntriesByName` filter path         |
| `cmd/gc/cmd_mcp.go` (equivalent filter)    | MCP filter path                                             |
| `docs/schema/city-schema.json`             | Schema entries for `skills`, `mcp`, `skills_append`, `mcp_append` |
| `docs/reference/config.md:162-200`         | Reference-doc entries for the removed fields                |
| `internal/config/compose_test.go:242-246`  | Attachment-defaults compose test                            |
| `internal/config/config_test.go:118-139`   | `TestParseAgentSkillsAndMCP` — delete test entirely         |
| `internal/config/config_test.go:172-234`   | `AgentDefaults.Skills`/`MCP` parsing tests — delete         |
| `internal/config/config_test.go:4104-4128` | Attachment-inheritance integration assertions               |
| `cmd/gc/pool_test.go:577-581`              | Pool-copy test uses `Skills`/`SharedSkills`/`SkillsDir` — delete entries |
| `internal/migrate/migrate_test.go:366, 395, 397` | Migrate fixture + allow-list entries for `Skills`/`SharedSkills`/`SkillsDir` |

**Backwards compatibility for v0.15.1.** The TOML parser in v0.15.1 retains
the field definitions as **tombstones** — accepted but unused — and emits a
one-time deprecation warning on load when any of the removed fields appear
in user TOML. The warning points at the file and field and states: "This
field was removed in v0.15.1. It is ignored in v0.15.1 and will be a parse
error in v0.16." A `gc doctor --fix` rule strips the fields from user TOML
on request. In v0.16 the tombstones are deleted and the parser rejects the
fields. This softens the upgrade for v0.15.0 adopters who wired these
fields into their configs based on the published JSON schema.

(The schema and reference docs are updated in v0.15.1 to mark the fields
deprecated; they are deleted in v0.16.)

### Per-skill fingerprint entries via `FingerprintExtra`

Each materialized symlink produces one entry in the agent's
`runtime.Config.FingerprintExtra`:

```go
cfg.FingerprintExtra["skills:"+skillName] = runtime.HashPathContent(source)
```

`FingerprintExtra` participates in `CoreFingerprint`
(`internal/runtime/fingerprint.go:132-136`) with a `"fp"` prefix sentinel,
ensuring the skill content hash contributes to the fingerprint without
colliding with `Env` keys.

**Why not `CopyEntry`.** Every runtime's staging loop iterates
`cfg.CopyFiles` and unconditionally stages each entry: `subprocess` and
`tmux` via `overlay.CopyFileOrDir`
(`internal/runtime/subprocess/subprocess.go:105-116`,
`internal/runtime/tmux/adapter.go:105`), `acp` via its own copy path
(`internal/runtime/acp/acp.go:154`), and `k8s` via `copyToPod`/tar
staging (`internal/runtime/k8s/staging.go`, invoked from
`internal/runtime/k8s/pod.go:62-70`). Adding skill `CopyEntry` records
would cause each runtime to stage/copy the skill content into the
workdir by its own mechanism, shadowing or duplicating the symlinks the
materializer placed. There is no "probed means don't stage" flag;
`Probed` only controls whether the fingerprint entry hashes by content
vs. path. `FingerprintExtra` is the single right carrier for "hashes
contribute to the fingerprint but delivery happens out-of-band."

**When `FingerprintExtra["skills:*"]` is populated.** The rule:
populate skill fingerprint entries for an agent if and only if skill
materialization actually reaches that agent. Concretely:

- Agent whose runtime is stage-2 eligible (`subprocess`, `tmux`) AND
  whose resolved `WorkDir` equals the scope root → stage 1 delivers,
  populate.
- Agent whose runtime is stage-2 eligible AND whose `WorkDir` differs
  from the scope root (pooled worktree) → stage 2 delivers, populate.
- Agent whose runtime is stage-2 **ineligible** (`k8s`, `acp`) →
  materialization does not run, so no skill content reaches the agent,
  so skill-catalog edits should not drain the agent. **Do not
  populate** any `skills:*` entries for these agents.

This rule ensures no spurious drain-restart cycles on remote-runtime
agents that can't consume the skills anyway. A unit test asserts the
policy.

**Diagnostic reporting.** `CoreFingerprintBreakdown` already exposes
`FingerprintExtra` under the `FPExtra` key. Extending its drift log to
surface per-skill key changes (`skills:<name>`) is a small follow-up,
tracked here but not required for the hotfix.

### Cleanup: ownership-by-target-prefix

On every materialization pass, before creating symlinks for the new
desired set:

1. Walk `<sink>/.<vendor>/skills/`.
2. For each entry, read its metadata via `os.Lstat` (does not follow the
   symlink) and the link target via `os.Readlink` (succeeds on dangling
   symlinks).
3. If the entry is a symlink **and** its target path has a prefix matching
   a known gc-managed skills root (the city pack's `<city>/skills/` or
   any agent's `<pack-dir>/agents/<name>/skills/`):
   - If it is not in the new desired set → delete.
   - If in the desired set but target has drifted → replace via atomic
     rename (see "Atomic symlink update" below).
4. Regular files and directories are left untouched.

No manifest file. Ownership is encoded in the symlink's target path.
This aligns with the CLAUDE.md "no status files, query live state"
principle.

#### Atomic symlink update

Each symlink create/replace uses a two-step write-then-rename:

1. Create a temporary symlink `<sink>/.<name>.tmp.<nonce>` pointing to the
   new target.
2. `os.Rename` over `<sink>/.<name>` (atomic on POSIX via the `rename(2)`
   syscall — no intermediate window where a reader sees the sink missing).

This eliminates the window an observer might see a broken or missing
symlink during cleanup+recreate.

#### Safety and decision matrix

Cleanup only deletes when (entry is a symlink) AND (target begins with a
gc-managed skills root). Regular files and dirs are always left alone.

| Entry type         | Target                   | In desired? | Action                     |
|--------------------|--------------------------|-------------|----------------------------|
| Symlink            | gc-managed root          | Yes, match  | Keep                       |
| Symlink            | gc-managed root          | Yes, drift  | Atomic replace             |
| Symlink            | gc-managed root          | No          | Delete                     |
| Symlink            | gc-managed root          | Dangling    | Delete                     |
| Symlink            | External path            | N/A         | Leave alone                |
| Regular file       | —                        | N/A         | Leave alone (legacy stub or user content) |
| Regular directory  | —                        | N/A         | Leave alone (legacy stub dir or user content) |

A unit test enumerates this matrix directly.

### Legacy stub migration

v0.15.0 cities have **regular directories** at
`<workdir>/.claude/skills/gc-<topic>/` written by the old
`materializeSkillStubs`, each containing a `SKILL.md` with the
`!` `gc skills <topic>` `` shell-escape. The new `core` pack delivers
skills at the same names (`gc-agents`, `gc-city`, `gc-dashboard`,
`gc-dispatch`, `gc-mail`, `gc-rigs`, `gc-work`), so the materializer
must clear the legacy path before creating the new symlinks.

**Migration step** (runs once per sink on first materialization pass after
upgrade):

1. For each legacy stub name, inspect `<sink>/gc-<topic>/`.
2. If the entry is a regular directory AND contains exactly one file
   `SKILL.md` AND that file's contents match the v0.15.0 stub shape
   (YAML frontmatter + the specific `!` `gc skills <topic>` `` command line),
   delete the directory recursively.
3. If the entry exists but doesn't match the stub shape, **leave it alone**
   and log a warning — this is user content the operator has placed at a
   name that conflicts with a `core` pack skill. The materializer
   skips that specific core skill for that sink and logs the conflict; the
   operator can resolve by renaming their own content.

Matching the specific stub-content shape (not just the directory name) is
important: users may have created their own `gc-<topic>/` skill with
real content, and we must not delete user work. The stub shape is
idempotent and self-identifying — we use the whole file body as the fingerprint.

The migration step is implemented inside the materializer and runs every
pass, but the decision matrix above makes it a no-op after the first
successful pass (the regular directory becomes a symlink, and the
symlink-branch rules take over from then on).

### Collision validation (startup validator)

Two agents sharing the same scope root cannot both contribute an
agent-local skill under the same name, because both would want to write
the same `<scope-root>/.<vendor>/skills/<name>` symlink with different
targets.

This is a **startup validator** — a new check function in the config/doctor
layer that runs at `gc start` and at every supervisor tick before
materialization. The validator:

- For each `(scope-root, vendor)` pair, groups agents that materialize
  into it.
- For each group, builds the multi-map
  `agent-local-skill-name → [agent-names]`.
- Emits a hard error for any key with more than one agent in its value
  list.

The same validator is also exposed as a `gc doctor` check
(`internal/doctor/skill_checks.go`), so operators can catch collisions
outside a startup gate. The doctor check and the startup gate call the
same function; the validator is the single source of truth. `gc doctor`
surfacing does **not** introduce a new "doctor runs at start" gate — the
supervisor invokes the validator directly and blocks on its error.

Example error text:

```
gc start: agent-local skill collision at scope root /path/to/rig (claude):
  "plan" is provided by both "mayor" and "supervisor"
  rename one of the colliding skills to resolve
```

### Mixed-provider cities

A city may have agents on different providers at the same scope root
(e.g., a `claude` mayor and a `codex` supervisor both city-scoped). The
per-agent iteration produces **per-vendor sinks side-by-side** at the
scope root:

```
<city>/
  .claude/skills/           # materialized for claude agents
    gc-work/ -> ...
    plan/ -> ...
  .codex/skills/            # materialized for codex agents
    gc-work/ -> ...
    plan/ -> ...
```

Each sink is filled only with its vendor's desired set. The collision
validator is scoped per `(scope-root, vendor)` pair; agents on different
vendors don't collide even if they share an agent-local skill name.

Acceptance tests cover at least:
- Mixed-provider city with one agent per vendor.
- Non-Claude workspace-default city with a Claude-provider agent (this is
  the case the old `Workspace.Provider == "claude"` gate mis-handled).

### Lifecycle: supervisor tick ordering

The corrected per-tick order:

```
1. ResolveFormulas                          (existing)
2. Validate agents / hooks                  (existing)
3. Skill collision validator                (new, blocks on violation)
4. Materialize skills                       (new universal materializer)
     - legacy stub migration (first-pass only)
     - cleanup orphans (ownership-by-target-prefix)
     - atomic symlink create/replace
5. BuildDesiredState per agent              (existing, with new additions)
     - append PreStart entry for stage-2 eligible runtimes (skip k8s/ACP)
     - populate FingerprintExtra["skills:<name>"] entries
6. Compute fingerprints                     (existing)
7. Drain on drift                           (existing)
```

Validation precedes materialization so a collision cannot produce a
half-written sink. The stage-2 PreStart entry is **appended** after any
existing user-configured PreStart entries in the config — user setup runs
first, materialize-skills runs last (immediately before the agent command).

**Concurrency note.** Stage-1 materialization writes to scope-root sinks
(`<city>/.claude/skills/`, `<rig>/.claude/skills/`). Stage-2 materialization
writes to per-session-worktree sinks
(`<rig>/.gc/worktrees/<rig>/polecat-N/.claude/skills/`). These paths are
disjoint by construction, so the supervisor-tick materializer and a
per-session PreStart never target the same sink. No cross-stage lock is
required.

**Pool scale-up cost.** A pool scaling from 0 to N spawns N sessions, each
running its own `gc internal materialize-skills` invocation in PreStart —
N separate walks of the skill catalog. For realistic pool sizes
(single-digit to low tens) and realistic skill catalog sizes (dozens),
this is well under a second per spawn. Not optimized for this release;
follow-up work can introduce per-pool caching if it becomes a concern.

### CLI surface

**Added:**

- `gc internal materialize-skills --agent <qualified-name> --workdir <path>` —
  not user-facing. Creates symlinks (with cleanup) for one agent at one
  workdir. Used by per-session `PreStart` and by the supervisor tick via a
  direct Go call (same function, two callers).
- `gc doctor` skill-collision check — new check file under `internal/doctor/`
  calling the shared validator function.
- `gc doctor --fix` rule — strips the removed attachment-list fields from
  user TOML on request.

**Changed:**

- `gc skill list` — removes the `Agent.Skills` / `SharedSkills` filter
  (`cmd/gc/cmd_skill.go:97-107`). Output is the union of city catalog and
  the target agent's own `agents/<name>/skills/`, matching what is
  materialized. `--agent <name>` narrows to one agent's effective view;
  agent-local wins on name collision in the output.

**Removed:**

- `gc skills <topic>` command (`cmd/gc/cmd_skills.go`) — obsoleted by moving
  content into the `core` pack.
- `cmd/gc/skill_stubs.go` + call sites at `cmd/gc/cmd_start.go:491` and
  `cmd/gc/cmd_supervisor.go:1443-1444`.
- `cmd/gc/skills/*.md` embedded content (moves into `core/skills/`).

### New `core` bootstrap pack

Path: `internal/bootstrap/packs/core/`.

Contents in v0.15.1:

```
core/
├── pack.toml
└── skills/
    ├── gc-agents/SKILL.md
    ├── gc-city/SKILL.md
    ├── gc-dashboard/SKILL.md
    ├── gc-dispatch/SKILL.md
    ├── gc-mail/SKILL.md
    ├── gc-rigs/SKILL.md
    └── gc-work/SKILL.md
```

Each `SKILL.md` has real YAML frontmatter followed by the static content
migrated from `cmd/gc/skills/<topic>.md`. No shell-escape — content is
first-class.

Added to `BootstrapPacks` in `internal/bootstrap/bootstrap.go:34-38`:

```go
{Name: "core", Source: "(embedded)", Version: "0.1.0", AssetDir: "packs/core"},
```

Becomes an implicit import for every city alongside `import` and `registry`.
`gc init` resolves it into the user-global cache via
`config.GlobalRepoCachePath` — same mechanism used for other implicit
bootstrap packs. The resolved root is
`<gcHome>/cache/repos/<GlobalRepoCacheDirName(source, commit)>/` (a
single directory component keyed on combined source+commit hash, not by
pack name). `gc init` also adds an entry to `~/.gc/implicit-import.toml`.
The materializer reaches the `core` pack's `skills/` via
`ReadImplicitImports` as described in "Skill source discovery" above.

**Name-collision with a user-declared `[imports.core]`.** Today's
behavior is silent shadowing:
`internal/bootstrap/bootstrap.go:91-94` unconditionally writes the
bootstrap entry into `~/.gc/implicit-import.toml`, and
`internal/bootstrap/packs/import/lib/implicit.py:154` (the `gc import`
splice logic) plus the packman semantics
(`docs/packv2/doc-packman.md:169`) treat explicit `[imports.X]` as
taking precedence over the implicit splice. A user with `[imports.core]`
therefore silently overrides the bootstrap entry, which on upgrade would
silently replace the expected `gc-<topic>` skills with whatever is in
the user's `core` pack.

v0.15.1 **adds** explicit collision-detection on top of the existing
behavior. Two new checks, both calling into the shared
`internal/bootstrap/collision.go` predicate:

1. `gc init` / `gc import install` refuses to write the implicit-import
   entry for `core` if the loading city already declares
   `[imports.core]`. It prints a message directing the operator to
   rename one side and exits non-zero.
2. The composer emits a hard diagnostic (not a silent shadow) when a
   user's `[imports.core]` would shadow the bootstrap `core` entry,
   blocking load until renamed.

Acceptance coverage for both surfaces. This is **new work** in
v0.15.1, not a free ride on existing code.

The post-v0.15.1 main-branch fold-in will add `maintenance` and `dolt`
content under `core/assets/` with transitive re-export from `core/pack.toml`.
That is not part of this release.

## Migration / upgrade

**v0.15.0 → v0.15.1:**

- Users with `skills = [...]`, `shared_skills = [...]`, or the MCP
  equivalents on an agent or under `[agent_defaults]` see a **one-time
  deprecation warning** on config load pointing at the file and field.
  The fields are parsed and ignored. `gc doctor --fix` offers to strip
  them. Hard parse error lands in v0.16.
- Users who relied on `gc skills <topic>` as a CLI reference must switch
  to reading the skill content directly. Files are real markdown now,
  living at the resolved cache path for the `core` bootstrap pack
  (`<gcHome>/cache/repos/<key>/skills/gc-<topic>/SKILL.md`, where
  `<key>` is `GlobalRepoCacheDirName(source, commit)`). Operators who
  need the exact path can read `~/.gc/implicit-import.toml` to find
  `core`'s source+commit and compute the key. The command is removed
  outright; a follow-up could add `gc skill path <name>` as a
  convenience if this becomes painful.
- Existing `~/.gc/implicit-import.toml` files gain a `core` entry on next
  `gc init` or `gc import install`. No user action required unless a
  collision is detected (see above).
- Existing `<workdir>/.claude/skills/gc-<topic>/` regular directories
  written by the old stub materializer are detected by the legacy-stub
  migration step and removed before the new symlinks are created, but
  only when the directory's contents match the v0.15.0 stub shape
  exactly. User-placed content at the same names is preserved, with a
  warning and the corresponding core skill skipped for that sink.
- Existing cities using `Workspace.Provider == "claude"` with a mix of
  providers across agents: the old materializer gated the whole operation
  at the workspace level, silently skipping any Claude agent in a
  non-Claude workspace. The new per-agent gating flips this; those
  agents now get skills materialized. Acceptance coverage required.

## Testing

**Unit:**
- `internal/runtime/fingerprint_test.go` — extend with `FingerprintExtra`
  `skills:*` key coverage and per-key drift assertion.
- New `cmd/gc/materialize_skills_test.go` — vendor-map lookup (8 providers,
  4 active), per-skill entry creation, cleanup decision matrix
  (table-driven across the seven rows above), legacy-stub migration
  (match vs user-content-preserve), atomic-replace under drift, collision
  validator formatting.
- `internal/config/field_sync_test.go` — update allow-list to reflect the
  removed fields.

**Acceptance:**
- `test/acceptance/skill_test.go` — extend with:
  - "city skill is materialized into every agent's sink (per-vendor)."
  - "agent-local skill is only in that agent's sink."
  - "adding a skill to the city catalog drains affected agents."
  - "removing a skill cleans up the symlink on next tick."
  - "renaming a skill (delete + add) cleans up old and creates new."
  - "user-placed `.claude/skills/my-skill/` directory is preserved."
  - "user-placed `.claude/skills/gc-work/` with non-stub content is
    preserved and the core skill is skipped for that sink with a warning."
  - "collision between two agents at the same scope root blocks start."
  - "mixed-provider city produces one sink per provider at the scope root."
  - "k8s-runtime agent spawns with no skill sink and logs the skip line."

**Integration:**
- `test/integration/skill_lifecycle_test.go` (new) — full cycle with a
  fake pool: modify `<city>/skills/` content, assert the agent drains
  and respawns with refreshed symlinks. Includes a multi-session pool
  scale-up assertion that every session lands with a populated sink.

## Open questions / follow-ups

1. **Vendor path verification.** Each `materialize` map entry must be
   re-verified against the vendor's current CLI docs during
   implementation. Swap entries as needed.
2. **Support for `copilot`, `cursor`, `pi`, `omp`.** Deferred pending
   vendor-path verification.
3. **Remote-runtime (k8s, ACP) skill delivery.** Deferred. Likely shape is
   a content-copy into the pod's workdir via a new runtime hook, not
   symlinks.
4. **MCP activation.** Lands on main post-v0.15.1.
5. **Skill promotion (`gc skill promote`).** Not in this hotfix. Tracked
   in `docs/packv2/doc-agent-v2.md:207`.
6. **Imported-pack skill catalogs.** Still current-city-pack-only. Tracked
   in `docs/packv2/doc-pack-v2.md:59`.
7. **Per-pool PreStart caching.** Optional perf improvement if pool
   scale-up invocation cost becomes noticeable.
8. **`CoreFingerprintBreakdown` per-skill drift log.** Small ergonomic
   follow-up — surface `skills:<name>` keys individually in drift
   diagnostics rather than collapsed under `FPExtra`.
