package api_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/api"
)

// requireIdempotency lists the POST operations that MUST accept an
// Idempotency-Key header: resource creates, and the session actions a client
// cannot safely retry without a key (a retried submit or message is delivered
// twice, a retried stop can interrupt a later turn, a retried wake records a
// second wake, a retried respond fails once the first one cleared the
// interaction, and a retried kill or reset can kill or reset the session the
// controller restarted after the first). Suspend and close take one too: the
// operation journal records every session lifecycle command under its key
// (DES-O0API §7.4). This set grows as each wiring slice (audit P0 #4) lands; a
// regression that drops the header fails
// TestCreateEndpointsAreTriagedForIdempotency.
var requireIdempotency = map[string]bool{
	"create-bead":             true,
	"create-session":          true,
	"send-mail":               true,
	"create-agent":            true,
	"create-provider":         true,
	"create-rig":              true,
	"create-convoy":           true,
	"add-pack":                true,
	"reply-mail":              true,
	"register-extmsg-adapter": true,
	"emit-event":              true,
	"post-v0-city":            true,
	"submit-session":          true,
	"send-session-message":    true,
	"respond-session":         true,
	"post-v0-city-by-city-name-session-by-id-stop":    true,
	"post-v0-city-by-city-name-session-by-id-wake":    true,
	"post-v0-city-by-city-name-session-by-id-kill":    true,
	"post-v0-city-by-city-name-session-by-id-close":   true,
	"post-v0-city-by-city-name-session-by-id-suspend": true,
	"post-v0-city-by-city-name-session-by-id-reset":   true,
}

// pendingIdempotency lists known create operations that are deliberately NOT
// yet wired for Idempotency-Key (audit P0 #4, slices S2+). It is a reviewed
// TODO list, not an exemption: when a slice wires one of these, MOVE it to
// requireIdempotency — the test enforces the move so the lists stay honest.
// Empty now that create-session is wired; keep it for the next create that
// lands before its key does.
var pendingIdempotency = map[string]bool{}

// exemptFromIdempotency lists POST operations that are NOT resource creates and
// are not required to take an Idempotency-Key: actions on an existing resource
// (id in the path), dispatch/trigger endpoints (their own conflict guards), and
// reads-via-POST. Not being a create is not enough on its own: an action a
// client cannot safely retry without a key belongs in requireIdempotency, like
// the session actions listed there. Listing every exemption explicitly (rather
// than relying on a status-code heuristic) makes the guard airtight: every POST
// must be classified, so a new create at ANY status (201, 202, …) that is
// neither wired nor triaged fails the test.
//
// The session actions here, permission-mode, pin, rename and unpin, set a
// state: with no other request in between, a retry sets the same state again
// or is refused.
var exemptFromIdempotency = map[string]bool{
	// ensure-extmsg-group is identity-idempotent by design: the ensure
	// semantics (same group in → same group out) make a retry safe without a
	// key, so wiring one would be dead weight (owner decision, 2026-07-11).
	// Accepted trade-off: the ExtMsgGroupCreated event fires on every ensure,
	// so a retry can double-emit it — tolerable for an ensure endpoint; move
	// this opid to requireIdempotency if that ever matters.
	"ensure-extmsg-group": true,
	// propose-session-op is keyed by the operation journal itself: the op id
	// derives from the session, the principal and the client key (the body's
	// or the Idempotency-Key header's), and a retry returns the stored op
	// (200) or key-reuse (409) across restarts, which the LRU cannot.
	"propose-session-op": true,
	// cancel-session-op is idempotent by state: the first recorded cancel
	// wins and a retry answers already_terminal or requested.
	"cancel-session-op": true,
	"post-v0-city-by-city-name-agent-by-base-by-action":        true,
	"post-v0-city-by-city-name-agent-by-dir-by-base-by-action": true,
	"post-v0-city-by-city-name-bead-by-id-assign":              true,
	"post-v0-city-by-city-name-bead-by-id-close":               true,
	"post-v0-city-by-city-name-bead-by-id-reopen":              true,
	"post-v0-city-by-city-name-bead-by-id-update":              true,
	"post-v0-city-by-city-name-convoy-by-id-add":               true,
	"post-v0-city-by-city-name-convoy-by-id-close":             true,
	"post-v0-city-by-city-name-convoy-by-id-remove":            true,
	"post-v0-city-by-city-name-extmsg-bind":                    true,
	"post-v0-city-by-city-name-extmsg-inbound":                 true,
	"post-v0-city-by-city-name-extmsg-outbound":                true,
	"post-v0-city-by-city-name-extmsg-participants":            true,
	"post-v0-city-by-city-name-extmsg-transcript-ack":          true,
	"post-v0-city-by-city-name-extmsg-unbind":                  true,
	"post-v0-city-by-city-name-formulas-by-name-preview":       true,
	"post-v0-city-by-city-name-formulas-by-name-validate":      true,
	"post-v0-city-by-city-name-mail-by-id-archive":             true,
	"post-v0-city-by-city-name-mail-by-id-mark-unread":         true,
	"post-v0-city-by-city-name-mail-by-id-read":                true,
	"post-v0-city-by-city-name-order-by-name-disable":          true,
	"post-v0-city-by-city-name-order-by-name-enable":           true,
	"post-v0-city-by-city-name-order-by-name-run":              true,
	"post-v0-city-by-city-name-rig-by-name-by-action":          true,
	"post-v0-city-by-city-name-runs-by-run-id-cancel":          true,
	"post-v0-city-by-city-name-service-by-name-restart":        true,
	"post-v0-city-by-city-name-session-by-id-permission-mode":  true,
	"post-v0-city-by-city-name-session-by-id-pin":              true,
	"post-v0-city-by-city-name-session-by-id-rename":           true,
	"post-v0-city-by-city-name-session-by-id-unpin":            true,
	"post-v0-city-by-city-name-sling":                          true,
	"post-v0-city-by-city-name-unregister":                     true,
	"rotate-events":                                            true,
	"trigger-maintenance-dolt-gc":                              true,
}

