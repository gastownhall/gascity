# Builtin provider conformance

Every builtin provider in `profiles.go` is more than a config entry — it is
a **conformance-tested integration**. The worker conformance program proves,
per provider, that Gas City can launch the CLI, deliver work to it, read its
conversation back, survive restarts without losing context, and isolate fresh
sessions from old ones. The grid below is generated from the conformance
suite's report artifacts; a cell is only ✅ when a real test run produced a
passing result for that provider.

## Compatibility grid

<!-- BEGIN GENERATED: worker-conformance-grid (scripts/worker_conformance_grid.py) -->
_Generated 2026-08-16 from 32 conformance report(s); `zcode` row added 2026-08-18 from its own phase-1/2/3 reports._

### Phase 1 — transcript & continuation contract (deterministic fixtures)

| Provider | Transcript discovery | Transcript normalization | Continuation continuity | Fresh-session isolation |
|---|---|---|---|---|
| `claude` | ✅ | ✅ | ✅ | ✅ |
| `codex` | ✅ | ✅ | ✅ | ✅ |
| `cursor` | ✅ | ✅ | ✅ | ✅ |
| `gemini` | ✅ | ✅ | ✅ | ✅ |
| `kimi` | ✅ | ✅ | ✅ | ✅ |
| `opencode` | ✅ | ✅ | ✅ | ✅ |
| `mimocode` | ✅ | ✅ | ✅ | ✅ |
| `zcode` | ✅ | ✅ | ✅ | ✅ |
| `pi` | ✅ | ✅ | ✅ | ✅ |
| `antigravity` | ✅ | ✅ | ✅ | ✅ |

### Phase 2 — runtime substrate (deterministic, fake transport)

| Provider | Bring-up | Diagnostics | Interactions | Tool events |
|---|---|---|---|---|
| `claude` | ✅ | ✅ | ✅ | ✅ |
| `codex` | ✅ | ✅ | ✅ | ✅ |
| `cursor` | ✅ | ✅ | ✅ | ✅ |
| `gemini` | ✅ | ✅ | ✅ | ✅ |
| `kimi` | ✅ | ✅ | ✅ | ✅ |
| `opencode` | ✅ | ✅ | ✅ | ✅ |
| `mimocode` | ✅ | ✅ | ✅ | ✅ |
| `zcode` | ✅ | ✅ | ✅ | ✅ |
| `pi` | ✅ | ✅ | ✅ | ✅ |
| `antigravity` | ✅ | ✅ | ✅ | ✅ |

### Phase 3 — live inference proofs (real provider CLI + real models)

| Provider | Spawn + task | Continuation | Fresh reset | Workspace task | Multi-turn | Interrupt/recover |
|---|---|---|---|---|---|---|
| `claude` | 🔒 | 🔒 | 🔒 | 🔒 | 🔒 | 🔒 |
| `codex` | 🔒 | 🔒 | 🔒 | 🔒 | 🔒 | 🔒 |
| `cursor` | 🔒 | 🔒 | 🔒 | 🔒 | 🔒 | 🔒 |
| `gemini` | 🔒 | 🔒 | 🔒 | 🔒 | 🔒 | 🔒 |
| `kimi` | 🔒 | ✅ | 🔒 | 🔒 | 🔒 | 🔒 |
| `opencode` | 🔒 | 🔒 | 🔒 | 🔒 | 🔒 | 🔒 |
| `mimocode` | ✅ | ✅ | ✅ | ✅ | ✅ | ➖ |
| `zcode` | ✅ | ✅ | ✅ | ✅ | ✅ | ✅ |
| `pi` | 🔒 | 🔒 | 🔒 | 🔒 | 🔒 | 🔒 |
| `antigravity` | 🔒 | ✅ | 🔒 | 🔒 | 🔒 | 🔒 |
<!-- END GENERATED: worker-conformance-grid -->

### Symbols

