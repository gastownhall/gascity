package main

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/orderdispatch"
	"github.com/gastownhall/gascity/internal/orders"
	"github.com/gastownhall/gascity/internal/webhookmatch"
)

// TestDispatchSeamFiresExecViaDispatchOne proves the exported orderdispatch
// seam fires a webhook-triggered exec order through the SAME dispatchOne core the
// tick loop uses, and — the R4 assertion at the dispatch layer — that the
// namespaced exec-env overlay a webhook supplies (GC_WEBHOOK_ARG_*) reaches the
// process WITHOUT letting a payload value shadow a static [order.env] entry.
func TestDispatchSeamFiresExecViaDispatchOne(t *testing.T) {
	cityDir := t.TempDir()
	store := beads.NewMemStore()
	var rec memRecorder

	envCh := make(chan []string, 1)
	execRun := func(_ context.Context, _ /*cmd*/, _ /*dir*/ string, env []string) ([]byte, error) {
		envCh <- env
		return nil, nil
	}

	// A filler auto-order keeps the dispatcher non-nil; the webhook order is fired
	// through the seam directly, so it need not appear in the dispatcher's set.
	filler := orders.Order{Name: "filler", Trigger: "cooldown", Interval: "1h", Exec: "true"}
	ad := buildOrderDispatcherFromListExec([]orders.Order{filler}, store, nil, execRun, &rec)
	if ad == nil {
		t.Fatal("expected non-nil dispatcher")
	}
	mad := ad.(*memoryOrderDispatcher)
	mad.cityPath = cityDir

	// The webhook order declares a static [order.env] FOO=static and a required
	// param repo. The delivery's extracted args (as E6 would hand them) carry a
	// payload FOO that must NOT shadow the static one once namespaced.
	order := orders.Order{
		Name:    "pr-review-exec",
		Trigger: "webhook",
		Exec:    "true",
		Env:     map[string]string{"FOO": "static"},
		Params:  map[string]orders.OrderParam{"repo": {Required: true}},
	}
	rawVars := map[string]string{"repo": "octo/demo", "FOO": "payload"}

	res, err := mad.Dispatch(context.Background(), orderdispatch.DispatchRequest{
		Order:   order,
		Vars:    rawVars,
		ExecEnv: webhookmatch.ExecEnvVars(rawVars),
		Source:  orderdispatch.SourceWebhook,
	})
	if err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	if !res.Fired || res.TrackingID == "" {
		t.Fatalf("expected fired dispatch with tracking id, got %+v", res)
	}

	drainCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if !mad.drain(drainCtx) {
		t.Fatal("dispatch did not drain in time")
	}

	var env []string
	select {
	case env = <-envCh:
	default:
		t.Fatal("exec never ran (no env captured)")
	}
	got := map[string]string{}
	for _, entry := range env {
		if k, v, ok := strings.Cut(entry, "="); ok {
			got[k] = v
		}
	}

	// R4: the static [order.env] key is intact — the payload FOO could not shadow
	// it because the webhook arg was namespaced.
	if got["FOO"] != "static" {
		t.Fatalf("env[FOO] = %q, want static (payload must not shadow [order.env])", got["FOO"])
	}
	// The namespaced webhook args reach the process under GC_WEBHOOK_ARG_.
	if got["GC_WEBHOOK_ARG_FOO"] != "payload" {
		t.Fatalf("env[GC_WEBHOOK_ARG_FOO] = %q, want payload", got["GC_WEBHOOK_ARG_FOO"])
	}
	if got["GC_WEBHOOK_ARG_repo"] != "octo/demo" {
		t.Fatalf("env[GC_WEBHOOK_ARG_repo] = %q, want octo/demo", got["GC_WEBHOOK_ARG_repo"])
	}
	// No bare payload key leaked into the environment.
	if _, bare := got["repo"]; bare {
		t.Fatalf("env leaked a bare param key repo: %v", got)
	}

	// The seam went through dispatchOne: OrderFired then OrderCompleted for the
	// scoped order.
	if !rec.hasType(events.OrderFired) {
		t.Fatal("missing OrderFired event; dispatch did not go through dispatchOne")
	}
	if !rec.hasSubject("pr-review-exec") {
		t.Fatal("no event recorded for the fired order subject")
	}
}

// TestDispatchSeamRefusesMissingRequiredParam proves the seam itself fail-closes
// on a missing required param (defense in depth beneath the E6 sink guard):
// nothing is written and no order fires.
func TestDispatchSeamRefusesMissingRequiredParam(t *testing.T) {
	store := beads.NewMemStore()
	var rec memRecorder
	ranExec := false
	execRun := func(context.Context, string, string, []string) ([]byte, error) {
		ranExec = true
		return nil, nil
	}
	filler := orders.Order{Name: "filler", Trigger: "cooldown", Interval: "1h", Exec: "true"}
	mad := buildOrderDispatcherFromListExec([]orders.Order{filler}, store, nil, execRun, &rec).(*memoryOrderDispatcher)
	mad.cityPath = t.TempDir()

	order := orders.Order{
		Name:    "needs-repo",
		Trigger: "webhook",
		Exec:    "true",
		Params:  map[string]orders.OrderParam{"repo": {Required: true}},
	}
	res, err := mad.Dispatch(context.Background(), orderdispatch.DispatchRequest{
		Order:  order,
		Vars:   nil, // required repo absent
		Source: orderdispatch.SourceWebhook,
	})
	if err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	if !res.Rejected || res.Fired {
		t.Fatalf("expected rejection, got %+v", res)
	}
	if res.TrackingID != "" {
		t.Fatalf("tracking bead written for a refused dispatch: %q", res.TrackingID)
	}
	if ranExec {
		t.Fatal("exec ran despite missing required param")
	}
	if !strings.Contains(res.Reason, "repo") {
		t.Fatalf("reason = %q, want it to name the missing required param", res.Reason)
	}
}
