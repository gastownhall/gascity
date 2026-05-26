# AgenticFun Usage

## 1. Build the city-local gc

Run from `examples/agenticfun`:

```bash
./scripts/build-gc.sh
export PATH="$PWD/.gc/bin:$PATH"
```

This keeps the AgenticFun example on its own `gc` binary. Agents inherit that
binary through `GC_BIN`, and `.gc/bin` is first on their `PATH`.

## 2. Clone the org repos

Run from `examples/agenticfun`:

```bash
mkdir -p repos
git clone https://github.com/AgenticFunProject/equipments repos/equipments
git clone https://github.com/AgenticFunProject/quotes repos/quotes
git clone https://github.com/AgenticFunProject/web-page repos/web-page
git clone https://github.com/AgenticFunProject/booking repos/booking
git clone https://github.com/AgenticFunProject/users repos/users
```

## 3. Start the city

```bash
gc start .
gc status
```

The city already defines five rigs: `equipments`, `quotes`, `web-page`,
`booking`, and `users`.

Default harness/CLI is Codex over tmux (`codex/tmux-cli`).
AgenticFun sessions are demand-driven by default; agents wake on routed work or
explicit `gc session wake`, then sleep after a short idle window.

## 4. Send work to the right repo

Plan a vague idea:

```bash
gc sling equipments/agenticfun.architect \
  -f mol-agenticfun-idea-to-slice \
  --var idea="Improve equipment availability search"
```

Send an existing bead to a builder:

```bash
gc sling quotes/agenticfun.builder <bead-id> --on mol-agenticfun-slice-build
```

Run a playtest pass:

```bash
gc sling web-page/agenticfun.playtester \
  -f mol-agenticfun-playtest-loop \
  --var subject=<bead-or-branch>
```

Send merge-ready work to integration:

```bash
gc sling booking/agenticfun.integrator \
  -f mol-agenticfun-integrate \
  --var subject=<bead-or-branch>
```

## 5. Repo defaults

| Rig | Profile | Main checks |
| --- | --- | --- |
| `equipments` | TypeScript service | `npm test`, `npm run build` |
| `quotes` | Python FastAPI service | `pytest`, `python -m build` |
| `web-page` | Static JS demo | `python3 -m http.server 8080 --directory .` |
| `booking` | Java/Spring service | `./mvnw compile`, `./mvnw test` |
| `users` | TypeScript service | `npm test`, `npm run build` |

## 6. Stop

```bash
gc stop .
```
