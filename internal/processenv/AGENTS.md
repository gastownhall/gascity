# Provider process environment

Readiness probes and worker launches must use the same selected credential
context. Preserve `CODEX_HOME` and `CODEX_ACCESS_TOKEN` through the curated exact
key allowlist: dropping either can check a different account and falsely report
readiness. Keep other Codex process/tooling variables outside that allowlist;
the `CODEX_` namespace is broader than authentication.

Keep controller-only values explicitly empty in worker overlays, and retain the
nesting resets. Diagnostics report safe readiness verdicts and remedies without
provider output or credential values. Changes to this boundary need a caller
regression through provider preflight, plus coverage for similarly named keys.
