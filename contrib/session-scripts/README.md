# Session Scripts

Community-maintained session provider scripts for Gas City's exec session
provider. These are real implementations we ship, but they have external
dependencies and aren't the same support tier as `gc` itself.

See [Exec Session Provider](../../docs/reference/exec-session-provider.md)
for the protocol specification.

## Scripts

### gc-session-screen

GNU screen backend. Creates screen sessions, sends keystrokes for nudge
and interrupt, captures output via `hardcopy`, and stores metadata in
sidecar files.

**Dependencies:** `screen`, `jq`, `bash`

**Usage:**

```bash
export GC_SESSION=exec:/path/to/contrib/session-scripts/gc-session-screen
gc start my-city
```

**Parity with tmux provider:** The script implements the full 13-operation
protocol but does not yet include Gas Town theming (status bar colors,
role emoji, keybindings) or lifecycle features (remain-on-exit, auto-respawn,
zombie detection). See comments in the script header for the full gap list.

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
