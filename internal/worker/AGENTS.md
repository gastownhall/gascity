# internal/worker — change guide

`internal/worker/handle.go` is the canonical boundary for session creation
and lifecycle operations. Callers outside `internal/session` reach sessions
through `worker.Handle`, not through `session.Manager`.

## Active migration: worker boundary

Started `12a0a848` on Apr 17 2026, in progress. New code on affected paths
must take the canonical route, not the legacy route.

Production `cmd/gc/*.go` files must route through `worker.Handle` — enforced
by `TestGCNonTestFilesStayOnWorkerBoundary` in
`cmd/gc/worker_boundary_import_test.go`, which forbids non-test files from
importing `session.NewManagerWithOptions(`, `worker.SessionHandle`,
`sessionlog`, and similar bypass paths in `cmd/gc`. The remaining
manager-construction/direct-create bypasses are split by category:
`internal/api/session_manager.go` constructs `session.Manager` values for API
handlers. (`internal/api/session_resolution.go`'s named-session create was
converted to the worker boundary — it now routes through
`worker.Handle.Create(ctx, worker.CreateModeStarted)` via
`newResolvedWorkerSessionHandle`, no longer calling `mgr.CreateSession(...)`
directly.) Session creation goes through the single
`Manager.CreateSession(ctx, session.CreateOptions{...})` entry point
(`NewManagerWithOptions` is the sole Manager constructor). This list is not a
sessionlog read-site inventory; stream and transcript readers in
`internal/api/` and `internal/session/` still read session logs directly.
Package-internal helpers in `internal/session/` may construct and use
`session.Manager`; tests may construct it directly. Do not add new non-test
direct `session.Manager.CreateSession` call sites outside the worker boundary.
