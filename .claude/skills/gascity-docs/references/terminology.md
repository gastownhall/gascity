# Renaming a concept across the docs

When the project's word for something changes (e.g. controller → orchestrator,
SDK → platform), the rule is: **rename the concept in prose; preserve every literal
that reflects the actual program.** The code and its output are the source of
truth — if the docs rename a string the binary still prints, the docs now lie.

## The discipline

1. **Survey first.** Count occurrences in `docs/`, `engdocs/`, `AGENTS.md`, and
   `*.go`. Read enough to tell *prose* (the concept) from *literals*.
2. **Rename prose only**, with judgment per occurrence — fix articles and grammar
   so it reads naturally ("an SDK" → "a platform").
3. **Preserve literals:**
   - Real program output inside a code fence (e.g. `Controller: supervisor-managed (PID …)`, `Welcome to Gas City SDK!`). Verify against `cmd/` / `internal/` source; if the binary prints it, the doc must match.
   - JSON field names / API fields (e.g. the `controller` field — `json:"controller"`), config keys, flags, file paths, and any backticked code identifier.
   - A *different* thing with the same word ("provider SDKs", Claude Code's SDK — not Gas City).
   - **Generated files** (`docs/reference/cli.md`, `config.md`, `schema/*`): never hand-edit. If the term must change there, change it in the Go source and run `go run ./cmd/genschema`.
4. **Don't touch the code or `engdocs/` implementation names** as part of a docs
   rename unless explicitly asked — those describe the implementation, which keeps
   its own names. Renaming the binary's output strings is a separate Go-source PR
   (with test updates), worth flagging as a follow-up so output and docs align.
5. **Watch for collateral words.** A word-boundary match for `controller` must not
   touch `control bead` / `control-dispatcher` / `control flow`; a rename must not
   mangle `supervisor` / `reconciler` (different components).
6. **Verify after:** every remaining occurrence in `docs/` (outside generated
   files) should be a justified literal — audit them explicitly, classify each as
   "literal (correct)" or "missed prose (fix)".

## Settled literal exceptions (as of the orchestrator/platform rename)

- `controller` — keep in: `gc status` output (`Controller: …`), the `controller`
  JSON field in city-status, config-field descriptions in `config.md` (generated).
  Everywhere else in docs prose: **orchestrator**.
- `SDK` — keep in: the `Welcome to Gas City SDK!` init banner (real output), and
  "provider SDKs" (a different SDK). Everywhere else in docs prose: **platform**.