type idemSpecDoc struct {
	Paths map[string]map[string]idemSpecOp `json:"paths"`
}

type idemSpecOp struct {
	OperationID string          `json:"operationId"`
	Parameters  []idemSpecParam `json:"parameters"`
	Responses   map[string]any  `json:"responses"`
}

type idemSpecParam struct {
	In   string `json:"in"`
	Name string `json:"name"`
}

func (op idemSpecOp) hasIdempotencyHeader() bool {
	for _, p := range op.Parameters {
		if p.In == "header" && p.Name == "Idempotency-Key" {
			return true
		}
	}
	return false
}

func liveIdemSpec(t *testing.T) idemSpecDoc {
	t.Helper()
	sm := api.NewSupervisorMux(emptyTestResolver{}, nil, false, "", "", time.Time{})
	req := httptest.NewRequest(http.MethodGet, "/openapi.json", nil)
	rec := httptest.NewRecorder()
	sm.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /openapi.json returned %d: %s", rec.Code, rec.Body.String())
	}
	var doc idemSpecDoc
	if err := json.Unmarshal(rec.Body.Bytes(), &doc); err != nil {
		t.Fatalf("parse live spec: %v", err)
	}
	return doc
}

// TestCreateEndpointsAreTriagedForIdempotency is the guard that no create
// endpoint silently ships without Idempotency-Key. It requires EVERY POST
// operation to be classified into exactly one of three sets:
//
//   - requireIdempotency  → MUST declare the header (regression guard)
//   - pendingIdempotency  → known create, not yet wired; MUST NOT declare the
//     header yet (wiring it forces a move to requireIdempotency)
//   - exemptFromIdempotency → not a create (action/dispatch/read); no header
//
// A POST operation in none of the sets fails the test: a new endpoint was added
// without triage. If it creates a resource, or a client cannot safely retry it
// without a key (as for the session actions in requireIdempotency), wire it
// through withIdempotency and add it to requireIdempotency; otherwise add it to
// exemptFromIdempotency. This full partition closes the gap where a create at a
// non-201 status (e.g. 202) could slip past a status-code heuristic.
func TestCreateEndpointsAreTriagedForIdempotency(t *testing.T) {
	spec := liveIdemSpec(t)

	seen := map[string]bool{}
	for path, methods := range spec.Paths {
		post, ok := methods["post"]
		if !ok {
			continue
		}
		opid := post.OperationID
		seen[opid] = true

		switch {
		case requireIdempotency[opid]:
			if !post.hasIdempotencyHeader() {
				t.Errorf("create %q (POST %s) must declare an Idempotency-Key header but does not — "+
					"wire it through withIdempotency and add the header input field", opid, path)
			}
		case pendingIdempotency[opid]:
			if post.hasIdempotencyHeader() {
				t.Errorf("%q (POST %s) now declares Idempotency-Key — move it from pendingIdempotency "+
					"to requireIdempotency in idempotency_guard_test.go", opid, path)
			}
		case exemptFromIdempotency[opid]:
			// Not a create; no assertion.
		default:
			t.Errorf("POST %s (op %q) is not triaged for idempotency. If it creates a resource, "+
				"wire it through withIdempotency and add %q to requireIdempotency; otherwise add it "+
				"to exemptFromIdempotency in idempotency_guard_test.go.", path, opid, opid)
		}
	}

	// Catch typos / renamed operations: every listed opid must exist in the spec.
	for _, set := range []map[string]bool{requireIdempotency, pendingIdempotency, exemptFromIdempotency} {
		for opid := range set {
			if !seen[opid] {
				t.Errorf("idempotency guard lists %q but no such POST operation exists in the spec", opid)
			}
		}
	}
}
