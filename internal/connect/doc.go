// Package connect is the contract between a controller that mints worker-site
// credentials and the box that consumes them through `gc connect`.
//
// It owns four wire forms, all pure data with no I/O:
//
//   - the connect bundle (Bundle, EncodeBundle, DecodeBundle), the secret
//     JSON document a controller hands a box through its runtime provider;
//   - the per-box bearer and the token-file sentinel (MintBearer,
//     ParseBearer, Sentinel), HMAC-SHA256 values keyed by the city's key;
//   - the worker-site descriptor `.gc/control-plane.toml` (Descriptor,
//     EncodeDescriptor, DecodeDescriptor), which marks a city directory as a
//     worker site and records where its ledger lives;
//   - the typed refusals (Refusal and its stable Code values) that `gc
//     connect` reports in its JSON error envelope.
//
// Nothing in this package reads files, dials servers, or holds a key. The
// controller side (minting, token-file rendering) and the box side (`gc
// connect`, worker-site mode) build on these types.
package connect
