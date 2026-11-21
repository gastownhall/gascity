package main

import (
	"bufio"
	"context"
	"encoding/json"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/convergence"
)

// TestConvergeCreateTimedOutRequestIsSkipped: while the event loop is busy (a
// long controller tick), a `converge:` create gets "controller did not respond
// in time" once the reply wait expires. The request stays queued, so when the
// loop drains it later it must skip it rather than create a loop behind the
// caller's back; a caller that retries after the error then gets exactly one
// loop (gastownhall/gascity#8387).
func TestConvergeCreateTimedOutRequestIsSkipped(t *testing.T) {
	old := convergeReplyTimeout
	convergeReplyTimeout = 100 * time.Millisecond
	t.Cleanup(func() { convergeReplyTimeout = old })

	cr, store := setupConvergenceRuntime(t)
	payload := `{"command":"create","params":{"formula":"test-formula","target":"test-agent","max_iterations":"3"}}`

	// Attempt 1: the event loop is inside a tick and does not read the channel.
	server, client := net.Pipe()
	go func() { handleConvergeSocketCmd(server, payload, cr.convergenceReqCh); server.Close() }() //nolint:errcheck // test cleanup
	line, err := bufio.NewReader(client).ReadString('\n')
	if err != nil {
		t.Fatal(err)
	}
	client.Close() //nolint:errcheck // test cleanup
	var first convergenceReply
	if err := json.Unmarshal([]byte(line), &first); err != nil {
		t.Fatalf("decoding attempt 1 reply %q: %v", line, err)
	}
	if first.Error != "controller did not respond in time" {
		t.Fatalf("attempt 1 error = %q, want %q", first.Error, "controller did not respond in time")
	}

	// The tick ends; the event loop drains the queue it was given and must
	// skip the request whose caller already gave up.
	for len(cr.convergenceReqCh) > 0 {
		req := <-cr.convergenceReqCh
		r := cr.handleConvergenceRequest(context.Background(), req)
		if !strings.Contains(r.Error, "request abandoned") {
			t.Errorf("queued %q processed after caller timed out: error=%q result=%s, want a \"request abandoned\" error", req.Command, r.Error, r.Result)
		}
	}

	// Attempt 2: the caller retries the create it was told had failed.
	var req convergenceRequest
	if err := json.Unmarshal([]byte(payload), &req); err != nil {
		t.Fatalf("decoding retry payload: %v", err)
	}
	r := cr.handleConvergenceRequest(context.Background(), req)
	if r.Error != "" {
		t.Fatalf("retry create failed: %q", r.Error)
	}

	loops, err := store.List(beads.ListQuery{Type: "convergence"})
	if err != nil {
		t.Fatal(err)
	}
	active := 0
	for _, b := range loops {
		t.Logf("convergence %s status=%s state=%s formula=%s target=%s", b.ID, b.Status,
			b.Metadata[convergence.FieldState], b.Metadata[convergence.FieldFormula], b.Metadata[convergence.FieldTarget])
		if b.Status != "closed" {
			active++
		}
	}
	if active != 1 {
		t.Errorf("one create was reported failed and one retried, but %d loops are live for the same formula and target", active)
	}
}
