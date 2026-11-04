# cmd/gc — change guide

- Pack commands consume global `--city`/`--rig` before the resolved leaf;
  keep those scopes out of the child argv. Scope-looking flags after
  that leaf or its `--` remain child input. Keep eager and lazy dispatch
  equivalent, with city context supplied through the native pack environment.
