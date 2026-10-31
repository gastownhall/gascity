// Command fakeacp is a scriptable reference ACP agent for integration tests.
//
// It speaks ACP JSON-RPC 2.0 over stdio: it answers initialize and
// session/new, echoes each session/prompt back as "echo: <text>" in an
// agent_message_chunk update, and answers the prompt with a stopReason. A
// stdin reader goroutine runs alongside one goroutine per prompt, so
// session/cancel and replies to the fake's own requests are handled while a
// prompt is in flight.
//
// Scenario flags script protocol behavior (turn delays, chunked replies,
// thoughts, tool calls, permission and filesystem requests, prompt errors,
// cancel and SIGINT handling); see parseOptions. With --log-dir the fake
// records what it received so tests can assert on the client's side of the
// conversation. By default SIGINT is ignored, mirroring real ACP agents for
// which Interrupt is a soft prompt cancel rather than session teardown, and
// SIGTERM exits.
package main

import (
	"fmt"
	"os"
)

func main() {
	opts, err := parseOptions(os.Args[1:])
	if err != nil {
		fmt.Fprintf(os.Stderr, "fakeacp: %v\n", err)
		os.Exit(2)
	}
	logs, err := newRecorder(opts.logDir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "fakeacp: %v\n", err)
		os.Exit(2)
	}
	a := newAgent(opts, newWriter(os.Stdout), logs)
	a.handleSignals()
	a.serve(os.Stdin)
}
