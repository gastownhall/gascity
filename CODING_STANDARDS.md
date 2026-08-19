# Coding standards

## Tests

- Prefer the highest available seam that proves real behaviour: unit tests next
  to the code for package logic, process-backed `cmd/gc` tests for CLI
  workflows, and `//go:build integration` tests under `test/` when behaviour
  crosses filesystem, tmux, Git, Beads/Dolt, or a real `gc` binary.
- Prefer exercising production behaviour over treating a green suite as proof
  that a city, agent, pack, or formula workflow works end to end.
- Mock only third-party services or process boundaries we cannot control. Do
  not mock packages or business rules that we own.
- For command and workflow changes, the default proof is to run the real
  command against a temporary city (or focused package helpers) and assert
  exit status, output, and persisted state.
- Assert results produced by production code. Do not assert values assembled
  only by test helpers, copied production logic, or mocks configured by the
  same test.
- Use `t.TempDir()` for filesystem tests. Put integration tests that need real
  infrastructure behind `//go:build integration`. Read `TESTING.md` before
  adding a new test tier or seam.
- Keep concurrency tests deterministic. Synchronize on observable state or
  explicit test seams instead of sleeps when possible, and run race-sensitive
  packages with `go test -race ./path/to/package`.
- Use `make test` (or `make test-fast-parallel` when sharding helps) as the
  fast unit baseline. Prefer the sharded targets in `TESTING.md` —
  `make test-cmd-gc-process-parallel`, `make test-integration-shards-parallel`,
  `make test-local-full-parallel` — over a monolithic `go test ./...` sweep.
  Markdown-only changes do not require Go builds or tests.

## Comments and docs

- Code comments use ASD-STE100 Simplified Technical English: use short direct
  sentences, one instruction per sentence, and consistent terms.
- Use established Gas City domain terms consistently: city, agent, pack,
  formula, bead, molecule, wisp, convoy, sling, hook, rig, event bus, and
  config. Role behaviour is user-supplied configuration — never hardcode Gas
  Town role names (or any specific role name) in Go, comments that encode
  framework judgment, or standards text that implies a built-in role.
- Do not write comments that only repeat what the code already makes clear.
  Explain invariants, boundary conditions, compatibility constraints, and
  non-obvious reasons.
- Do not put brittle references in comments or docs, such as line numbers,
  temporary paths, current versions, or "as of today" claims, when those
  details can change.
- Update user-facing docs when commands, configuration, output, or operational
  behaviour changes. Keep `AGENTS.md` as the agent constitution; do not fork
  conflicting instructions into parallel constitution files.

## Common footguns

- Tautological tests that assert a mock was called exactly as the test
  configured it.
- Mocks of packages, modules, or services we own.
- Treating a green suite as proof that a real agent, city, or pack can
  complete the workflow.
- Encoding agent judgment in Go: behavioural heuristics and arbitrary decision
  thresholds belong in prompts, packs, and formulas. Deterministic protocol
  rules and safety boundaries can remain in Go (Zero Framework Cognition).
- Hardcoding role names in Go or SDK paths. Roles are pure configuration; if
  removing a `[[agent]]` entry breaks an SDK feature, that is a design error.
- Confusing ephemeral wisps with durable Beads-backed work, or assuming every
  command reads the same Beads database.
- Swallowing process, tmux, Git, or persistence errors and then reporting
  success.
- Using sleeps to hide lifecycle races or relying on goroutine completion
  order for stable results.
- Writing PID/lock/status files to track running processes instead of querying
  live state (process table, ports, `ps`, `lsof`).
- Bare `tmux kill-server` cleanup, or killing the default tmux server. Target
  only a known city/test socket (`tmux -L <socket> ...`), or prefer `gc stop`.
- Narrating comments, stale README claims, and hard-coded implementation
  details.
- Evading complexity or quality gates with denser syntax, hidden branching, or
  indirection that does not reduce real complexity.
- Hand-written JSON, `map[string]any`, or `json.RawMessage` on HTTP/SSE wire
  paths outside documented API exceptions. Prefer typed Huma endpoints and
  registered event payloads.

## Go

- Format with `gofmt`; keep `go vet ./...` and `make lint` (`golangci-lint`)
  clean.
- Validate trust boundaries manually as well as with linters. Check
  user-controlled paths, subprocess arguments, SQL identifiers, and external
  input before use; linter exclusions are not evidence that an operation is
  safe.
- Keep the module path `github.com/gastownhall/gascity` and the Go version in
  `go.mod` honest. Do not use newer language features without deliberately
  updating the module version.
- Follow Zero Framework Cognition: Go transports data, enforces deterministic
  protocols and safety boundaries, and performs deterministic operations;
  agents and formulas perform behavioural judgment and reasoning.
- Use existing package boundaries and production seams. Keep Cobra command
  wiring in `cmd/gc` thin, and put reusable domain behaviour in the relevant
  `internal` package. The CLI and HTTP+SSE API are projections over the domain
  object model — neither re-implements domain logic.
- Pass `context.Context` through blocking work. Give subprocesses and external
  operations explicit cancellation or timeouts.
- Wrap errors with operation and resource context. Preserve causes with `%w`,
  and do not convert failures into apparent success. Prefer
  `fmt.Errorf("adding rig %q: %w", name, err)` style messages.
- Make concurrent output deterministic. Protect shared state, avoid goroutine
  leaks, and verify concurrency changes with the race detector.
- Preserve compatibility across documented city, rig, worktree, pack, and
  configuration layouts unless a migration is part of the change.
- When adding a field to `config.Agent`, also update `AgentPatch`,
  `AgentOverride`, their apply functions, and the `poolAgents` deep-copy in
  `cmd/gc/pool.go` (`TestAgentFieldSync` covers the struct definitions).
- Run `make build` and `make test` (or `make test-fast-parallel`) before
  merging changes that can affect Go behaviour. Run focused package tests
  during development, sharded process/integration targets for affected paths,
  and `make dashboard-check` when touching `internal/api/`, OpenAPI artifacts,
  or `cmd/gc/dashboard/`. Markdown-only changes do not require Go builds or
  tests.
- For releases, keep release tags honest with `make check-version-tag` when
  HEAD is a release tag.
- Use a dedicated branch for every pull request, based on the intended base
  branch. Never open a pull request from a fork's `main` branch.
- Every exported function has a doc comment. No panics in library code —
  return errors. Prefer atomic file writes (`temp file` → `os.Rename`).
