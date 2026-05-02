# Event Query Primitives

The `events` package provides a read-only query layer over the event bus.
These primitives are pure functions — no I/O, no subscriptions — making them
easy to compose and test.

## Extended Filter

`Filter` now supports six predicates plus a result cap:

```go
type Filter struct {
    Type     string    // match events with this Type (e.g. "bead.created")
    Actor    string    // match events with this Actor
    Subject  string    // match events with this Subject (e.g. a bead ID)
    Since    time.Time // match events at or after this time (inclusive)
    Until    time.Time // match events at or before this time (inclusive)
    AfterSeq uint64    // match events with Seq > AfterSeq
    Limit    int       // cap results at this count (0 or negative = unlimited)
}
```

Zero values are always ignored, so existing callers that set only `Type` or
`Actor` continue to work without change.

### Subject filter

The most common diagnostic query: "what happened to bead gc-42?"

```go
evts, err := provider.List(events.Filter{Subject: "gc-42"})
```

### Until filter

Pair `Since` and `Until` to query a time window:

```go
evts, err := provider.List(events.Filter{
    Since: start,
    Until: end,
})
```

### Limit

`Limit` caps the result slice to the first N matches in chronological scan
order and stops scanning as soon as the cap is reached when the provider can do
so locally. This is the earliest matching window, not the latest N events; use
`ListTail` or caller-side tail slicing when a view needs the trailing window:

```go
firstCreated, err := provider.List(events.Filter{
    Type:  events.BeadCreated,
    Limit: 10,
})
```

For `Multiplexer` calls, `Limit` is applied after provider results are merged
and sorted by timestamp, city, then sequence. That preserves one deterministic
global earliest-window ordering across cities, but it also means the cap does
not bound each provider's local scan work.

## Aggregation Helpers

Three pure functions produce frequency maps over a `[]Event` slice:

```go
// CountByType returns type → count.
func CountByType(evts []Event) map[string]int

// CountByActor returns actor → count.
func CountByActor(evts []Event) map[string]int

// CountBySubject returns subject → count.
func CountBySubject(evts []Event) map[string]int
```

These are intentionally simple. The caller drives composition:

```go
all, _ := provider.List(events.Filter{Since: yesterday})
byType := events.CountByType(all)
// byType["bead.created"] == 17
// byType["session.woke"] == 5
```

## Implementation

| Artifact | Purpose |
|---|---|
| `internal/events/reader.go` | `Filter` extended with `Subject`, `Until`, `Limit`; `matchesFilter` helper; `ReadFiltered` updated |
| `internal/events/fake.go` | `Fake.List` updated to use `matchesFilter` and apply `Limit` |
| `internal/events/exec/exec.go` | Exec provider keeps the legacy script filter JSON shape and applies SDK-side filtering after script output so old scripts cannot bypass new filter fields |
| `internal/events/multiplexer.go` | Multiplexer applies `Limit` globally after deterministically merging and sorting provider results |
| `internal/events/query.go` | `CountByType`, `CountByActor`, `CountBySubject` |
| `internal/events/query_test.go` | Tests covering all new filter predicates and count helpers |

`matchesFilter` is the predicate used by `ApplyFilter`, `ReadFiltered`, and the
in-memory provider, ensuring code paths enforce the same predicate logic. The
`exec` provider still passes the legacy `Type`/`Actor`/`Since`/`AfterSeq`
filter shape to an external script as JSON for provider-side narrowing, but
asks scripts for an unbounded result set and then applies the SDK filter
locally. Scripts that don't recognize new fields can return unfiltered data;
the in-process caller enforces `Subject`, `Until`, and `Limit` on its side.
