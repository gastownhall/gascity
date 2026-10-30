// Package connect is the contract between a controller that mints worker-site
// credentials and the box that consumes them through `gc connect`.
//
// It owns five wire forms, all pure data with no I/O:
//
//   - the connect bundle (Bundle, EncodeBundle, DecodeBundle), the secret
//     JSON document a controller hands a box through its runtime provider;
//   - the per-box bearer and the token-file sentinel (MintBearer,
//     ParseBearer, Sentinel), HMAC-SHA256 values keyed by the city's key;
//   - the worker-site descriptor `.gc/control-plane.toml` (Descriptor,
//     EncodeDescriptor, DecodeDescriptor), which marks a city directory as a
//     worker site and records where its ledgers live: one per served
//     storage binding the box uses, each with the classes it serves and the
//     workspaces attached to it;
//   - the rendered bd serve token file (RenderTokenFile, ParseOperatorLines,
//     InspectTokenFile): the operator's lines, the sentinel, then one bearer
//     per eligible session row, as a pure function of those inputs;
//   - the typed refusals (Refusal and its stable Code values) that `gc
//     connect` reports in its JSON error envelope.
//
// Nothing in this package reads files, dials servers, or holds a key. The
// controller side (minting; the token-file renderer's I/O in
// internal/connect/tokenrender) and the box side (`gc connect`, worker-site
// mode) build on these types.
package connect
