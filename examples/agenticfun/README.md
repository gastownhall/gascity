# AgenticFunProject Org Example

AgenticFun is a Gas City pack dedicated to the public
`AgenticFunProject` GitHub organization. It combines Swarmforge's
architect/builder/reviewer discipline with Gas City primitives:

- durable beads for work and handoff
- formulas for repeatable planning, build, playtest, integration, and
  convergence workflows
- orders for deterministic maintenance
- configured agents instead of hardcoded roles

Default harness: Codex CLI over tmux (`codex/tmux-cli`). The city sets
`workspace.provider = "codex"` and `[session].provider = "tmux"`, and each
pack agent declares `provider = "codex"` so the pack defaults to Codex even
when imported elsewhere.

## Agents

City-scoped:

- `director` coordinates ideas and routing.
- `ops` handles infrastructure work that cannot be expressed as an exec order.

Rig-scoped:

- `architect` turns ideas into accepted slices.
- `builder` implements one small slice at a time.
- `reviewer` validates behavior and tests read-only.
- `playtester` exercises visible behavior as a user.
- `integrator` validates handoff and merges or rejects.

## Use

```bash
cd examples/agenticfun
./scripts/build-gc.sh
export PATH="$PWD/.gc/bin:$PATH"

mkdir -p repos
git clone https://github.com/AgenticFunProject/equipments repos/equipments
git clone https://github.com/AgenticFunProject/quotes repos/quotes
git clone https://github.com/AgenticFunProject/web-page repos/web-page
git clone https://github.com/AgenticFunProject/booking repos/booking
git clone https://github.com/AgenticFunProject/users repos/users

gc start .
```

`gc start .` keeps AgenticFun sessions demand-driven by default. Agents wake
when work is routed to them or when you explicitly wake a session, then sleep
after a short idle window. The city also uses a slower health patrol cadence so
idle demos spend less CPU and write fewer runtime events.

The example city defines these rigs directly:

| Rig | Repo | Profile | Branch | Key commands |
| --- | --- | --- | --- | --- |
| `equipments` | `AgenticFunProject/equipments` | TypeScript service | `master` | `npm install`, `npm test`, `npm run build`, `npm run dev` |
| `quotes` | `AgenticFunProject/quotes` | Python FastAPI service | `main` | `./scripts/bootstrap-venv.sh`, `pytest`, `python -m build`, `uvicorn app.main:app --reload` |
| `web-page` | `AgenticFunProject/web-page` | Static JavaScript demo | `main` | `python3 -m http.server 8080 --directory .` |
| `booking` | `AgenticFunProject/booking` | Java/Spring spec-first service | `master` | `./mvnw compile`, `./mvnw test`, local Spring profile run |
| `users` | `AgenticFunProject/users` | TypeScript service | `main` | `npm install`, `npm test`, `npm run build`, `npm run dev` |

For a vague idea, route planning to the architect:

```bash
gc sling equipments/agenticfun.architect \
  -f mol-agenticfun-idea-to-slice \
  --var idea="Add an equipment availability drill-down"
```

For bounded iteration, create a convergence loop:

```bash
gc converge create \
  --formula mol-agenticfun-fun-converge \
  --target web-page/agenticfun.builder \
  --var subject=<bead-or-branch>
```
