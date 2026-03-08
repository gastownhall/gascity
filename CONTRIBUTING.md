# Contributing to Gas City

Thanks for your interest in contributing! Gas City is experimental software, and we welcome contributions that help explore these ideas.

## Getting Started

1. Fork the repository
2. Clone your fork
3. Install prerequisites (see README.md)
4. Set up tooling and git hooks: `make setup`
5. Build and test: `make build && make check`

## Development Workflow

We use a direct-to-main workflow for trusted contributors. For external contributors:

1. Create a feature branch from `main`
2. Make your changes
3. Ensure quality gates pass: `make check`
4. Submit a pull request

### PR Branch Naming

**Never create PRs from your fork's `main` branch.** Always create a dedicated branch for each PR:

```bash
# Good - dedicated branch per PR
git checkout -b fix/session-startup upstream/main
git checkout -b feat/formula-parser upstream/main

# Bad - PR from main accumulates unrelated commits
git checkout main  # Don't PR from here!
```

Why this matters:
- PRs from `main` accumulate ALL commits pushed to your fork
- Multiple contributors pushing to the same fork's `main` creates chaos
- Reviewers can't tell which commits belong to which PR
- You can't have multiple PRs open simultaneously

Branch naming conventions:
- `fix/*` - Bug fixes
- `feat/*` - New features
- `refactor/*` - Code restructuring
- `docs/*` - Documentation only

## Code Style

- Follow standard Go conventions (`gofmt`, `go vet`)
- Keep functions focused and small
- Add comments for non-obvious logic
- Include tests for new functionality

## Design Philosophy

Gas City follows two core principles that shape every contribution. Understanding
these will save you (and reviewers) time.

### Zero Framework Cognition (ZFC)

**Go provides transport. Agents provide cognition.**

Gas City's Go code handles plumbing: tmux sessions, message delivery, hooks,
nudges, file transport, and observability primitives.
All reasoning, judgment calls, and decision-making happen in the AI agents via
molecule formulas and role templates.

This means:
- **No hardcoded thresholds in Go.** Don't write `if age > 5*time.Minute`
  to decide if an agent is stuck. Expose the age as data and let the agent decide.
- **No heuristics in Go.** Don't write detection logic that pattern-matches
  agent behavior. Give agents the tools to observe, and let them reason.
- **Formulas over subcommands.** If the feature is "detect X and do Y," it's
  probably a molecule step, not a new `gc` subcommand.

**The test:** Before adding Go code, ask yourself — *"Am I adding transport or
cognition?"* If the answer is cognition, it should be a molecule step or
formula instruction instead.

For the full rationale, see
[Zero Framework Cognition](https://steve-yegge.medium.com/zero-framework-cognition-a-way-to-build-resilient-ai-applications-56b090ed3e69).

### Bitter Lesson Alignment

Gas City bets on models getting smarter, not on hand-crafted heuristics getting
more elaborate. If an AI agent can observe data and reason about it, we expose
the data (transport) rather than encoding the reasoning (cognition). Today's
clumsy heuristic is tomorrow's technical debt — but a clean observability
primitive ages well.

**Examples:**

| Good (transport) | Bad (cognition in Go) |
|---|---|
| `gc session attach <name>` | Go code deciding *when* to nudge |
| `bd show <id>` exposing bead status | Go code deciding *what* bead status means |
| `tmux has-session` checking liveness | Go code with hardcoded "stuck after N minutes" |

## What to Contribute

Good first contributions:
- Bug fixes with clear reproduction steps
- Documentation improvements
- Test coverage for untested code paths
- Small, focused features

For larger changes, please open an issue first to discuss the approach.

## Commit Messages

- Use present tense ("Add feature" not "Added feature")
- Keep the first line under 72 characters
- Reference issues when applicable

## Make Commands

Run `make help` to see all targets. Key commands:

| Command | What it does |
|---|---|
| `make setup` | Install tools (golangci-lint) and git hooks |
| `make build` | Compile `gc` binary with version metadata |
| `make install` | Build and install `gc` to `~/.local/bin` |
| `make check` | Fast quality gates: format check, lint, vet, unit tests |
| `make check-all` | All quality gates including integration tests |
| `make test` | Unit tests only |
| `make test-integration` | All tests including integration (tmux, etc.) |
| `make lint` | Run golangci-lint |
| `make fmt` | Auto-fix formatting |
| `make fmt-check` | Fail if formatting would change files |
| `make vet` | Run `go vet` |
| `make cover` | Run tests with coverage report |
| `make clean` | Remove build artifacts |

Before submitting a PR, run:

```bash
make check
```

The pre-commit hook (installed by `make setup`) runs `make check` automatically on every commit.

## Questions?

Open an issue for questions about contributing. We're happy to help!
