// Package legacyop is the operation journal's O0 front door for the legacy
// executors: the API and the CLI record each recordable request as an op
// before their legacy path runs it, and close the op from the legacy result
// after (engdocs/design/des-o0-api-shadow-ops.md §5).
//
// A Recorder runs under one rollout gate mode. Off records nothing and does
// no store I/O. Auto (shadow) records where the store can and otherwise lets
// the request run unrecorded, loudly: a telemetry count and the Degraded
// hook. Require (durable) refuses a request it cannot record before any legacy
// effect. Admit decides: execute (recorded or not) or replay a stored or
// in-flight outcome, and returns an error only for refusals a caller maps to
// its wire. Finish closes the op from the legacy result, or leaves a
// client-keyed op open for a same-key retry to adopt.
//
// The ops are acceptance records: their executor is legacy:api or legacy:cli,
// so a lane never takes one; only their legacy path, an API adopter (Adopt;
// the CLI never adopts) or the sweeper (Sweep, at finish_by) closes one.
// Delivered text never carries an op id, key or marker: this package never
// sees the text a runtime receives.
//
// The package is transitional and is deleted with the ops gates. It imports
// no internal/api, cmd/gc, internal/config or internal/events: its callers
// wire gates, events and their own sentinel errors in.
package legacyop
