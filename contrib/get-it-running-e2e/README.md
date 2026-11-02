# Get-it-running end-to-end check

Proves that a coding agent handed this repository, and nothing else, can get
Gas City running: install the latest Homebrew release, create a city, add a
sample rig, send work through a formula, and give the user a dashboard link to
the completed run. Run it whenever the README's "Get it running" section, the
docs quickstart, or a release candidate changes.

Each variant runs Claude Code headless (`--dangerously-skip-permissions`) in a
fresh Ubuntu 24.04 Incus container that has Homebrew, git with a global
identity, and Claude Code, but no Gas City and no Dolt config:

| Variant | Starts in | Prompt |
|---|---|---|
| L1 | empty home | `Get this running: <repo url>` |
| L2 | empty home | `Set up <repo url> on my machine and show me it working` |
| L3 | empty home | `I want to try <repo url>. Install it and get it going.` |
| C1 | fresh clone | `get this running` |
| C2 | fresh clone | `set this up for me` |
| C3 | fresh clone | `how do I use this? get it going` |

A variant passes when the agent's final message links a run
(`http://127.0.0.1:8372/city/<city>/runs/<id>`), the city API reports that run
`completed`, and `hello.sh` is committed in the rig. Agent questions to the
user are counted in `grade.json` (`ask_user_calls`).

## Running it

On a Linux host with Incus (one round uses about 6 containers, 7 minutes, and
$2 of model usage):

```bash
./provision-image.sh                      # once; publishes the gce2e-base image
mkdir -p ~/gce2e && umask 077
# Copy a logged-in Claude Code credential to ~/gce2e/.cred.json. On macOS:
#   security find-generic-password -s "Claude Code-credentials" -w | ssh <host> 'cat > ~/gce2e/.cred.json'
GCE2E_BRANCH=main ./run-round.sh 1        # results in ~/gce2e/results/r1/<variant>/
```

Each variant directory holds `transcript.jsonl` (the agent's stream-json
output), `final.txt`, `run-api.txt`, `harness.log`, and `grade.json`. Failing
containers are kept as `gce2e-r<round>-<variant>` for inspection; delete them
with `incus delete -f`. Delete `~/gce2e/.cred.json` when you are done.

Six containers each running a city can exhaust the default per-user inotify
instance limit (128); systemd in a container then fails to boot with "Too many
open files" and the variant is graded `HARNESS-ERROR`. Raise it with
`sudo sysctl -w fs.inotify.max_user_instances=1024`.

## Tutorial walkthrough

`run-tutorials.sh` checks that the user docs work with the latest Homebrew
release, the way a reader follows them. In one fresh `gce2e-base` container it
runs one headless Claude Code session per page, in reading order:
Installation, Quickstart, then Tutorials 01 through 07. Each session follows its
page literally, building on the machine state the earlier pages left, and logs
every step to a JSONL report as `worked`, `failed`, `improvised` (it had to do
something the page did not say), or `skipped` (an alternative install method,
an optional step, or one that needs an interactive terminal).

A page passes when it logged steps and none failed or were improvised. Run it
whenever a page under `docs/getting-started/` or `docs/tutorials/` changes, or
after a release:

```bash
./run-tutorials.sh 1 ../../docs           # results in ~/gce2e/results/t1/
```

A round takes 30 to 40 minutes. The results directory holds `harness.log` (the
per-page verdicts and every step that did not work, with the agent's note on
what the page should say), and per page `NN-<page>.jsonl` (the step report),
`NN-<page>.transcript.jsonl`, and the prompt. The container is kept as
`gce2e-t<round>` for inspection; delete it with `incus delete -f`.
