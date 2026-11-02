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
`gc bd show <id>`. If the result action is `drain`, what you do next
depends on how you called the hook:

- A drain-aware call (`gc hook --claim --drain-ack --json`) acknowledges
  drain for you when it can. Exit status 0 means your session is done —
  exit. The JSON line carries `drain_acknowledged=true` when the ack was
  recorded; a tolerated refusal also exits 0 without it — leave anyway,
  the controller stops the seat through its own drain. Exit status 1
  means the acknowledgement failed: run `gc runtime drain-ack` yourself,
  then exit. A drain whose reason is `drain_pending` says the same: run
  `gc runtime drain-ack`, then exit.
- A plain poll (`gc hook --claim --json`, without `--drain-ack`)
  acknowledges nothing: its `drain` answer only reports that no work is
  available right now. Follow your prompt's completion guidance —
  re-check, or drain through the drain-aware form — instead of treating
  the plain poll's answer as a session exit.

A call that fails, or that prints no JSON line, is a fault — report it;
do not fabricate a drain from it.

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
