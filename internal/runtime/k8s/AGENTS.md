# Native Kubernetes session provider

The native provider's default agent Pods must satisfy Restricted Pod Security:
run as non-root, disable privilege escalation, drop all capabilities, and use
the RuntimeDefault seccomp profile. Leave UID, primary GID, and `fsGroup`
unset unless the city config or operator explicitly supplies them. Gas City does
not allocate UIDs; Kubernetes admission may assign one.

The agent image uses numeric `1001:0` as its non-root fallback. Writable image
paths use group 0 with group permissions matching the owner permissions. When
`[session.k8s] run_as_group` or `GC_K8S_RUN_AS_GROUP` selects a nonzero primary
GID, preserve that GID and add supplementary GID 0 so HOME and prebaked
workspace remain writable. Preserve any configured `fsGroup`; staged `emptyDir`
workspaces need it or a cluster-assigned `fsGroup` so kubelet applies writable
volume ownership. Do not make directories world-writable or add runtime chown to
the normal non-legacy path.

`LINUX_USERNAME` is a compatibility exception: its existing startup path needs
UID 0 for account creation, sudoers setup, ownership changes, and `su`. Keep
standalone use intact, but reject it when any UID, GID, or `fsGroup` is
explicitly configured through TOML or `GC_K8S_*` environment overrides. Do not
describe those Pods as Restricted-compatible.
