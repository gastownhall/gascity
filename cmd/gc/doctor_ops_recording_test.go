package main

import (
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/clock"
	"github.com/gastownhall/gascity/internal/doctor"
	"github.com/gastownhall/gascity/internal/opjournal"
	"github.com/gastownhall/gascity/internal/opjournal/legacyop"
	"github.com/gastownhall/gascity/internal/rollout"
)

// opsRecordingClosingStore counts CloseStore calls, so the check is seen closing
// the store it opened; err is what each close returns.
type opsRecordingClosingStore struct {
	beads.Store
	closes *atomic.Int32
	err    error
}

func (c opsRecordingClosingStore) CloseStore() error {
	c.closes.Add(1)
	return c.err
}

func newTestOpsRecordingCheck(store beads.Store, now time.Time, opts ...rollout.ForTestOption) (*opsRecordingDoctorCheck, *atomic.Int32) {
	var opens atomic.Int32
	c := newOpsRecordingDoctorCheck("/no/city", nil, rollout.ForTest(opts...), nil)
	c.open = func() (beads.Store, error) {
		opens.Add(1)
		return store, nil
	}
	c.sessions = func(s beads.Store) beads.Store {
		if cc, ok := s.(opsRecordingClosingStore); ok {
			return cc.Store
		}
		return s
	}
	c.clk = &clock.Fake{Time: now}
	return c, &opens
}