| Symbol | Meaning |
|---|---|
| ✅ | Verified passing by a recorded conformance run |
| ❌ | Verified failing — known incompatibility |
| ⚠️ | Partially verified (some requirements in the group have no recorded result) |
| ➖ | Unsupported by the provider CLI (documented capability gap, not a bug) |
| 🔒 | Not verified — requires credentials/binaries unavailable on the verification host |

## What each section proves

### Phase 1 — transcript & continuation contract

Deterministic tests over provider-native transcript fixtures. No network, no
models; this is the data contract.

- **Transcript discovery** (`WC-TX-001`) — Gas City finds the provider's
  on-disk conversation for a session's working directory.
- **Transcript normalization** (`WC-TX-002`) — the provider-native format
  (JSONL, export JSON, sqlite-mirrored exports, …) converts into Gas City's
  canonical message shape: roles, text, thinking, tool calls.
- **Continuation continuity** (`WC-CONT-001`) — a continued session keeps the
  same logical conversation and its full history.
- **Fresh-session isolation** (`WC-CONT-002`) — a reset session does NOT
  inherit or alias the previous conversation.

### Phase 2 — runtime substrate

Deterministic tests against a fake transport: the machinery around the CLI.

- **Bring-up** (`WC-BRINGUP-001`) — startup resolves to a bounded outcome
  (ready / blocked / failed), never a hang.
- **Diagnostics** (`WC-TX-003`) — malformed or truncated transcripts surface
  as degraded history, not silent data loss.
- **Interactions** (`WC-INT-000…006`) — permission prompts and questions from
  the CLI surface as typed pending interactions; responding clears them,
  mismatched responses are rejected, and the full lifecycle lands in durable
  history.
- **Tool events** (`WC-TOOL-001/002`) — tool calls and results are preserved
  through normalization, including a still-open tool call at the tail.

Additionally, startup command/config materialization (`WC-START-001/002`),
initial-message delivery semantics (`WC-INPUT-001…005`), instance-local
interaction dedup (`WC-INT-004`), and the real tmux transport proof
(`WC-TRANSPORT-001`) are enforced for **every profile** by
`make test-worker-core-phase2` and `make test-worker-core-phase2-real-transport`;
those legs don't emit per-profile report artifacts, so they appear here as a
blanket guarantee instead of grid columns.

### Phase 3 — live inference proofs

The real provider CLI, real credentials, real models, driven through the
production worker stack (tmux transport, hook plugins, transcript mirrors).

- **Spawn + task** (`WI-START-001`, `WI-TASK-001`, `WI-TX-001`) — a fresh
  worker session boots, completes a concrete file-writing task, and its
  transcript is discovered and normalized.
- **Continuation** (`WI-CONT-001`) — stop the worker, start it again, and the
  resumed session recalls context from before the restart. For hook-managed
  providers this exercises the full production chain: provider hook plugin →
  `gc prime --hook` → session-key persistence in the city store → resume flag
  on relaunch.
- **Fresh reset** (`WI-RESET-001`) — resetting a session rotates the provider
  conversation and provably suppresses recall of the prior anchor.
- **Workspace task** (`WI-TOOL-001`) — the worker reads workspace files and
  completes a task against them.
- **Multi-turn** (`WI-MTURN-001`) — a three-turn workflow with context
  handoff between turns.
- **Interrupt/recover** (`WI-INT-001`) — an in-flight turn can be interrupted
  and the session continues afterward. (Marked ➖ where the provider CLI
  cannot cancel an in-flight turn.)

## Regenerating the grid

Refreshing one provider without disturbing the others:

```bash
python3 scripts/worker_conformance_grid.py \
  --report-dir /tmp/grid/phase1 --report-dir /tmp/grid/phase2 \
  --report-dir /tmp/grid/live \
  --provider <provider> \
  --readme internal/worker/builtin/README.md
```

Full regeneration (only correct when the report dirs cover every provider):

