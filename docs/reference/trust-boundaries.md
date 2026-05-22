---
title: "Command Execution Trust Boundaries"
---

Gas City intentionally runs operator-configured commands. Those commands are a
feature, not a sandbox. Treat city config, imported packs, exec provider
scripts, and agent startup commands as trusted code with the same review
expectations as shell scripts committed to the repository.

## Trust Model

| Input | Trust level | Rule |
|-------|-------------|------|
| Maintainer-authored city config and local site config | Trusted operator code | May define shell commands and explicit env. Review before use. |
| Imported packs and rig configs | Trusted dependency code | Pin/review packs before importing into a privileged city. |
| Bead titles, descriptions, mail, formula vars, PR text, and API request fields | Untrusted data | Do not concatenate into shell commands. Pass as env, JSON, stdin, or argv. |
| GitHub Actions `pull_request_target` payloads | Untrusted data in a privileged workflow | Do not checkout or execute contributor code. Use metadata-only operations. |
| Ambient process environment | Untrusted for secret propagation | Controller-side shell helpers strip inherited secret-looking env keys by default. |

## Execution Surfaces

| Surface | Command source | Actor | Working directory | Env behavior | Log behavior |
|---------|----------------|-------|-------------------|--------------|--------------|
| `work_query` via `gc hook` and controller probes | Agent config | Trusted operator or pack | Agent's canonical city or rig repo | Inherited secrets are stripped; Gas City projects explicit store/session env. | Errors are diagnostic only. Avoid placing secrets in command literals. |
| `scale_check` | Agent config | Trusted operator or pack | Agent's canonical city or rig repo | Inherited secrets are stripped; Gas City projects explicit store env. | Parse failures include command context; command literals must not contain secrets. |
| `on_boot` and `on_death` | Agent pool config | Trusted operator or pack | City or rig repo | Inherited secrets are stripped; explicit store env may be provided when needed. | Hook failures are logged; output should not include secrets. |
| Order `check` triggers | Order config | Trusted operator or pack | Order target scope | Inherited secrets are stripped; explicit condition env may be provided. | Failure reason records exit status, not command output. |
| Order `exec` | Order config | Trusted operator or pack | Order target scope | Inherited secrets are stripped; explicit order env may be provided. | Failure errors and output are redacted before logs/events. |
| `gc sling` and `/sling` command runner | Sling target config | Trusted operator or pack | City or rig repo | Inherited secrets are stripped; explicit routing/store env may be provided. | Returned command output is caller-visible. Do not route untrusted text into shell. |
| Agent `command` | Agent config | Trusted operator or pack | Session work directory | Session env is explicit runtime env plus configured env. Secrets may be passed only by intentional config. | Agent stdout/stderr is session output and may be visible to operators. |
| `pre_start` | Agent config | Trusted operator or pack | Session work directory | Provider-specific runtime env; intended for setup before session start. | Provider warnings should avoid secrets. |
| `session_setup`, `session_setup_script`, `session_live` | Agent config | Trusted operator or pack | Running session environment | Provider-specific runtime env; remote providers run inside the target container or pod. | Provider warnings should avoid secrets. |
| `exec:` session provider | User-supplied provider script | Trusted operator code | Provider-defined | Direct exec, not `sh -c`; start config is JSON on stdin. | Provider stderr may be surfaced in errors. Do not print secrets. |
| `exec:` beads, mail, and events providers | User-supplied provider script | Trusted operator code | Provider-defined | Direct exec, not `sh -c`; request data is stdin/argv. | Provider stderr may be surfaced in errors. Do not print secrets. |
| Pack fetch/include, Git probes, Docker, Dolt, tmux, kubectl, `bd` helpers | Gas City code plus configured paths/URLs | Maintainer-reviewed code paths | Command-specific | Direct exec with argv except provider setup scripts where documented. | Errors are surfaced for diagnosis; avoid embedding credentials in URLs. |

## Secret Propagation

Controller-side shell helpers remove inherited environment variables whose keys
look secret-bearing, including names containing `TOKEN`, `PASSWORD`, `SECRET`,
`PRIVATE_KEY`, `API_KEY`, `ACCESS_KEY`, `CREDENTIAL`, `OAUTH`, or `AUTH_JSON`.
This prevents ambient CI or maintainer shell secrets from reaching `work_query`,
`scale_check`, hooks, order checks, order exec commands, and sling helpers by
accident.

If a command truly needs a secret, pass it explicitly through the relevant city,
rig, provider, or workflow configuration. Explicit values are preserved because
they represent an operator decision, and failure logs redact known secret values
before writing order exec errors or events.

## Rules For Authors

- Do not put secrets directly in command strings. Use env variables or provider
  credential files.
- Do not interpolate bead content, PR text, mail, formula vars, branch names, or
  other user-controlled values into `sh -c` commands.
- When showing a command for a human to copy, build it from argv and quote each
  argument with Gas City's shell quoting helper.
- Keep `pull_request_target` workflows metadata-only. They may label or comment
  but must not checkout or run contributor code with privileged tokens.
- Prefer direct `exec.Command(..., args...)` style boundaries for new provider
  contracts. Use `sh -c` only for explicitly operator-authored shell snippets.