func TestOpsRecordingDoctorCheck(t *testing.T) {
	t.Run("off", func(t *testing.T) {
		c, opens := newTestOpsRecordingCheck(newOpSweepStore(), opSweepT0)
		r := c.Run(&doctor.CheckContext{})
		if r.Status != doctor.StatusOK || r.Message != "ops recording is off" {
			t.Fatalf("result = %v %q, want OK, ops recording is off", r.Status, r.Message)
		}
		if opens.Load() != 0 {
			t.Fatal("the check opened the store with every gate off")
		}
	})

	t.Run("auto with an incapable store warns", func(t *testing.T) {
		c, _ := newTestOpsRecordingCheck(incapableStore{beads.NewMemStore()}, opSweepT0, rollout.WithOpsContent(rollout.Auto))
		r := c.Run(&doctor.CheckContext{})
		if r.Status != doctor.StatusWarning {
			t.Fatalf("status = %v, want Warning: %s", r.Status, r.Message)
		}
		for _, want := range []string{"ops.content=auto", "conditional_writes_unsupported", "unrecorded"} {
			if !strings.Contains(r.Message, want) {
				t.Errorf("message %q lacks %q", r.Message, want)
			}
		}
		if !strings.Contains(r.FixHint, "conditional writes") || strings.Contains(r.FixHint, "id prefix") {
			t.Errorf("remedy %q, want the conditional-writes remedy alone", r.FixHint)
		}
	})

	t.Run("require with an incapable store errors", func(t *testing.T) {
		c, _ := newTestOpsRecordingCheck(&beads.MemStore{HonorExplicitIDs: true}, opSweepT0,
			rollout.WithOpsContent(rollout.Auto), rollout.WithOpsLifecycle(rollout.Require))
		r := c.Run(&doctor.CheckContext{})
		if r.Status != doctor.StatusError {
			t.Fatalf("status = %v, want Error: %s", r.Status, r.Message)
		}
		for _, want := range []string{"ops.content=auto", "ops.lifecycle=require", "no_namespace", "refuses"} {
			if !strings.Contains(r.Message, want) {
				t.Errorf("message %q lacks %q", r.Message, want)
			}
		}
		if !strings.Contains(r.FixHint, "id prefix") || strings.Contains(r.FixHint, "conditional writes") {
			t.Errorf("remedy %q, want the id-prefix remedy alone", r.FixHint)
		}
	})

	t.Run("an unopenable store", func(t *testing.T) {
		c, _ := newTestOpsRecordingCheck(nil, opSweepT0, rollout.WithOpsControl(rollout.Require))
		c.open = func() (beads.Store, error) { return nil, errors.New("dolt down") }
		r := c.Run(&doctor.CheckContext{})
		if r.Status != doctor.StatusError || !strings.Contains(r.Message, legacyop.DegradeStoreError) {
			t.Fatalf("result = %v %q, want Error naming store_error", r.Status, r.Message)
		}
		if !strings.Contains(r.FixHint, "reachable") || strings.Contains(r.FixHint, "conditional writes") {
			t.Errorf("remedy %q, want the reachability remedy", r.FixHint)
		}
	})

	t.Run("a failed listing warns", func(t *testing.T) {
		store := &opSweepFlakyStore{opSweepStore: newOpSweepStore()}
		store.fails.Store(1)
		c, _ := newTestOpsRecordingCheck(store, opSweepT0, rollout.WithOpsControl(rollout.Require))
		r := c.Run(&doctor.CheckContext{})
		if r.Status != doctor.StatusWarning || !strings.Contains(r.Message, "listing open legacy ops failed") || !strings.Contains(r.Message, "dolt down") {
			t.Fatalf("result = %v %q, want a Warning naming the failed listing", r.Status, r.Message)
		}
	})

	t.Run("unresolved gates are not checked", func(t *testing.T) {
		var opens atomic.Int32
		c := newOpsRecordingDoctorCheck("/no/city", nil, rollout.ForTest(rollout.WithOpsContent(rollout.Require)), errors.New("bad [ops]"))
		c.open = func() (beads.Store, error) {
			opens.Add(1)
			return newOpSweepStore(), nil
		}
		r := c.Run(&doctor.CheckContext{})
		if r.Status != doctor.StatusOK || !strings.Contains(r.Message, "not checked") {
			t.Fatalf("result = %v %q, want OK not checked", r.Status, r.Message)
		}
		if opens.Load() != 0 {
			t.Fatal("the check opened the store with unresolved gates")
		}
	})

	t.Run("capable is OK and closes the store", func(t *testing.T) {
		var closes atomic.Int32
		store := newOpSweepStore()
		fresh := seedOp(t, store, &clock.Fake{Time: opSweepT0}, opjournal.ExecutorLegacyAPI, "fresh")
		// Expired, but within two sweep intervals: the sweeper is not late
		// yet. A failed close of the probed store does not change the verdict.
		c, _ := newTestOpsRecordingCheck(opsRecordingClosingStore{store, &closes, errors.New("close failed")}, fresh.FinishBy.Add(opSweepInterval),
			rollout.WithOpsControl(rollout.Auto))
		r := c.Run(&doctor.CheckContext{})
		if r.Status != doctor.StatusOK {
			t.Fatalf("status = %v, want OK: %s", r.Status, r.Message)
		}
		if closes.Load() != 1 {
			t.Fatalf("store closed %d time(s), want 1", closes.Load())
		}
	})

	t.Run("a stale expired op warns", func(t *testing.T) {
		store := newOpSweepStore()
		stale := seedOp(t, store, &clock.Fake{Time: opSweepT0}, opjournal.ExecutorLegacyCLI, "stale")
		seedOp(t, store, &clock.Fake{Time: opSweepT0}, opjournal.ExecutorLane, "lane")
		c, _ := newTestOpsRecordingCheck(store, stale.FinishBy.Add(2*opSweepInterval+time.Second),
			rollout.WithOpsLifecycle(rollout.Auto))
		r := c.Run(&doctor.CheckContext{})
		if r.Status != doctor.StatusWarning {
			t.Fatalf("status = %v, want Warning: %s", r.Status, r.Message)
		}
		if !strings.Contains(r.Message, "1 open legacy op(s)") || !strings.Contains(r.Message, "sweeper") {
			t.Fatalf("message %q does not name the one stale op and the sweeper", r.Message)
		}
		if len(r.Details) != 1 || !strings.Contains(r.Details[0], stale.ID) {
			t.Fatalf("details = %v, want the stale op %s", r.Details, stale.ID)
		}
		if !strings.Contains(r.FixHint, "restart the controller") {
			t.Fatalf("remedy %q does not name the boot-latched gates", r.FixHint)
		}
	})

	t.Run("flags", func(t *testing.T) {
		c, _ := newTestOpsRecordingCheck(nil, opSweepT0)
		if c.Name() != "ops-recording" || c.CanFix() || c.WarmupEligible() {
			t.Fatalf("name/canfix/warmup = %q/%v/%v", c.Name(), c.CanFix(), c.WarmupEligible())
		}
	})
}
