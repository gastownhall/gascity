# Hook continuation

Explicit continuation takes its root from `gc hook --claim --root`, never from
the session's mutable current-claim stamp. Apply the root predicate before
frontier selection, every claim tier, continuation reservation and delivery.
Scoped v2 results distinguish live-root wait from terminal completion and never
acknowledge drain or fall back to generic acquisition. Store failures are errors;
unscoped acquisition retains the v1 contract.

Resolve scoped candidates against the current work leg before domain checks.
Only proven `ErrNotFound` permits trying the next leg; every other read error
fails the request. Input and workflow ancestry checks must succeed before and
after claiming, and sibling ancestry/teardown checks must succeed before
reservation. Complete scoped domain and reservation checks before publishing
execution stamps, started records or work results. Unscoped v1 retains its
existing read-error behavior.
