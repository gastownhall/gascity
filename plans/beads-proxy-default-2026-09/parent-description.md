## Problem

Fresh Gas City Beads/Dolt scopes need to use the bd 1.3 proxied-server path
by default for its performance and lifecycle benefits without changing the
behavior of existing direct, embedded, or DoltLite scopes. Operators need
explicit direct escape hatches and an opt-in proxy fronting an external Dolt
server.

## Release posture

Fresh bd/Dolt initialization with no topology intent is proxied-local: bd owns
the local proxy and child Dolt server. Explicit provider-neutral init intent
uses `transport=direct|proxied` and `target=local|external`; the supported
external proxy shape is a local bd-managed proxy fronting an externally managed
Dolt server. A remotely hosted proxy service is unsupported. Existing direct
local/remote, embedded, DoltLite, and proxied scopes remain authoritative and
are not automatically converted. Direct mode is an explicit recovery escape
hatch.

## Scope

Keep Gas City provider-agnostic. `gc init` delegates creation and lifecycle to
the selected beads provider; bd writes its own config, metadata, client-info,
endpoint state, and provider-owned process controls. Gas City persists only a
provider-neutral lifecycle-owner marker where necessary. Implement persisted
state authority, policy-only inheritance, precedence and ambient-environment
isolation, fail-closed readiness/refusal behavior, retry-safe provider
initialization, and real front-door parity/refusal coverage. Track existing
history/workflow/tracker/pack gaps as dependent release gates.