```bash
GC_WORKER_REPORT_DIR=/tmp/grid/phase1 make test-worker-core
GC_WORKER_REPORT_DIR=/tmp/grid/phase2 make test-worker-core-phase2
# per provider, with that provider's credentials staged:
GC_WORKER_REPORT_DIR=/tmp/grid/live PROFILE=<provider>/tmux-cli make test-worker-inference
python3 scripts/worker_conformance_grid.py \
  --report-dir /tmp/grid/phase1 --report-dir /tmp/grid/phase2 \
  --report-dir /tmp/grid/live \
  --readme internal/worker/builtin/README.md
```

## Verification notes

- Live results were last recorded on the maintainer verification host; 🔒
  cells mean the credentials or account state for that provider were not
  available there at generation time, not that the integration is broken.
- Live continuation for hook-managed providers (`opencode`, `mimocode`,
  `kimi`, `pi`, `antigravity`) runs through the production hook chain:
  provider hook plugin → `gc prime --hook` → city-store session key →
  resume flag. `mimocode` (model `xiaomi-token-plan-sgp/mimo-v2.5-pro`),
  `kimi` (Ollama Cloud `kimi-k2.6`), and `antigravity` have recorded live
  passes through that chain. `pi` is wired identically but its bootstrap
  task times out on the verification host before resume mechanics engage.
- `mimocode` spawn/reset/workspace/multi-turn proofs run on the free
  `mimo/mimo-auto` tier; continuation ran on
  `xiaomi-token-plan-sgp/mimo-v2.5-pro`.
- `opencode` live runs are backed by the Gemini free-tier model on this host;
  task-completion timeouts there reflect model capability, not the
  integration contract. Its managed-city live tests (reset, workspace) also
  need an `opencode` provider-readiness entry before `gc init --provider
  opencode` accepts them.
- `antigravity` and `mimocode` cannot cancel an in-flight turn
  (`WI-INT-001` ➖) — documented CLI limitations, both discovered live by
  this same conformance program.
- `zcode` rows are from a full live run on glm-5.3 via the Z.ai coding plan
  (`make test-worker-inference PROFILE=zcode/tmux-cli`). It has no launchable
  TUI, so the pane runs the engine's own adapter
  (`internal/worker/adapters/zcode`); `WI-INT-001` passes because the adapter
  cancels the turn itself — the ZCode CLI installs a SIGINT handler and works
  straight through a pane `^C`. Usage extraction is unregistered for the family,
  so `WC-TX-USAGE-001` / `WC-USAGE-COST-001` record Unsupported.
- `zcode`'s phase-2 Tool events and Interactions cells certify that the
  NORMALIZER handles those shapes, not that the family produces them. Its
  transcripts are an export mirror this repo writes, and the adapter emits text
  parts only — no tool or interaction parts — because each turn is a headless
  call whose structured events ZCode does not expose. The scenarios feed
  hand-built mirrors carrying those parts, which is a real reader contract and
  worth keeping, but a green cell here is not evidence that a live zcode worker
  surfaces tool calls or permission prompts. Wiring real tool-part extraction is
  a follow-up, not something these cells already cover.
- A `zcode` conversation reset ARCHIVES the superseded conversation rather than
  deleting it: gc's reset contract issues the reset and then reads the pre-reset
  transcript, so the record has to stay resolvable by its own scope. Superseded
  scopes move to `$XDG_STATE_HOME/gascity/zcode/archived-transcripts/`, a
  different tree from the live mirror root the model browses, and discovery
  unions both. The anti-leak guarantee is therefore "the prior conversation is
  not adjacent", not "not present" — adjacency is the leak that was observed
  (GLM read a stale mirror sitting beside the fresh one in the same directory).
  Enforcement is the behavioral no-recall proof plus the per-epoch CLI HOME,
  which IS deleted because ZCode's own session database has no preservation
  contract and is the copy the CLI itself would reattach to.
- The zcode row is refreshed with `--provider zcode`, which rewrites only that
  provider's rows. A full regeneration writes every provider from the reports it
  is handed, so running it on a host that holds one provider's reports would
  silently downgrade every other provider's recorded cells to "not verified".
