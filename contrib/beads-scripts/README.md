# Beads Scripts

Community-maintained bead store provider scripts for Gas City's exec beads
provider. These are reference implementations that wrap external bead stores.

See [Exec Beads Provider](../../docs/reference/exec-beads-provider.md)
for the protocol specification.

## Scripts

### gc-beads-br

beads_rust (`br`) backend. Wraps the `br` CLI to provide full bead store
functionality with SQLite + JSONL backing.

**Dependencies:** `br` (beads_rust), `jq`, `bash`

**Usage:**

```bash
export GC_BEADS=exec:/path/to/contrib/beads-scripts/gc-beads-br
gc start my-city
```

Or in `city.toml`:

```toml
[beads]
provider = "exec:/path/to/contrib/beads-scripts/gc-beads-br"
```

**Label conventions:**

| Convention | Purpose |
|-----------|---------|
| `parent:<id>` | Tracks parent-child relationships (Children operation) |
| `meta:<key>=<value>` | Stores metadata (SetMetadata operation) |
| `needs:<step-id>` | Tracks step dependencies within molecules |

**Lifecycle operations:**

| Operation | Behavior |
|-----------|----------|
| `ensure-ready` | Exit 2 (br uses embedded SQLite, always ready) |
| `shutdown` | Exit 2 (no server process to stop) |

**Other optional operations:**

- `mol-cook` — composed in Go by `exec.Store` using Create calls; script
  returns exit 2 (not applicable)
- `init` — not needed; run `br init` separately if required
- `config-set` — not applicable
- `purge` — not supported; use `br` CLI directly for cleanup

### gc-beads-k8s

Kubernetes beads provider. Runs `bd` inside a lightweight "beads runner"
pod (`gc-beads-runner`) via `kubectl exec`. The pod connects to Dolt
running as a StatefulSet inside the cluster — no port-forwarding needed
from the controller's laptop.

**Dependencies:** `kubectl`, `jq`, `bash`

**Container requirements:** `bd`, `jq`, `bash`, `git` (same image as agent pods)

**Usage:**

```bash
export GC_BEADS=exec:/path/to/contrib/beads-scripts/gc-beads-k8s
export GC_K8S_IMAGE=myregistry/gc-agent:latest
gc start my-city
```

Or in `city.toml`:

```toml
[beads]
provider = "exec:/path/to/contrib/beads-scripts/gc-beads-k8s"
```

**Environment variables:**

| Variable | Default | Description |
|----------|---------|-------------|
| `GC_K8S_NAMESPACE` | `gc` | K8s namespace |
| `GC_K8S_CONTEXT` | current | kubectl context |
| `GC_K8S_IMAGE` | (required) | Container image (same as agent pods) |
| `GC_K8S_DOLT_HOST` | `dolt.gc.svc.cluster.local` | Dolt service DNS |
| `GC_K8S_DOLT_PORT` | `3307` | Dolt service port |

**Lifecycle operations:**

| Operation | Behavior |
|-----------|----------|
| `ensure-ready` | Create `gc-beads-runner` pod if not Running, wait for Ready, init `.beads/` |
| `shutdown` | `kubectl delete pod gc-beads-runner` |

**Other optional operations:**

- `mol-cook` — composed in Go by `exec.Store` using Create calls; script
  returns exit 2 (not applicable)
- `purge` — not supported in Phase 1; exit 2
