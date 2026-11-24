package k8s

import (
	"context"
	"testing"
	"testing/synctest"
	"time"

	corev1 "k8s.io/api/core/v1"
)

// startDeadlineOps records whether each createPod ctx carried a deadline.
type startDeadlineOps struct {
	*holdOps
	createDeadline []bool
}

func (s *startDeadlineOps) createPod(ctx context.Context, pod *corev1.Pod) (*corev1.Pod, error) {
	_, ok := ctx.Deadline()
	s.createDeadline = append(s.createDeadline, ok)
	return s.holdOps.createPod(ctx, pod)
}

// TestStartIsNotCutAtTheRequestBound: Start runs under its caller's ctx (the
// runtime start lease bounds it), never under the per-verb request bound. A
// pod create that takes twice the request bound still succeeds, nothing is
// cut, and under a caller ctx with no deadline the create ctx carries none.
func TestStartIsNotCutAtTheRequestBound(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		slow := 2 * testRequestBound
		ops := &startDeadlineOps{holdOps: newHoldOps(func(method string, _ int, _ []string) time.Duration {
			if method == "createPod" {
				return slow
			}
			return 0
		})}
		p := deadlineProvider(ops)

		start := time.Now()
		if err := p.Start(context.Background(), "gc-test-agent", staleProbeConfig()); err != nil {
			t.Fatalf("Start with a %v pod create (request bound %v) = %v; want success", slow, testRequestBound, err)
		}
		if elapsed := time.Since(start); elapsed < slow {
			t.Fatalf("Start returned after %v; the create was never held", elapsed)
		}
		if ops.cutCount() != 0 {
			t.Fatalf("cut calls = %d; want none", ops.cutCount())
		}
		if len(ops.createDeadline) == 0 {
			t.Fatal("Start made no createPod call: the test is vacuous")
		}
		for i, has := range ops.createDeadline {
			if has {
				t.Fatalf("createPod #%d ran under a deadline; want Start's caller ctx (none)", i)
			}
		}
	})
}
