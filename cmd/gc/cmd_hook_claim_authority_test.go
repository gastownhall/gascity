package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/events"
)

// AC-8: when an armed store refuses a claim gc makes, the refusal reaches gc
// as the typed beads.ErrClaimNotAuthorized, not as "already claimed": gc hook
// records a bead.claim_refused event naming the bead, the assignee and the
// store, writes nothing else to the bead, and drains claims_errored so the
// claim is retried later, on both claim tiers.
func TestDoHookClaimReportsClaimAuthorityRefusalAsNotAuthorized(t *testing.T) {
	const refusal = "exit status 1: Error updating work-1: Error 1644 (45000): gc-claim-authority: worker-1 is not an authorized claimant for routed bead work-1 in store gc_rig\n" +
		`{"error":"1 of 1 issues failed to update","failed":[{"id":"work-1","error":"Error 1644 (45000): gc-claim-authority: worker-1 is not an authorized claimant for routed bead work-1 in store gc_rig"}],"schema_version":1}`
	cases := []struct {
		name     string
		assignee string
	}{
		{name: "routed candidate", assignee: ""},
		{name: "ready assignment", assignee: "worker-1"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			originalRunner := hookClaimCommandRunnerWithEnvContext
			t.Cleanup(func() { hookClaimCommandRunnerWithEnvContext = originalRunner })
			var ran []string
			hookClaimCommandRunnerWithEnvContext = func(context.Context, map[string]string) beads.CommandRunner {
				return func(_ string, name string, args ...string) ([]byte, error) {
					ran = append(ran, name+" "+strings.Join(args, " "))
					if name == "bd" && reflect.DeepEqual(args, []string{"update", "work-1", "--claim", "--json"}) {
						return nil, errors.New(refusal)
					}
					t.Fatalf("unexpected command after the refused claim: %s %q", name, args)
					return nil, nil
				}
			}
			candidate, err := json.Marshal([]beads.Bead{{
				ID: "work-1", Status: "open", Assignee: tc.assignee,
				Metadata: map[string]string{"gc.routed_to": "worker"},
			}})
			if err != nil {
				t.Fatalf("marshal candidate: %v", err)
			}
			var refused []hookClaimRefusal
			var rejected, failed []string
			ops := hookClaimOps{
				Runner:            func(string, string) (string, error) { return string(candidate), nil },
				DrainAck:          func(io.Writer) error { return nil },
				EmitClaimFailed:   func(beadID string, _ hookClaimOptions, _ error) { failed = append(failed, beadID) },
				EmitClaimRejected: func(beadID, _, _ string) { rejected = append(rejected, beadID) },
				EmitClaimRefused:  func(r hookClaimRefusal) { refused = append(refused, r) },
			}

			var stdout, stderr bytes.Buffer
			code := doHookClaim("query", "/rig", hookClaimOptions{
				Assignee:           "worker-1",
				IdentityCandidates: []string{"worker-1"},
				RouteTargets:       []string{"worker"},
				DrainAck:           true,
				JSON:               true,
			}, ops, &stdout, &stderr)
			if code != 0 {
				t.Fatalf("doHookClaim() = %d, want 0; stderr=%s", code, stderr.String())
			}
			var result hookClaimJSONResult
			if err := json.Unmarshal(stdout.Bytes(), &result); err != nil {
				t.Fatalf("stdout is not JSON: %v\nraw: %s\nstderr: %s", err, stdout.String(), stderr.String())
			}
			if result.Action != "drain" || result.Reason != hookClaimReasonClaimsErrored {
				t.Fatalf("claim result = %+v, want drain/%s; stderr=%s", result, hookClaimReasonClaimsErrored, stderr.String())
			}
			if len(refused) != 1 {
				t.Fatalf("claim_refused emitted %d times, want once; stderr=%s", len(refused), stderr.String())
			}
			got := refused[0]
			if !errors.Is(got.Err, beads.ErrClaimNotAuthorized) {
				t.Fatalf("refusal error %v is not beads.ErrClaimNotAuthorized", got.Err)
			}
			if got.BeadID != "work-1" || got.Assignee != "worker-1" || got.Store != "gc_rig" {
				t.Fatalf("refusal = %+v, want bead work-1, assignee worker-1, store gc_rig", got)
			}
			if len(rejected) != 0 || len(failed) != 0 {
				t.Fatalf("refusal also reported as rejected %q / failed %q", rejected, failed)
			}
			if strings.Contains(stderr.String(), "already claimed") || !strings.Contains(stderr.String(), "not authorized") {
				t.Fatalf("stderr = %q, want a not-authorized refusal, not already claimed", stderr.String())
			}
			if want := []string{"bd update work-1 --claim --json"}; !reflect.DeepEqual(ran, want) {
				t.Fatalf("bd ran %q, want only the refused claim %q: the bead must be left untouched", ran, want)
			}
		})
	}
}

// AC-8: the claim-refused event is a registered event type whose payload
// names the bead, the assignee and the store.
func TestEmitClaimRefusedRecordsRegisteredEvent(t *testing.T) {
	if !slices.Contains(events.KnownEventTypes, events.BeadClaimRefused) {
		t.Fatalf("%s is missing from KnownEventTypes", events.BeadClaimRefused)
	}
	if sample, ok := events.LookupPayload(events.BeadClaimRefused); !ok {
		t.Fatalf("%s has no registered payload", events.BeadClaimRefused)
	} else if _, ok := sample.(events.BeadClaimRefusedPayload); !ok {
		t.Fatalf("%s registered payload is %T", events.BeadClaimRefused, sample)
	}
	rec := events.NewFake()
	emitClaimRefused(rec, hookClaimRefusal{
		BeadID: "work-1", Assignee: "worker-1", Store: "gc_rig",
		Err: &beads.ClaimNotAuthorizedError{Message: "gc-claim-authority: worker-1 is not an authorized claimant for routed bead work-1 in store gc_rig"},
	})
	if len(rec.Events) != 1 {
		t.Fatalf("recorded %d events, want 1", len(rec.Events))
	}
	evt := rec.Events[0]
	if evt.Type != events.BeadClaimRefused || evt.Subject != "work-1" || evt.Actor != "worker-1" {
		t.Fatalf("event = %+v, want %s on work-1 by worker-1", evt, events.BeadClaimRefused)
	}
	var payload events.BeadClaimRefusedPayload
	if err := json.Unmarshal(evt.Payload, &payload); err != nil {
		t.Fatalf("decode payload: %v", err)
	}
	if payload.BeadID != "work-1" || payload.Assignee != "worker-1" || payload.Store != "gc_rig" || !strings.Contains(payload.Reason, "not an authorized claimant") {
		t.Fatalf("payload = %+v", payload)
	}
}
