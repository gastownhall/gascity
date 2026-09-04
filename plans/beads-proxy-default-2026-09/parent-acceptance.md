Every confirmed proxied-mode gap is represented by a tracked child with a
reproduction and testable completion criteria. Fresh `gc init` with no
topology intent creates proxied-local through bd; explicit direct-local,
direct-external, and proxied-external intents map to provider capabilities.
Existing direct local/remote, embedded, DoltLite, and proxied scopes remain
unchanged across restart and ambient environment changes. GC delegates all
provider-owned lifecycle and never spawns, probes, reaps, or falls back.

Real front-door tests cover init/start/rig add/stop, bd context/CRUD/labels/
dependencies/config, restart/reconnect, inheritance/precedence, credentials,
typed unsupported refusals, provider readiness failures, partial-init retry,
and no file/process/row/event mutation on refusal. Active bundled packs and
tracker adapters use one provider-neutral persistence path; unsupported bd
operations are acceptable only when no active Gas City path invokes them.
Migration/handoff remains explicit and journaled. External SQL tests do not
claim Dolt Git history/remotes parity. Final release requires all in-scope
children, Beads RC gates, Gas City quality gates, and an actual RC2 pin/rerun.
