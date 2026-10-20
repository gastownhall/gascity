// Package legacyop is the operation journal's O0 front door for the legacy
// executors: the API and the CLI record each recordable request as an op
// before their legacy path runs it, and close the op from the legacy result
// after (engdocs/design/des-o0-api-shadow-ops.md §5; the comments here cite it
// as DES-O0API). That document is not on main yet; until it lands, the doc
// comments here and the tests that pin them are the in-tree contract.
//
// A Recorder runs under one rollout gate mode. Off records nothing and does
// no store I/O. Auto (shadow) records where the store can and otherwise lets
// the request run unrecorded, loudly: a telemetry count, a log line rate
// limited per (surface, stage, reason), and the Degraded hook, which the
// caller turns into its own signal (the API's ops.recording_degraded event;
// the CLI's one warning naming the stage, which the CLI glue prints, so it
// gives the recorder a discarding Logger). Require (durable) refuses a
// request it cannot record before any legacy effect. Admit decides: execute
// (recorded or not) or replay a stored or in-flight outcome, and returns an
// error only for refusals a caller maps to its wire. Finish closes the op
// from the legacy result, or, on the API, leaves a client-keyed op open for a
// same-key retry to adopt before its finish_by; CLI ops always close.
//
// The ops are acceptance records: their executor is legacy:api or legacy:cli,
// so a lane never takes one; only their legacy path, an API adopter (Adopt;
// the CLI never adopts), the sweeper (Sweep, past finish_by) or a same-key
// Admit past finish_by closes one.
// Delivered text never carries an op id, key or marker: this package never
// sees the text a runtime receives.
//
// The package is transitional and is deleted with the ops gates. It imports
// no internal/api, cmd/gc, internal/config or internal/events: its callers
// wire gates, events and their own sentinel errors in.
package legacyop
