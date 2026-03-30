# Events Scripts

Community-maintained events provider scripts for Gas City's exec events
provider. These store events in external infrastructure backends, selected
via `GC_EVENTS=exec:/path/to/script`.

See `internal/events/exec/exec.go` for the protocol specification.

## Scripts

### gc-events-k8s

Kubernetes backend. Stores events as ConfigMaps with label selectors for
efficient querying. Sequence numbers are tracked in a dedicated counter
ConfigMap with compare-and-swap updates for atomicity.

**Dependencies:** `kubectl`, `jq`, `bash`

**Usage:**

```bash
export GC_EVENTS=exec:/path/to/contrib/events-scripts/gc-events-k8s
export GC_K8S_NAMESPACE=gc    # optional, default: gc
gc start my-city
```

**Configuration:**

| Variable | Default | Description |
|----------|---------|-------------|
| `GC_K8S_NAMESPACE` | `gc` | K8s namespace for event ConfigMaps |
| `GC_K8S_CONTEXT` | current | kubectl context to use |

**ConfigMap layout:**

- `gc-events-seq` — counter ConfigMap tracking the latest sequence number
- `gc-evt-0000000042` — one ConfigMap per event, with labels for type/actor

See [contrib/k8s/](../k8s/) for the full K8s setup.

## Testing

Each script has a companion `.test` file:

```bash
./contrib/events-scripts/gc-events-k8s.test
```

Tests use a mock kubectl (no cluster required).
