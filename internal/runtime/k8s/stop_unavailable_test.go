package k8s

import (
	"context"
	"errors"
	"net/url"
	"syscall"
	"testing"
	"testing/synctest"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/gastownhall/gascity/internal/runtime"
)

// TestStopThatCannotReachTheAPIServerIsRuntimeUnavailable: a Stop whose list
// or delete never got an answer from the API server (its deadline ended, or
// the connection failed) could not tell whether the pod is gone, so the error
// wraps runtime.ErrRuntimeUnavailable along with its cause. An answer the API
// server did give keeps its own shape: a refusal is not "unavailable", and a
// NotFound is a stopped session.
func TestStopThatCannotReachTheAPIServerIsRuntimeUnavailable(t *testing.T) {
	podGR := schema.GroupResource{Resource: "pods"}
	refused := &url.Error{Op: "Delete", URL: "https://apiserver/api/v1/pods/x", Err: syscall.ECONNREFUSED}
	for _, tc := range []struct {
		name            string
		hold            func(string, int, []string) time.Duration
		listErr         error
		deleteErr       error
		wantNil         bool
		wantUnavailable bool
		wantCause       error
	}{
		{name: "list ends at its deadline", hold: holdMethods("listPods"), wantUnavailable: true, wantCause: context.DeadlineExceeded},
		{name: "delete ends at its deadline", hold: holdMethods("deletePod"), wantUnavailable: true, wantCause: context.DeadlineExceeded},
		{name: "delete connection refused", deleteErr: refused, wantUnavailable: true, wantCause: syscall.ECONNREFUSED},
		{name: "delete forbidden", deleteErr: apierrors.NewForbidden(podGR, "x", errors.New("rbac")), wantUnavailable: false},
		{name: "list not found", listErr: apierrors.NewNotFound(podGR, "x"), wantNil: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				hold := tc.hold
				if hold == nil {
					hold = holdMethods()
				}
				ops := newHoldOps(hold)
				ops.listErr, ops.deleteErr = tc.listErr, tc.deleteErr
				addRunningPods(ops.fakeK8sOps, 1)
				p := deadlineProvider(ops)

				err := p.Stop(deadlineSession)
				if tc.wantNil {
					if err != nil {
						t.Fatalf("Stop = %v; want nil", err)
					}
					return
				}
				if err == nil {
					t.Fatal("Stop = nil; want an error")
				}
				if got := errors.Is(err, runtime.ErrRuntimeUnavailable); got != tc.wantUnavailable {
					t.Fatalf("Stop = %v; errors.Is(ErrRuntimeUnavailable) = %v, want %v", err, got, tc.wantUnavailable)
				}
				if tc.wantCause != nil && !errors.Is(err, tc.wantCause) {
					t.Fatalf("Stop = %v; want it to wrap %v", err, tc.wantCause)
				}
			})
		})
	}
}
