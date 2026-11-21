# internal/cliauth — change guide

## Vendor-neutral hosted-service wire

The OSS client of a hosted Gas City service (`internal/cliauth`, the
`gc login`/`gc whoami` commands in `cmd/gc/cmd_login.go`) speaks a generic,
published protocol (`docs/reference/specs/service-protocol-v0.md`) and holds
an **opaque bearer** it never parses. Account/commercial policy — trial,
billing, credit, plan, quota, org/tenant identity — must **never** be a wire
field; it travels only in the opaque server-authored `message`/`links`
fields the CLI prints verbatim (spec §5). Default endpoint URLs (e.g.
`defaultServiceURL = "https://gascity.com"`) are **configuration data, not
commercial code** — sanctioned exactly like the pack-registry default.
Enforced by `scripts/check-core-boundary.sh` check (f) and the
`internal/cliauth` wire golden test; all provisioning/billing/trial logic
lives server-side in the private hosted repos.
