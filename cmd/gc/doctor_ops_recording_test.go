package main

import (
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/beads/beadstest"
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

// newTestOpsRecordingCheck is the check over store with every ops gate off
// but those opts turn on.
func newTestOpsRecordingCheck(store beads.Store, now time.Time, opts ...rollout.ForTestOption) (*opsRecordingDoctorCheck, *atomic.Int32) {
	var opens atomic.Int32
	c := newOpsRecordingDoctorCheck("/no/city", nil, rollout.ForTest(opsGatesOff(opts...)...), nil)
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
		var closes atomic.Int32
		store := newOpSweepStore()
		c, opens := newTestOpsRecordingCheck(opsRecordingClosingStore{store, &closes, nil}, opSweepT0)
		r := c.Run(&doctor.CheckContext{})
		if r.Status != doctor.StatusOK || r.Message != "ops recording is off" {
			t.Fatalf("result = %v %q, want OK, ops recording is off", r.Status, r.Message)
		}
		if opens.Load() != 1 || closes.Load() != 1 {
			t.Fatalf("store opened %d, closed %d time(s), want 1 and 1", opens.Load(), closes.Load())
		}
		// With no op row the check reads one row at most and probes nothing.
		if ops := store.Ops(); len(ops) != 1 || ops[0].Kind != beadstest.OpList || ops[0].Query.Limit != 1 {
			t.Fatalf("store ops = %+v, want one List of one row", ops)
		}
	})

	t.Run("off with only unexpired ops is OK", func(t *testing.T) {
		store := newOpSweepStore()
		fresh := seedOp(t, store, &clock.Fake{Time: opSweepT0}, opjournal.ExecutorLegacyAPI, "fresh")
		c, _ := newTestOpsRecordingCheck(store, fresh.FinishBy.Add(opSweepInterval))
		if r := c.Run(&doctor.CheckContext{}); r.Status != doctor.StatusOK || r.Message != "ops recording is off" {
			t.Fatalf("result = %v %q, want OK, ops recording is off", r.Status, r.Message)
		}
	})

	// The sweeper stops with every gate off once a pass leaves no work, so a
	// legacy op that expires after that stays open: the check still scans.
	t.Run("off with a stale expired op warns", func(t *testing.T) {
		store := newOpSweepStore()
		stale := seedOp(t, store, &clock.Fake{Time: opSweepT0}, opjournal.ExecutorLegacyCLI, "stale")
		c, _ := newTestOpsRecordingCheck(store, stale.FinishBy.Add(opsRecordingStaleAfter+time.Second))
		r := c.Run(&doctor.CheckContext{})
		if r.Status != doctor.StatusWarning || !strings.HasPrefix(r.Message, "ops recording is off; 1 open legacy op(s)") {
			t.Fatalf("result = %v %q, want a gates-off Warning counting the stale op", r.Status, r.Message)
		}
		if len(r.Details) != 1 || !strings.Contains(r.Details[0], stale.ID) {
			t.Fatalf("details = %v, want the stale op %s", r.Details, stale.ID)
		}
		if !strings.HasPrefix(r.FixHint, "restart the controller") || !strings.Contains(r.FixHint, "every ops gate off") {
			t.Fatalf("remedy %q, want the gates-off restart remedy", r.FixHint)
		}
	})

	t.Run("off with an unopenable store warns", func(t *testing.T) {
		c, _ := newTestOpsRecordingCheck(nil, opSweepT0)
		c.open = func() (beads.Store, error) { return nil, errors.New("dolt down") }
		r := c.Run(&doctor.CheckContext{})
		if r.Status != doctor.StatusWarning || !strings.HasPrefix(r.Message, "ops recording is off, but") || !strings.Contains(r.Message, legacyop.DegradeStoreError) {
			t.Fatalf("result = %v %q, want a gates-off Warning naming store_error", r.Status, r.Message)
		}
		if len(r.Details) != 1 || !strings.Contains(r.Details[0], "dolt down") || !strings.Contains(r.FixHint, "reachable") {
			t.Fatalf("details = %q, remedy %q, want the open error and the reachability remedy", r.Details, r.FixHint)
		}
	})

	t.Run("off with a failed probe warns", func(t *testing.T) {
		store := &opSweepFlakyStore{opSweepStore: newOpSweepStore()}
		store.fails.Store(1)
		c, _ := newTestOpsRecordingCheck(store, opSweepT0)
		r := c.Run(&doctor.CheckContext{})
		if r.Status != doctor.StatusWarning || !strings.Contains(r.Message, "could not be checked") ||
			len(r.Details) != 1 || !strings.Contains(r.Details[0], "dolt down") {
			t.Fatalf("result = %v %q %q, want a gates-off Warning naming the failed probe", r.Status, r.Message, r.Details)
		}
	})

	// Setting the gates to off is no remedy when they are off already.
	t.Run("off with an incapable store holding op rows warns", func(t *testing.T) {
		store := newOpSweepStore()
		seedOp(t, store, &clock.Fake{Time: opSweepT0}, opjournal.ExecutorLegacyCLI, "orphan")
		c, _ := newTestOpsRecordingCheck(incapableStore{store}, opSweepT0)
		r := c.Run(&doctor.CheckContext{})
		if r.Status != doctor.StatusWarning || !strings.HasPrefix(r.Message, "ops recording is off, but") {
			t.Fatalf("result = %v %q, want a gates-off Warning", r.Status, r.Message)
		}
		if r.FixHint == "" || strings.Contains(r.FixHint, "set the ops gates to off") {
			t.Fatalf("remedy %q, want one that does not offer turning the gates off", r.FixHint)
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
		if !strings.Contains(r.FixHint, "conditional writes") || strings.Contains(r.FixHint, "id prefix") || !strings.Contains(r.FixHint, "set the ops gates to off") {
			t.Errorf("remedy %q, want the conditional-writes remedy alone, or the gates off", r.FixHint)
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
		if r.Status != doctor.StatusWarning || !strings.Contains(r.Message, "listing expired legacy ops failed") || !strings.Contains(r.Message, "dolt down") {
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
		if !strings.Contains(r.Message, "; 1 open legacy op(s)") || !strings.Contains(r.Message, "sweeper") {
			t.Fatalf("message %q does not name the one stale op and the sweeper", r.Message)
		}
		if len(r.Details) != 1 || !strings.Contains(r.Details[0], stale.ID) {
			t.Fatalf("details = %v, want the stale op %s", r.Details, stale.ID)
		}
		if !strings.Contains(r.FixHint, "restart the controller") {
			t.Fatalf("remedy %q does not name the boot-latched gates", r.FixHint)
		}
	})

	t.Run("stale ops past the read limit are a floor", func(t *testing.T) {
		store := newOpSweepStore()
		var last opjournal.Op
		for i := range opSweepLimit + 1 {
			last = seedOp(t, store, &clock.Fake{Time: opSweepT0}, opjournal.ExecutorLegacyCLI, fmt.Sprintf("stale%03d", i))
		}
		c, _ := newTestOpsRecordingCheck(store, last.FinishBy.Add(2*opSweepInterval+time.Second),
			rollout.WithOpsLifecycle(rollout.Auto))
		r := c.Run(&doctor.CheckContext{})
		want := fmt.Sprintf("; at least %d open legacy op(s)", opSweepLimit)
		if r.Status != doctor.StatusWarning || !strings.Contains(r.Message, want) {
			t.Fatalf("result = %v %q, want a Warning counting %q", r.Status, r.Message, want)
		}
	})

	t.Run("a page of ops not yet stale does not hide a stale op behind it", func(t *testing.T) {
		store := newOpSweepStore()
		// Older ops with a later finish_by fill the read's first page.
		for i := range opSweepLimit {
			seedOp(t, store, &clock.Fake{Time: opSweepT0.Add(time.Hour)}, opjournal.ExecutorLegacyCLI, fmt.Sprintf("inflight%03d", i))
		}
		c, _ := newTestOpsRecordingCheck(store, opSweepT0.Add(legacyop.ExecutionBound+opsRecordingStaleAfter+time.Second),
			rollout.WithOpsLifecycle(rollout.Auto))
		if r := c.Run(&doctor.CheckContext{}); r.Status != doctor.StatusOK {
			t.Fatalf("status = %v, want OK: no op is stale and the read reached the end: %s", r.Status, r.Message)
		}
		stale := seedOp(t, store, &clock.Fake{Time: opSweepT0}, opjournal.ExecutorLegacyCLI, "stale")
		r := c.Run(&doctor.CheckContext{})
		if r.Status != doctor.StatusWarning || !strings.Contains(r.Message, "; 1 open legacy op(s)") {
			t.Fatalf("result = %v %q, want a Warning counting the stale op behind the page", r.Status, r.Message)
		}
		if len(r.Details) != 1 || !strings.Contains(r.Details[0], stale.ID) {
			t.Fatalf("details = %v, want the stale op %s", r.Details, stale.ID)
		}
	})

	t.Run("a truncated scan warns", func(t *testing.T) {
		store := newOpSweepStore()
		seedUndecodableOps(t, store, 8*opSweepLimit) // a head longer than the scan's page cap
		seedOp(t, store, &clock.Fake{Time: opSweepT0}, opjournal.ExecutorLegacyCLI, "behind")
		c, _ := newTestOpsRecordingCheck(store, opSweepT0, rollout.WithOpsContent(rollout.Auto))
		r := c.Run(&doctor.CheckContext{})
		if r.Status != doctor.StatusWarning || !strings.Contains(r.Message, "page cap") || !strings.Contains(r.Message, "not checked") {
			t.Fatalf("result = %v %q, want a Warning that the scan stopped at its page cap", r.Status, r.Message)
		}
		want := fmt.Sprintf("%d op row(s) this gc cannot decode", 8*opSweepLimit)
		if len(r.Details) != 1 || !strings.Contains(r.Details[0], want) || !strings.Contains(r.Details[0], "gc-op-newer00000") {
			t.Fatalf("details = %q, want one line counting %q and naming the first row", r.Details, want)
		}
	})

	t.Run("a truncated scan's stale count is a floor", func(t *testing.T) {
		store := newOpSweepStore()
		stale := seedOp(t, store, &clock.Fake{Time: opSweepT0}, opjournal.ExecutorLegacyCLI, "stale")
		seedUndecodableOps(t, store, 8*opSweepLimit) // with the stale op, more rows than the page cap reads
		c, _ := newTestOpsRecordingCheck(store, stale.FinishBy.Add(2*opSweepInterval+time.Second),
			rollout.WithOpsLifecycle(rollout.Auto))
		r := c.Run(&doctor.CheckContext{})
		if r.Status != doctor.StatusWarning || !strings.Contains(r.Message, "; at least 1 open legacy op(s)") {
			t.Fatalf("result = %v %q, want a Warning counting at least 1", r.Status, r.Message)
		}
		if len(r.Details) != 2 || !strings.Contains(r.Details[0], stale.ID) || !strings.Contains(r.Details[1], "cannot decode") {
			t.Fatalf("details = %q, want the stale op, then the undecodable rows", r.Details)
		}
	})

	t.Run("flags", func(t *testing.T) {
		c, _ := newTestOpsRecordingCheck(nil, opSweepT0)
		if c.Name() != "ops-recording" || c.CanFix() || c.WarmupEligible() {
			t.Fatalf("name/canfix/warmup = %q/%v/%v", c.Name(), c.CanFix(), c.WarmupEligible())
		}
	})
}

// TestOpsRecordingDoctorCheckDefaultAuto covers the builtin default (every
// ops gate auto, none set), which records only over a SQLite sessions store:
// any other store is OK while it holds no op row, Dolt-served or not, capable
// or not, and the check says whether an explicit gate would record there
// (only a store that can record ops is offered the opt-in); one holding op
// rows is still checked for expired ones when it can open, and a Warning when
// it cannot. An explicit auto stays loud over a store that cannot record, and
// a SQLite store reads as recording.
func TestOpsRecordingDoctorCheckDefaultAuto(t *testing.T) {
	defaulted := func(store beads.Store, now time.Time) *opsRecordingDoctorCheck {
		c, _ := newTestOpsRecordingCheck(store, now)
		c.flags = rollout.ForTest()
		return c
	}
	bdWithHelp := func(help string) *beads.BdStore {
		return beads.NewBdStoreWithPrefix(t.TempDir(), func(_, _ string, args ...string) ([]byte, error) {
			if len(args) == 2 && args[1] == "--help" {
				return []byte(help), nil
			}
			return nil, fmt.Errorf("unexpected bd %v", args)
		}, "gc")
	}
	// A bd whose conditional-write verbs all advertise --if-revision: a
	// Dolt-served store that can record ops.
	bd := bdWithHelp("Flags:\n      --if-revision int   fence\n")
	const optIn = "set the [ops] gates to auto or require to record ops here"
	for _, tc := range []struct {
		name  string
		store beads.Store
		where string
		// cannot is the reason an explicit gate would not record either,
		// empty for a store that can record ops.
		cannot string
	}{
		{"a Dolt-served store", cliOpBdLeafProxy{Store: newOpSweepStore(), leaf: bd}, "a Dolt-served sessions store", ""},
		// The default gc init topology: a proxied store whose writes land in
		// a released bd, which lacks --if-revision.
		{"a Dolt-served store behind a released bd", cliOpBdLeafProxy{Store: newOpSweepStore(), leaf: bdWithHelp("Flags:\n      --json   JSON output\n")}, "a Dolt-served sessions store", legacyop.DegradeConditionalWritesUnsupported},
		{"a capable memory store", newOpSweepStore(), "not SQLite-backed", ""},
		{"no conditional writes", incapableStore{beads.NewMemStore()}, "not SQLite-backed", legacyop.DegradeConditionalWritesUnsupported},
		{"no namespace", &beads.MemStore{HonorExplicitIDs: true}, "not SQLite-backed", legacyop.DegradeNoNamespace},
	} {
		t.Run(tc.name+" is OK", func(t *testing.T) {
			r := defaulted(tc.store, opSweepT0).Run(&doctor.CheckContext{})
			if r.Status != doctor.StatusOK || !strings.Contains(r.Message, "auto (default) records nothing on") || !strings.Contains(r.Message, tc.where) {
				t.Fatalf("result = %v %q, want OK saying the default records nothing on %s", r.Status, r.Message, tc.where)
			}
			if tc.cannot == "" {
				if !strings.Contains(r.Message, optIn) {
					t.Fatalf("message %q, want the opt-in for a store that can record ops", r.Message)
				}
				return
			}
			if strings.Contains(r.Message, optIn) || !strings.Contains(r.Message, "cannot record ops ("+tc.cannot+")") {
				t.Fatalf("message %q, want no opt-in and the store's own reason it cannot record ops (%s)", r.Message, tc.cannot)
			}
		})
	}
	t.Run("incapable store holding op rows warns", func(t *testing.T) {
		store := newOpSweepStore()
		seedOp(t, store, &clock.Fake{Time: opSweepT0}, opjournal.ExecutorLegacyCLI, "orphan")
		r := defaulted(incapableStore{store}, opSweepT0).Run(&doctor.CheckContext{})
		if r.Status != doctor.StatusWarning || !strings.Contains(r.Message, "auto by default") || !strings.Contains(r.Message, "cannot be checked") {
			t.Fatalf("result = %v %q, want a default Warning naming the unchecked op rows", r.Status, r.Message)
		}
		if strings.Contains(r.FixHint, "set the ops gates to off") {
			t.Fatalf("remedy %q offers turning the gates off, which does not reach the op rows", r.FixHint)
		}
	})
	t.Run("a Dolt-served store holding an expired op warns about it", func(t *testing.T) {
		store := newOpSweepStore()
		op := seedOp(t, store, &clock.Fake{Time: opSweepT0}, opjournal.ExecutorLegacyCLI, "left")
		r := defaulted(cliOpBdLeafProxy{Store: store, leaf: bd}, op.FinishBy.Add(opsRecordingStaleAfter+time.Second)).Run(&doctor.CheckContext{})
		if r.Status != doctor.StatusWarning || !strings.Contains(r.Message, "a Dolt-served sessions store") || !strings.Contains(r.Message, "expired") {
			t.Fatalf("result = %v %q, want a Warning naming the default and the expired op", r.Status, r.Message)
		}
	})
	t.Run("explicit auto stays loud", func(t *testing.T) {
		c := defaulted(incapableStore{beads.NewMemStore()}, opSweepT0)
		c.flags = rollout.ForTest(rollout.WithOpsControl(rollout.Auto))
		if r := c.Run(&doctor.CheckContext{}); r.Status != doctor.StatusWarning {
			t.Fatalf("status = %v, want Warning under an explicit auto: %s", r.Status, r.Message)
		}
	})
	t.Run("explicit auto over a capable non-SQLite store records", func(t *testing.T) {
		c := defaulted(newOpSweepStore(), opSweepT0)
		c.flags = rollout.ForTest(rollout.WithOpsContent(rollout.Auto))
		if r := c.Run(&doctor.CheckContext{}); r.Status != doctor.StatusOK || !strings.Contains(r.Message, "can record ops") {
			t.Fatalf("result = %v %q, want OK, the store can record ops", r.Status, r.Message)
		}
	})
	t.Run("SQLite store records", func(t *testing.T) {
		r := defaulted(newOpSweepSQLiteStore(t), opSweepT0).Run(&doctor.CheckContext{})
		if r.Status != doctor.StatusOK || !strings.Contains(r.Message, "can record ops") {
			t.Fatalf("result = %v %q, want OK, the store can record ops", r.Status, r.Message)
		}
	})
}
