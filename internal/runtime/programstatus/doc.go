// Package programstatus reads the OSC 7501 Program Status Protocol (spec
// rev 0.4) out of a terminal's output stream and keeps the reported records.
//
// The pieces compose into the capture tap's pipeline:
//
//   - [Scanner] reassembles OSC 7501 reports, support probes and OSC 133;A
//     prompt starts from a byte stream delivered in arbitrary reads (BEL or
//     ST terminated, optionally wrapped in tmux DCS passthrough).
//   - [ProbeMatcher] is the minimal probe detector the probe-answering relay
//     runs in the agent's data path; it agrees with the Scanner on every
//     stream.
//   - [ParseBody] validates one report against the spec's grammar and
//     limits; a report that breaks a rule is dropped whole, never guessed.
//   - [Store] applies reports with the spec's record semantics and keeps the
//     root record's turn counters.
//   - [Encode], [Decode], [EncodeText] and [DecodeText] are the pane-option
//     codec gc reads the published record through.
//
// The package imports only the standard library and internal/runtime.
package programstatus
