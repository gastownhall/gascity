# Plan: Extract Shared Object Model

## Status: Complete

## Architecture

```
cmd/gc/cmd_*.go               internal/api/handler_*.go
  (arg parsing,                 (HTTP routing,
   text formatting,              JSON serialization,
   exit codes)                   status codes)
        \                              /
         \                            /
          v                          v
   internal/sling/        internal/convoy/
   internal/agentutil/    internal/graphroute/
   internal/pathutil/
            |
            v
   internal/{beads,config,formula,molecule,agent,events,...}
```

## Domain Packages

### internal/sling/ -- work routing

**Intent-based API** (new):
```go
s, _ := sling.New(deps)           // validate once
s.RouteBead(ctx, beadID, target, opts)
s.LaunchFormula(ctx, name, target, opts)
s.AttachFormula(ctx, name, beadID, target, opts)
s.ExpandConvoy(ctx, convoyID, target, opts, querier)
```

Each method takes exactly the params it needs via focused option
structs (`RouteOpts`, `FormulaOpts`). No flag bag.

**Typed routing** (new):
```go
type BeadRouter interface {
    Route(ctx, RouteRequest) error
}
```
Domain says "route this bead to this target." Implementation decides
how (shell command, direct store, API call).

**Legacy API** (preserved for backward compat):
`DoSling(SlingOpts, SlingDeps, querier)` and `DoSlingBatch` still
work. New methods delegate to them internally.

### internal/graphroute/ -- graph decoration

380 lines of graph.v2 routing extracted from sling. Owns step
binding resolution, cycle detection, control-dispatcher routing.
Own `Deps` interface (`AgentResolver` only).

### internal/convoy/ -- convoy CRUD

ConvoyCreate, ConvoyProgress, ConvoyAddItems, ConvoyClose with
event emission via `events.Recorder`.

### internal/agentutil/ -- agent resolution + pool expansion

Options-driven `ResolveAgent`, `ExpandAgents`, `ScaleParamsFor`,
`LookupSessionName`, `IsMultiSessionAgent`, `DeepCopyAgent`.

### internal/pathutil/ -- path utilities

`NormalizePathForCompare`, `SamePath`. Shared by 11+ CLI files.

## Design Principles

- **Intent-based API**: callers express intent, not flags
- **Typed routing**: domain describes what, implementation decides how
- **Structured data**: domain returns typed fields, callers format
- **Narrow interfaces**: AgentResolver, BranchResolver, Notifier,
  BeadRouter
- **Deps validated at construction**: New() checks required fields
- **No I/O in domain**: zero fmt.Fprintf, zero io.Writer
- **Graph routing separated**: own package, own tests, own deps
- **Backward compat**: old DoSling/DoSlingBatch preserved during
  caller migration

## Caller Migration Status

**API handler**: Fully migrated. Creates `sling.New(deps)` and
dispatches to `sl.RouteBead`/`sl.LaunchFormula`/`sl.AttachFormula`
based on request body intent. No SlingOpts in the API.

**CLI**: Partially migrated. Creates `sling.New(deps)` for
validation. Batch plain-bead routes via `sl.ExpandConvoy`. Single
sling and formula batch still use legacy `DoSling`/`DoSlingBatch`
because CLI tests inject custom queriers. Full migration requires
test infrastructure changes (queriers that use deps.Store).

**Legacy API preserved**: `DoSling`, `DoSlingBatch`, `SlingOpts`
still exist. The intent methods delegate to them internally.
Delete once CLI tests are updated to use deps.Store as querier.
