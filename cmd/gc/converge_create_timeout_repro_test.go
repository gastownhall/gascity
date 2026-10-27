package main

import (
	"bufio"
	"context"
	"encoding/json"
	"net"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/convergence"
)

// TestReproConvergeCreateTimeoutStillCreates: while the event loop is busy (a
// long controller tick), a `converge:` create gets "controller did not respond
// in time" after 60 s, but the request stays queued and the loop creates it
// later. A caller that retries after the error gets a second loop.
func TestReproConvergeCreateTimeoutStillCreates(t *testing.T) {
	cr, store := setupConvergenceRuntime(t)
	payload := `{"command":"create","params":{"formula":"test-formula","target":"test-agent","max_iterations":"3"}}`

	// Attempt 1: the event loop is inside a tick and does not read the channel.
	server, client := net.Pipe()
	go func() { handleConvergeSocketCmd(server, payload, cr.convergenceReqCh); server.Close() }()
	start := time.Now()
	line, err := bufio.NewReader(client).ReadString('\n')
	if err != nil {
		t.Fatal(err)
	}
	client.Close()
	var first convergenceReply
	_ = json.Unmarshal([]byte(line), &first)
	t.Logf("attempt 1 reply after %s: error=%q", time.Since(start).Round(time.Second), first.Error)
	t.Logf("requests still queued for the event loop: %d", len(cr.convergenceReqCh))

	// The tick ends; the event loop drains the queue it was given.
	for len(cr.convergenceReqCh) > 0 {
		req := <-cr.convergenceReqCh
		r := cr.handleConvergenceRequest(context.Background(), req)
		t.Logf("event loop processed the queued %q after the client was told it failed: error=%q result=%s", req.Command, r.Error, r.Result)
	}

	// Attempt 2: the caller retries the create it was told had failed.
	var req convergenceRequest
	_ = json.Unmarshal([]byte(payload), &req)
	r := cr.handleConvergenceRequest(context.Background(), req)
	t.Logf("attempt 2 (retry) reply: error=%q result=%s", r.Error, r.Result)

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
	if first.Error != "" && active != 1 {
		t.Errorf("one create was reported failed and one retried, but %d loops are live for the same formula and target", active)
	}
}
