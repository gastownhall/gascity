// Package readywork answers the city-wide claimable-work question `gc ready`
// asks: which beads, across every store a city spreads its work over, are ready
// for a given selector, in what order, and how many.
//
// It is the one reader behind two surfaces. The CLI runs it in-process over the
// stores it opens (cmd/gc `gc ready`), and the supervisor runs it over the
// stores it already holds open (GET /v0/city/{cityName}/work/ready). Both
// surfaces therefore produce the same rows, in the same order, with the same
// fields, from the same code: CLI == API holds by construction rather than by
// two copies kept in step.
//
// The supervisor's GET /v0/city/{cityName}/beads/ready
// (internal/api humaHandleBeadReady) is a separate reader, and `gc ready` is
// specified against that handler's FEDERATION CONTRACT: the same legs in the
// same order, first-leg-wins dedupe. Where the two deliberately differ (the CLI
// reads only the serving rigs, fails where the API answers partially, and reads
// every leg live where the API prefers a warm ready cache) is stated there and
// in cmd/gc ready_federation.go, and the split-topology conformance row I13
// (conformanceCLIReadyFederation) asserts that the two answers agree.
//
// # What the caller supplies
//
// The caller assembles the legs: the city store, the serving rig stores, and a
// relocated class binding, in that order, from the city's storage plan (see
// cmd/gc ready_federation.go). This package never decides which stores exist;
// it reads the ones it is handed, in the order it is handed them.
//
// # Every read is live
//
// Each leg is read through its live handle (beads.HandlesFor(store).Live), never
// through a read cache. A row that becomes ready before a read starts is in that
// read's answer, whichever process wrote it. That is the guarantee the work
// queries a seat claims through depend on, and it is why a CachingStore handed
// in as a leg is read through to its backing store rather than answered from
// its snapshot: writes made by an agent's own `bd` reach that snapshot only at
// its next reconcile.
//
// # Fail loud
//
// A CLI work query has nowhere to say "this answer is short", so any leg that
// cannot be read fails the whole answer, naming the leg. The plan's
// PartialDegrade verdict on rig legs is escalated here, deliberately.
//
// # Pushdown
//
// The selective predicates (--assignee, --unassigned, plain-string
// --metadata-field key=value) and, under the two orders a store can reproduce,
// --limit are pushed into each leg's query. A store may narrow its answer by
// them, and never widen it; the predicates are always re-applied to the merged
// set here, which stays the authority. See pushdown.go for why each pushed
// predicate is no stricter than the Go filter, and for the limit's exactness
// rules.
package readywork
