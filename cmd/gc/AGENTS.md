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
