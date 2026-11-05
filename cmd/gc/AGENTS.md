# cmd/gc — change guide

- Pack commands consume global `--city`/`--rig` before the resolved leaf;
  keep those scopes out of the child argv. Scope-looking flags after
  that leaf or its `--` remain child input. Keep eager and lazy dispatch
  equivalent, with city context supplied through the native pack environment.

- Endpoint migrations classify ownership against the unchanged current topology
  before any writes. Exclude provider-owned scopes from the entire mutation plan,
  including compatibility config and port-file synchronization. Unknown ownership
  is a refusal; proposed endpoint settings cannot establish current authority.

City endpoint transitions must finish guarded metadata canonicalization before
publishing new endpoint config, while inherited mirrors still match the original
city topology. Preserve the original rig metadata/config snapshots through the
plan and refuse observed drift before the first topology write; this observation
is not a cross-file concurrency lock. Do not bypass provider-owned classification.

A resolved resume command must not append the one-shot `initial_message`, even
when the controller start hash is absent. Keep the retained native conversation
and the stored message; first-start classification alone does not authorize
resending it. Fresh conversation launch and explicit fresh-wake input remain
unchanged.

Doctor's prompt-budget check resolves storage-backed query topology. Register it
through the store gate so stopped and suspended scopes remain cold, including
when doctor runs with `--fix`; report the existing explicit not-checked reason.

Whole-city suspension retires every journaled provider-owned scope, including
detached storage bindings, after drain settles. Reuse ordinary stop's scope
inventory and existing ownership and suspension fences. Rig-only suspension
must leave unrelated detached scopes alone; an unreadable inventory fails cold.

Construct doctor order-history resolvers only when their gated lookup runs.
Registration and `--check` selection must not open relocated storage before
a stopped or suspended store can be excluded.
