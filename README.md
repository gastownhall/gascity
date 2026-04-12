# Gas City

Gas City is an orchestration-builder SDK for multi-agent systems. It extracts
the reusable infrastructure from Gas Town into a configurable toolkit with
runtime providers, work routing, formulas, orders, health patrol, and a
declarative city configuration.

## Coming from Gas Town?

Start with [Coming from Gas Town?](docs/getting-started/coming-from-gastown.md).
It maps Town roles, commands, plugins, convoys, and directory habits onto Gas
City's primitive-first model so experienced Gas Town users can ramp without
trying to port the entire Town architecture literally.

## What You Get

- Declarative city configuration in `city.toml`
- Multiple runtime providers: tmux, subprocess, exec, ACP, and Kubernetes
- Beads-backed work tracking, formulas, molecules, waits, and mail
- A controller/supervisor loop that reconciles desired state to running state
- Packs, overrides, and rig-scoped orchestration for multi-project setups

## Quickstart

See the full install guide at [docs/getting-started/installation.md](docs/getting-started/installation.md).

### Prerequisites

Gas City requires the following tools on your system. `gc init` and
`gc start` check for these automatically and report any that are missing.

| Dependency | Required | Min Version | Install (macOS) | Install (Linux) |
|------------|----------|-------------|-----------------|-----------------|
| tmux | Always | — | `brew install tmux` | `apt install tmux` |
| git | Always | — | `brew install git` | `apt install git` |
| jq | Always | — | `brew install jq` | `apt install jq` |
| pgrep | Always | — | (included in macOS) | `apt install procps` |
| lsof | Always | — | (included in macOS) | `apt install lsof` |
| dolt | Beads provider `bd` | 1.86.1 | `brew install dolt` | [releases](https://github.com/dolthub/dolt/releases) |
| bd | Beads provider `bd` | 1.0.0 | [releases](https://github.com/gastownhall/beads/releases) | [releases](https://github.com/gastownhall/beads/releases) |
| flock | Beads provider `bd` | — | `brew install flock` | `apt install util-linux` |
| claude / codex / gemini | Per provider | — | See provider docs | See provider docs |

The `bd` (beads) provider is the default. To use a file-based store instead
(no dolt/bd/flock needed), set `GC_BEADS=file` or add `[beads] provider = "file"`
to your `city.toml`.

Install from Homebrew:

```bash
brew install gastownhall/gascity/gascity
gc version
```

Or build from source (requires `make` and Go 1.25+):

```bash
make install

gc init ~/bright-lights
cd ~/bright-lights
gc start

mkdir hello-world
cd hello-world
git init
gc rig add .

bd create "Create a script that prints hello world"
gc session attach mayor
```

For the longer walkthrough, start with
[Tutorial 01](docs/tutorials/01-cities-and-rigs.md).

## Documentation

The docs now use a Mintlify structure rooted in [`docs/`](docs/README.md).

- [Docs Home](docs/index.mdx)
- [Installation](docs/getting-started/installation.md)
- [Quickstart](docs/getting-started/quickstart.md)
- [Repository Map](docs/getting-started/repository-map.md)
- [Contributors](engdocs/contributors/index.md)
- [Reference](docs/reference/index.md)
- [Architecture](engdocs/architecture/index.md)
- [Design Docs](engdocs/design/index.md)
- [Archive](engdocs/archive/index.md)

Preview the docs locally:

```bash
cd docs
npx --yes mint@latest dev
```

## Repository Map

- `cmd/gc/`: CLI commands, controller wiring, and supervisor integration
- `internal/runtime/`: runtime provider abstraction and implementations
- `internal/config/`: `city.toml` schema, pack composition, and validation
- `internal/beads/`: store abstraction and provider implementations
- `internal/session/`: session bead metadata and wait helpers
- `internal/orders/`: periodic formula and exec dispatch
- `internal/convergence/`: bounded iterative refinement loops
- `examples/`: sample cities, packs, formulas, and configs
- `contrib/`: helper scripts and deployment assets
- `test/`: integration and support test packages

## Contributing

Read [CONTRIBUTING.md](CONTRIBUTING.md) and
[engdocs/contributors/index.md](engdocs/contributors/index.md) before opening a
PR.

Useful commands:

- `make setup`
- `make check`
- `make check-docs`
- `make test-integration`

### Tutorial Harness

This repo includes a project-scoped coding-agent skill for the tutorial
acceptance harness at
[`/.claude/skills/isolated-tutorial-harness`](.claude/skills/isolated-tutorial-harness/SKILL.md).
Use it when running or debugging the tutorial tests through Codex or Claude:

- invoke `$isolated-tutorial-harness`
- follow the workflow in the skill

The harness is designed to keep `gc` state isolated in temp homes, temp
supervisors, and temp runtime dirs while still authenticating provider CLIs
correctly. It expects a repo-local `.env` file with:

```bash
CLAUDE_CODE_OAUTH_TOKEN=...
```

Optional:

```bash
OPENAI_API_KEY=...
```

The main validation flow is:

```bash
go test ./test/acceptance/helpers -run 'TestProviderShim|TestEnsureClaude' -count=1
go test ./internal/api -run 'TestProbeCommandEnv(PreservesXDGOverridesWhenGHConfigDirIsSet|PassesClaudeOAuthToken)$|TestHandleProviderReadinessAcceptsClaudeOAuthTokenAuth' -count=1
go test -tags acceptance_c ./test/acceptance/tutorial_goldens -run '^TestTutorial01Cities$' -count=1 -v
go test -tags acceptance_c ./test/acceptance/tutorial_goldens -run '^TestTutorial04Communication$' -count=1 -v
go test -tags acceptance_c ./test/acceptance/tutorial_goldens -run '^TestTutorial03Sessions$' -count=1 -v
go test -tags acceptance_c ./test/acceptance/tutorial_goldens -count=1
```

If the isolated Claude path regresses, start with the skill before changing the
tutorials. The common failure mode is provider auth/readiness, not tutorial
content.

## License

MIT
