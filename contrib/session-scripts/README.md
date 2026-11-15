# Session Scripts

Community-maintained session provider scripts for Gas City's exec session
provider. These are real implementations we ship, but they have external
dependencies and aren't the same support tier as `gc` itself.

See [Exec Session Provider](../../docs/reference/exec-session-provider.md)
for the protocol specification.

## Scripts

### gc-session-screen

GNU screen backend and the reference observe.v1 implementation: it is
v2-ready (`observe.v1`, `report-attachment`), `city-scoped` and `local`, and
`gc runtime conformance gc-session-screen` passes. Creates screen sessions,
sends keystrokes for nudge and interrupt to the exact `<pid>.<name>`
session, captures output via `hardcopy`, and stores each box's identity and
metadata in 0600 sidecar files. Each city gets its own private screen
socket directory, so its listing holds only the boxes gc started for it.

State lives in `<home>/.gcss`, where `<home>` is the user's home directory
from the password database, never from `XDG_RUNTIME_DIR`, `TMPDIR` or
`HOME`: the supervisor, cron jobs and sudo shells all resolve the same
directory, so none of them can read a running box as absent and start a
twin. `GC_EXEC_STATE_DIR` overrides it, and then must be identical for every
gc process of the city. It is a 0700 directory the script refuses to use if
it is a symlink or someone else's: to keep state elsewhere, set
`GC_EXEC_STATE_DIR` instead of linking `~/.gcss`. Each city has two
directories there, keyed by the host name and the city path (`<key>`, 8 hex
digits), so two hosts sharing a home never list (and wipe) each other's
boxes: `<key>` holds the city's screen sockets and nothing else, and
`<key>.meta` each box's metadata.

A screen socket path, `<state>/<key>/<pid>.<name>`, must fit 107 bytes, so
the state directory's length limits session names: counting the pid as 7
digits, a name fits in 89 bytes less the length of the state directory,
which by default is 83 less the length of `<home>`. Under `/home/ubuntu`
that is 71 bytes, enough for gc's pool session names
(`<template>-gcg-session-<32 hex>`) of templates up to 26 characters. A
start whose socket path would not fit fails before screen runs.

**Reconciler v2:** the script is v2-ready, so a city running it with
`[daemon] session_reconciler` unset runs reconciler v2 from its next
controller start, unless the city falls back to legacy for another reason
(the boot log and `gc doctor` say which); set
`session_reconciler = "legacy"` to keep legacy. Before switching a city with
running sessions, see "Switching an existing exec city to v2" in
`docs/reference/exec-session-provider.md`.

**Upgrading from an earlier version:** with the old script still in place,
stop the city (`gc stop <city>`: the controller and every session), then
replace the file and start the city again. The earlier version kept its
boxes in the user's default screen directory, which this version does not
list: replacing the file under a running controller starts every session a
second time.
For the same reason, stop the city before upgrading from a gc that does not
set `GC_EXEC_CITY_PATH`, or renaming the host: either changes the city's
key, and its running boxes would no longer be listed.

After a reboot the old sockets list as dead. A legacy start wipes a name
only dead sessions hold and starts it; a v2 (`fresh_only`) start refuses it
with exit 73 and leaves the removal to gc, by the dead box's object.
A name whose box is gone, or listed dead, is not free while the box's agent
survives (one that ignores the hangup outlives a SCREEN that quit or was
killed): each start records its window process (boot id, pid and start
time), and while that process runs the name observes `unknown`, `inventory`
lists it `unknown`, a start refuses it (exit 73 under `fresh_only`), and a
corpse kill of a box listed dead answers `live`. `stop` ends that agent, and
so does a zombie kill after it quits the box. A record from before a reboot
names no process and is never killed, and a zombie (`/proc` state `Z`)
counts as gone.
A start whose session does not appear within 2s ends its own SCREEN (found
by its exact argv in `/proc`) before it fails, so no box appears after the
failure.

**Dependencies:** `bash`, `screen`, `jq`, `flock`, procps `ps` and `kill`,
`awk`, `cksum`, `uname`

**Usage:**

```bash
export GC_SESSION=exec:/path/to/contrib/session-scripts/gc-session-screen
gc start my-city
```

**Parity with tmux provider:** The script implements the protocol,
including observe.v1, but does not include Gas Town theming (status bar
colors, role emoji, keybindings) or remain-on-exit and auto-respawn. See the
script header.

### gc-session-k8s (reference — prefer native provider)

Kubernetes backend via exec protocol. Runs each agent session as a K8s
Pod using `kubectl` subprocesses. This script is now a **reference
implementation** — prefer the native K8s provider (`GC_SESSION=k8s` or
`[session] provider = "k8s"`) which uses client-go for direct API calls
and eliminates all subprocess overhead. Pod manifests are compatible
between the two for mixed-mode migration.

**Dependencies:** `kubectl`, `jq`, `bash`

**Usage (legacy):**

```bash
export GC_SESSION=exec:/path/to/contrib/session-scripts/gc-session-k8s
export GC_K8S_IMAGE=myregistry/gc-agent:latest
gc start my-city
```

**Native provider (recommended):**

```bash
export GC_SESSION=k8s
export GC_K8S_IMAGE=myregistry/gc-agent:latest
gc start my-city
```

See [`contrib/k8s/`](../k8s/) for the K8s manifests, Dockerfiles and an
example city, and [`gc-controller-k8s`](gc-controller-k8s) for deploying the
controller as a pod. The native provider reads its settings only from
`GC_K8S_*` environment variables in the controller's environment, not from
a `[session.k8s]` table: `GC_K8S_NAMESPACE` (default `gc`), `GC_K8S_IMAGE`
(required to start a session), `GC_K8S_CONTEXT`, `GC_K8S_SERVICE_ACCOUNT`,
`GC_K8S_PREBAKED`, the resources `GC_K8S_CPU_REQUEST` (default `500m`),
`GC_K8S_MEM_REQUEST` (`1Gi`), `GC_K8S_CPU_LIMIT` (`2`) and `GC_K8S_MEM_LIMIT`
(`4Gi`), the JSON-valued `GC_K8S_NODE_SELECTOR`, `GC_K8S_TOLERATIONS` and
`GC_K8S_AFFINITY`, and `GC_K8S_PRIORITY_CLASS_NAME`.
