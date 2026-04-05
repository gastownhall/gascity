# Contributing to Gas City

Gas City is experimental software, but the repo is now structured for external
contributors. Before making changes, read:

- [docs/index.mdx](docs/index.mdx)
- [engdocs/contributors/index.md](engdocs/contributors/index.md)
- [engdocs/contributors/codebase-map.md](engdocs/contributors/codebase-map.md)
- [engdocs/architecture/index.md](engdocs/architecture/index.md)
- [TESTING.md](TESTING.md)

## Getting Started

1. Fork the repository.
2. Clone your fork.
3. Install prerequisites from
   [docs/getting-started/installation.md](docs/getting-started/installation.md).
4. Set up tooling and hooks: `make setup`
5. Build and run the fast quality gates: `make build && make check`

## Development Workflow

We use a direct-to-main workflow for trusted contributors. External
contributors should:

1. Create a feature branch from `main`
2. Make the change
3. Run `make check`
4. Run `make check-docs` if you touched docs, navigation, or cross-links
5. Open a pull request

### Branch Naming

Never open a PR from your fork's `main` branch. Use a dedicated branch per PR:

```bash
git checkout -b fix/session-startup upstream/main
git checkout -b docs/mintlify-nav upstream/main
```

Suggested prefixes:

- `fix/*`
- `feat/*`
- `refactor/*`
- `docs/*`

## Code Style

- Follow standard Go conventions
- Keep functions focused and small
- Add tests for behavior changes
- Add comments only when the logic is not self-evident

## Design Philosophy

Gas City follows two project-level principles that should shape changes:

### Zero Framework Cognition

Go handles transport, not reasoning. If the behavior belongs in the model or
prompt, do not encode it as framework intelligence in Go.

### Bitter Lesson Alignment

Prefer durable infrastructure, observability, and composition over brittle
heuristics that a stronger model should eventually handle better.

For the capability boundary, use the
[Primitive Test](engdocs/contributors/primitive-test.md).

## Docs Workflow

The docs tree is now Mintlify-based.

- Config lives in `docs/docs.json`
- Preview locally with `cd docs && npx --yes mint@latest dev`
- Run docs checks with `make check-docs`

When updating docs:

- Architecture docs describe current behavior
- Design docs describe proposed behavior
- Archive docs keep historical notes out of the main onboarding path

## Make Targets

Run `make help` for the full list. The most useful targets are:

| Command | What it does |
|---|---|
| `make setup` | Install local tools and git hooks |
| `make build` | Build `gc` with version metadata |
| `make install` | Install `gc` into `$(go env GOPATH)/bin` |
| `make check` | Fast Go quality gates |
| `make check-docs` | Docs sync tests plus Mintlify broken-link checks |
| `make check-all` | Extended quality gates including integration tests |
| `make test` | Unit and repo-level Go tests |
| `make test-integration` | Integration tests |
| `make cover` | Coverage run |

## macOS Release Verification

Before tagging a release, run the macOS smoke test on a Mac:

```bash
./scripts/smoke-macos.sh                     # latest release, arm64
GC_VERSION=v0.13.4 ./scripts/smoke-macos.sh  # specific version
GC_ARCH=amd64 ./scripts/smoke-macos.sh       # Intel binary
```

The script downloads the release archive, extracts the `gc` binary, and runs it
inside a `sandbox-exec` jail that denies network access and restricts filesystem
writes to a temp directory. Tests: `version`, `help`, `doctor`, `init`.

Run this after changing build/packaging scripts or upgrading the Go toolchain.

## Commit Messages

- Use present tense
- Keep the first line under 72 characters
- Reference issues when relevant

## Questions

Open an issue if you need clarification before a larger change.
