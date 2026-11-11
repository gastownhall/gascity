# fx provider profiles (city.toml fragment)

Copy `city-fragment.toml` (the two `[providers.*]` blocks) into a city's
`city.toml` — the gated demo city is
`demo/gated-dependencies/container/city.toml` in the delivery repo. Both
providers are standalone: they do not derive from a builtin provider.

## Using them

- Select `provider = "fx"` for a long-lived role and `provider = "fx-ask"` plus
  `lifecycle = "one_shot"` for a role that runs bounded work and exits. Neither
  reads a `start_command`.
- Model, effort and permission mode come from the settings file of the
  environment fx runs in (`~/.fx/settings.json` under that `HOME`). This build
  rejects model/permission flags placed before the TUI's positional prompt, so
  do not model them as provider args for `fx`; for `fx-ask` they may go in
  `args` after `ask`.
- `--no-save` means the headless variant cannot resume. `-r` on the TUI needs a
  session id discovered after a turn (`fx sessions --json`); Core records
  `resume_flag` but does not discover ids for this provider.
- A one-shot provider under `[[named_session]] mode = "always"` is a test
  shape, not a deployment: the controller recreates the session after each
  clean exit and quarantines it after repeated deaths. Route real headless work
  through a pool/rig agent instead.
- `supports_hooks = false` is load-bearing: it is what makes Core compose
  `gc prime --hook` (preview form) into the launch prompt, because fx has no
  user-registerable hook. See `engdocs/design/hookless-startup-context.md`.

## Proving a profile starts (model-free shape used for this fragment)

Run the city under a private `GC_HOME` so the host's supervisor registry and API
port are untouched, and use a file bead store:

```sh
export GC_HOME=/tmp/fx-gc-home GC_BEADS=file
gc start "$CITY"                    # or a controller you already run
# then a desired session (named_session, pool or sling) launches fx
tmux -L <city socket> list-panes -a -F '#{pane_start_command}'
```

Expected: the TUI pane runs `fx` and shows the typed prompt and its answer; the
headless pane runs `fx ask --json --no-save --no-color '<prompt>'` as one argv
element, prints the JSON result, and exits 0 (`pane_dead=1`).

Do not put credentials in this fragment. A stub model on loopback
(`REQDUMP` captures request bodies) is enough to prove the transport and the
prompt actually sent.
