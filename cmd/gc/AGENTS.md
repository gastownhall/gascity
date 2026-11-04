# Hook continuation

Explicit continuation takes its root from `gc hook --claim --root`, never from
the session's mutable current-claim stamp. Apply the root predicate before
frontier selection, every claim tier, continuation reservation and delivery.
Scoped v2 results distinguish live-root wait from terminal completion and never
acknowledge drain or fall back to generic acquisition. Store failures are errors;
unscoped acquisition retains the v1 contract.
