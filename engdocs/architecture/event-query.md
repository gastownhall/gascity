# Event Query Primitives

The `events` package provides a read-only query layer over the event bus.
These primitives are pure functions — no I/O, no subscriptions — making them
easy to compose and test.

## Extended Filter

`Filter` now supports five predicates instead of four:

```go
type Filter struct {
    Type     string    // match events with this Type (e.g. "bead.created")
    Actor    string    // match events with this Actor
    Subject  string    // match events with this Subject (e.g. a bead ID)
    Since    time.Time // match events at or after this time (inclusive)
    Until    time.Time // match events at or before this time (inclusive)
    AfterSeq uint64    // match events with Seq > AfterSeq
    Limit    int       // cap results at this count (0 = unlimited)
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

`Limit` caps the result slice and stops scanning as soon as the cap is reached.
Useful for dashboards that only need the first N matches:

```go
recent, err := provider.List(events.Filter{
    Type:  events.BeadCreated,
    Limit: 10,
})
```

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
| `internal/events/query.go` | `CountByType`, `CountByActor`, `CountBySubject` |
| `internal/events/query_test.go` | 11 tests covering all new filter predicates and count helpers |

`matchesFilter` is an unexported helper shared by `ReadFiltered` and
`Fake.List`, ensuring both code paths enforce the same predicate logic.
The `exec` provider passes `Filter` to an external script as JSON — new fields
are marshalled automatically; scripts that don't recognize them return
unfiltered data (the in-process caller applies the filter on its side).
