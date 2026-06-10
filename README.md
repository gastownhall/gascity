<h1 align="center">Gas City</h1>

<p align="center">
  <strong>Composable orchestration infrastructure for multi-agent coding workflows.</strong>
</p>

<p align="center">
  <a href="https://github.com/gastownhall/gascity/actions/workflows/ci.yml?query=branch%3Amain"><img src="https://img.shields.io/github/actions/workflow/status/gastownhall/gascity/ci.yml?branch=main&label=Build&style=for-the-badge" alt="Build status"></a>
  <a href="https://docs.gascityhall.com"><img src="https://img.shields.io/badge/Docs-latest-c9a84c.svg?style=for-the-badge" alt="Documentation"></a>
  <a href="https://github.com/gastownhall/gascity/releases"><img src="https://img.shields.io/github/v/release/gastownhall/gascity?include_prereleases&style=for-the-badge" alt="GitHub release"></a>
  <a href="https://discord.gg/xHpUGUzZp2"><img src="https://img.shields.io/discord/1462817445562814505?label=Discord&logo=discord&logoColor=white&color=5865F2&style=for-the-badge" alt="Discord"></a>
  <a href="LICENSE"><img src="https://img.shields.io/badge/License-MIT-blue.svg?style=for-the-badge" alt="MIT License"></a>
</p>

Gas City is an orchestration-builder SDK for multi-agent systems. It extracts
the reusable infrastructure from Gas Town into a configurable toolkit with
runtime providers, work routing, formulas, orders, health patrol, and a
declarative city configuration.

## Sponsors

<p align="center">
  <a href="https://blacksmith.sh/">
    <img src="docs/images/blacksmith-powered.png" alt="Powered by Blacksmith" height="40">
  </a>
</p>

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
| dolt | Beads provider `bd` | 2.1.0 or newer | `brew install dolt` | [releases](https://github.com/dolthub/dolt/releases) |
| bd | Beads provider `bd` | 1.0.0 | [releases](https://github.com/gastownhall/beads/releases) | [releases](https://github.com/gastownhall/beads/releases) |
| flock | Beads provider `bd` | — | `brew install flock` | `apt install util-linux` |
| gh | Optional GitHub gates | — | `brew install gh` | [cli.github.com](https://cli.github.com/) |
| claude / codex / gemini | Per provider | — | See provider docs | See provider docs |

The `bd` (beads) provider is the default. To use a file-based store instead
(no dolt/bd/flock needed), set `GC_BEADS=file` or add `[beads] provider = "file"`
to your `city.toml`.

Managed Dolt checks require a final Dolt 2.1.0 or newer. Older and
pre-release builds are below Gas City's managed bd/Dolt compatibility floor;
releases before 1.86.2 can also miss the upstream GC/writer deadlock fix in
dolthub/dolt commit `ccf7bde206`, which can hang `dolt_backup sync` under
heavy write load.

Install from Homebrew:

```bash
brew install gastownhall/gascity/gascity
gc version
```

Or build from source (requires `make`, Go 1.25+, and ICU for a transitive Dolt
CGO dependency — `brew install icu4c` on macOS, `apt install libicu-dev` on
Linux; on macOS the Makefile auto-detects the keg-only `icu4c` paths):

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

### Nix/Flox machines (ICU not on the default CGO path)

On NixOS / Flox-managed Linux toolchains, system include/lib dirs are not
searched, so the build fails with `fatal error: unicode/uregex.h: No such file
or directory`. Point CGO at the Nix-store ICU dev headers + matching runtime
lib (pick the dev output whose propagated lib matches your `gc`/`dolt` link, to
avoid ICU version skew), and disable the Makefile's `/usr/lib` fallback so the
Nix and system toolchains don't get mixed:

```bash
# Re-resolve the dev header path if the store path changes:
#   find /nix/store -maxdepth 3 -path '*icu4c*-dev/include/unicode/uregex.h'
# Match the lib to what your installed binary links: ldd $(which gc) | grep icu
ICU_DEV=/nix/store/dvhx24q4icrig4q1v1lp7kzi3izd5jmb-icu4c-76.1-dev
ICU_LIB=/nix/store/i4lj3w4yd9x9jbi7a1xhjqsr7bg8jq7p-icu4c-76.1

CGO_ENABLED=1 \
CGO_CPPFLAGS="-I$ICU_DEV/include" \
CGO_LDFLAGS="-L$ICU_LIB/lib" \
SYS_USR_CGO_FALLBACK=0 \
  make build      # or: go build -o bin/gc ./cmd/gc
```

CGO-backed tests (e.g. `go test ./internal/beads`) take the same three CGO_*
vars — no `CGO_ENABLED=0` workaround needed once ICU is pointed at correctly.

For the longer walkthrough, start with
[Tutorial 01](docs/tutorials/01-cities-and-rigs.md).

## Documentation

📖 **Read the docs online: [docs.gascityhall.com](https://docs.gascityhall.com)**

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
make docs-dev

# or directly from the repo root
./mint.sh dev
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

## License

MIT
