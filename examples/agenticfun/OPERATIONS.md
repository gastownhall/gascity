# AgenticFun Operations

Run these commands from `examples/agenticfun` unless noted.

## Start

Build the city-local `gc` and put it first on `PATH`:

```bash
./scripts/build-gc.sh
export PATH="$PWD/.gc/bin:$PATH"
```

Clone the org repos once:

```bash
mkdir -p repos
git clone https://github.com/AgenticFunProject/equipments repos/equipments
git clone https://github.com/AgenticFunProject/quotes repos/quotes
git clone https://github.com/AgenticFunProject/web-page repos/web-page
git clone https://github.com/AgenticFunProject/booking repos/booking
git clone https://github.com/AgenticFunProject/users repos/users
```

Start the city:

```bash
gc start .
gc status
```

Default runtime is Codex over tmux: `codex/tmux-cli`.
AgenticFun sessions are demand-driven: routing work to
`repo/agenticfun.<agent>` or manually waking a session starts that agent, and
idle agents sleep after a short window. The city uses a slower health patrol
cadence to keep idle CPU and runtime-log growth modest.

## Attach

Attach to the city coordinator:

```bash
gc session attach agenticfun.director
```

Attach to a rig agent:

```bash
gc session attach equipments/agenticfun.architect
gc session attach quotes/agenticfun.builder
gc session attach web-page/agenticfun.playtester
gc session attach booking/agenticfun.integrator
gc session attach users/agenticfun.builder
```

List sessions first if unsure:

```bash
gc session list
```

Wake or nudge a session:

```bash
gc session wake equipments/agenticfun.builder
gc session nudge equipments/agenticfun.builder "Run gc hook and take routed work."
```

## Shutdown

Stop the AgenticFun city:

```bash
gc stop .
```

Check that sessions are gone:

```bash
gc status
gc session list --state=all
```

Do not use bare `tmux kill-server`. Gas City uses an isolated tmux socket for
the city; prefer `gc stop .` so controller state and sessions shut down
cleanly.
