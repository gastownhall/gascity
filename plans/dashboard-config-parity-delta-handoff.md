# Dashboard Config Parity Delta Handoff

Status: handoff-ready, discovery-first.

Owner repo: `/home/amos/projects/gascity`

Related codex_prompts Bead: `codex-prompts-obe`

## Goal

Investigate why the `codex_prompts` Dell-backed Gas City dashboard/supervisor path behaves differently from environments where the dashboard likely works normally. Identify the environment/configuration delta and implement the smallest first-class Gas City fix, or write a precise blocked design note if the fix requires an operator decision.

## Why This Exists

In the `codex_prompts` rig, helper-backed `cgc` reports a healthy native store and active agents, while raw `gc` and the dashboard/supervisor-backed path report degraded/non-native store and no sessions. The helper scripts are useful diagnostics, but they may be masking a GC-side configuration-resolution problem.

The important comparison is not "are other people viewing our dashboard"; it is "what differs between working/default Gas City dashboard deployments and our external Dell-backed city/rig topology?"

## Prior Evidence

- In `/home/amos/projects/codex_prompts`, this read-only parity probe failed live:
  `scripts/gc_config_parity_preflight.py --city "$CODEX_GC_CITY" --rig codex-prompts --json`
- Raw `gc` reported degraded/non-native store and `running_agents=0`.
- Helper-backed `cgc` reported native store healthy with active sessions/running agents.
- Local dashboard was served by a `gc dashboard` process on `*:8080`; supervisor/API listened on `127.0.0.1:8372`.
- Dashboard/supervisor process env inspection showed only `GC_CITY` and a Dolt CLI password, not the full helper-backed topology/credential env used by `cgc`.
- The `codex_prompts` WIP source path for the parity probe is `/home/amos/projects/codex_prompts/scripts/gc_config_parity_preflight.py`.

## Hypothesis To Test

The dashboard works in simpler/default environments because raw `gc`, supervisor, dashboard, and spawned sessions all resolve the same city/rig/store topology there. Our case differs because it uses an external Dell-backed Dolt endpoint, split city/HQ database versus repo rig database, SSH tunnel/auth helpers, and possibly inherited/cached rig binding state.

GC may not currently have one first-class non-secret topology plus secret-resolution path that every runtime surface uses consistently.

## Scope

- Compare a known-working/default Gas City dashboard setup against the `codex_prompts` Dell-backed external-city setup.
- Trace config resolution for raw CLI, supervisor, dashboard API, spawned sessions, and helper wrapper paths.
- Identify where city database, rig database, external host/port/user, rig binding, password/tunnel material, and native store eligibility diverge.
- Prefer a GC-native fix: durable non-secret topology in city/rig config plus one secret resolution mechanism shared by CLI, supervisor, dashboard, and sessions.
- Add or adapt a parity/preflight test in this repo so raw CLI, dashboard/API-backed status, and helper/script-backed status cannot disagree silently.

## Non-Goals

- Do not bless `cgc` as the product UX.
- Do not hardcode the `codex_prompts` Dell endpoint or secret values into Gas City source.
- Do not mutate the live `codex_prompts` city/runtime topology while investigating unless a separate active issue explicitly authorizes it.
- Do not collapse HQ/city Beads and repo rig Beads into one ambiguous scope.

## Likely Source Areas

- `cmd/gc/`
- `internal/api/`
- `internal/beads/`
- `internal/session/`
- `docs/runbooks/managed-city-endpoints.md`
- `engdocs/contributors/dolt-regression-audit.md`
- `docs/reference/schema/city-schema.json`
- `/home/amos/projects/codex_prompts/scripts/gc_config_parity_preflight.py`

## Execution Guidance

1. Reproduce or characterize the mismatch with read-only commands first.
2. Compare config/env/store resolution in a simple managed local city where dashboard status works normally against the `codex_prompts` external Dell-backed city.
3. Find the first boundary where raw CLI/supervisor/dashboard/session resolution loses information that helper-backed `cgc` still has.
4. Implement the smallest first-class fix behind existing config, beads, API, or runtime boundaries.
5. Add a regression test that models the external-city/rig-database split without requiring live secrets.
6. Document the intended configuration contract for external city/HQ plus repo rig stores.

## Suggested Verification

- Run a focused package test set covering changed areas, for example:
  `go test ./cmd/gc ./internal/api ./internal/beads ./internal/session`
- Run a read-only parity command showing raw CLI, supervisor/API-backed dashboard status, and helper-backed status agree in the relevant topology.
- If live validation is unsafe or unauthorized, record a blocked note with the exact command an operator should run.

## Beads Write Blocker

I attempted to create this as a Gas City Beads task from `/home/amos/projects/gascity`, but this checkout's Beads path is currently not writable with the installed `bd`:

- Direct `bd create` did not read `.beads/config.yaml` and failed because `issue_prefix` was missing.
- `bd bootstrap --dry-run` found remote Dolt data on `git@github.com:gastownhall/gascity.git`.
- `bd bootstrap --yes` cloned the remote store, but the installed `bd 1.0.4` then failed to open it with a `local_metadata` unknown-fields schema error.
- The failed remote clone was removed after diagnosis, and the previous local embedded database was restored.
- `gc bd` was also blocked by an invalid synthetic import cache for the `bd` pack.

Once Gas City Beads is repaired in this checkout, convert this file into a normal `ga-*` Beads task and remove or link this handoff file.
