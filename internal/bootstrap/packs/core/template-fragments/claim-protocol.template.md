{{ define "claim-protocol" -}}
## Startup Protocol

Find and claim your work with the hook. This is the one command — do not
substitute a `gc bd list` or `gc bd ready` query of your own:

```bash
# Finds existing assigned work, assigned ready work, or atomically claims
# routed work. If nothing is available, it acknowledges runtime drain.
gc hook --claim --drain-ack --json
```

If the result action is `work`, read the returned `bead_id` with
`gc bd show <id>`.

If the result action is `drain`, nothing is yours to run. What to do
depends on the call that returned it:

- **A drain-aware call** (with `--drain-ack`, like the one above): if the
  command exited 0, your session is done — exit. `drain_acknowledged: true`
  in the result confirms the acknowledgement; when the seat's drain-ack was
  refused as tolerated, the field is absent and the command still exits 0,
  so you still exit. If the command exited non-zero, the acknowledgement
  failed: run `gc runtime drain-ack`, then exit.
- **A plain poll** (`gc hook --claim --json`, without `--drain-ack`):
  nothing was acknowledged, and the command exits non-zero by design. It
  does not end your session by itself. If your instructions poll this way,
  they say when to re-check and when to drain.

A hook call that fails without printing a JSON result is a fault, not a
drain: do not treat it as one.

**Never look for your work with `gc bd list --assignee=<your alias>` or
`gc bd ready`.** Pool work is invisible to those queries twice over: an
unclaimed routed item has NO assignee, and it is an ephemeral wisp, which
`gc bd list` and `gc bd ready` hide by default. A worker that checks that way
reports "no work" while its own wisp sits open — that is how a scheduled
maintenance order once went 41 hours without running. Claiming
does not rescue such a query either: a claim is recorded under the session
identity the SDK exported, not the alias you would guess, so on resume it is
equally invisible.

There is no shorter query to fall back to. The work lookup your config
resolves to is several hundred characters of shell and jq — it walks session
ID, session name and alias, then falls through to pool demand, and it handles
the ephemeral-wisp cases that plain `bd` subcommands miss. `gc hook` exists so
that you never have to reproduce it. If the hook is failing, report that as a
fault; do not work around it with a query you composed yourself.
{{- end }}
