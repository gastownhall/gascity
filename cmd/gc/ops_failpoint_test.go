package main

import (
	"bytes"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/opjournal"
	"github.com/gastownhall/gascity/internal/opjournal/legacyop"
)

// opsFailpointHarness is an opsFailpoints whose variable reads raw (unset
// when raw is ""), whose stamp reads hooks, whose notices land in notice and
// whose kills and parks are counted.
type opsFailpointHarness struct {
	*opsFailpoints
	buf   *bytes.Buffer
	kills int
	parks int
}

func newOpsFailpointHarness(raw string, hooks bool) *opsFailpointHarness {
	h := &opsFailpointHarness{buf: &bytes.Buffer{}}
	h.opsFailpoints = &opsFailpoints{
		lookupEnv: func(name string) (string, bool) {
			if name != opsFailpointEnv || raw == "" {
				return "", false
			}
			return raw, true
		},
		hooksEnabled: func() bool { return hooks },
		notice:       h.buf,
		kill:         func() { h.kills++ },
		park:         func() { h.parks++ },
	}
	return h
}

func fpOp(id string, kind opjournal.Kind) opjournal.Op {
	return opjournal.Op{ID: id, Kind: kind}
}

func TestParseOpsFailpoint(t *testing.T) {
	valid := []struct {
		raw     string
		surface string
		point   legacyop.CrashPoint
		kind    opjournal.Kind
		n       int
	}{
		{"api:k1", legacyop.SurfaceAPI, legacyop.CrashPointAdmitted, "", 1},
		{"api:k2", legacyop.SurfaceAPI, legacyop.CrashPointAnswered, "", 1},
		{"api:k3", legacyop.SurfaceAPI, legacyop.CrashPointEffected, "", 1},
		{"api:k4", legacyop.SurfaceAPI, legacyop.CrashPointClosed, "", 1},
		{"api:k5", legacyop.SurfaceAPI, legacyop.CrashPointClosing, "", 1},
		{"api:k3:message", legacyop.SurfaceAPI, legacyop.CrashPointEffected, opjournal.KindMessage, 1},
		{"api:k3:2", legacyop.SurfaceAPI, legacyop.CrashPointEffected, "", 2},
		{"api:k3:message:2", legacyop.SurfaceAPI, legacyop.CrashPointEffected, opjournal.KindMessage, 2},
		{"cli:k1:kill", legacyop.SurfaceCLI, legacyop.CrashPointAdmitted, opjournal.KindKill, 1},
		{"cli:k4:wake:12", legacyop.SurfaceCLI, legacyop.CrashPointClosed, opjournal.KindWake, 12},
	}
	for _, tc := range valid {
		fp, err := parseOpsFailpoint(tc.raw)
		if err != nil || fp.surface != tc.surface || fp.point != tc.point || fp.kind != tc.kind || fp.n != tc.n {
			t.Errorf("parseOpsFailpoint(%q) = %+v, %v", tc.raw, fp, err)
		}
	}
	rejected := map[string]string{
		"api":                    "not <surface>",
		"api:k1:message:2:3":     "not <surface>",
		"ctl:k1":                 "unknown surface",
		"api:k6":                 "unknown point",
		"api:K1":                 "unknown point",
		"cli:k2":                 "the api surface's only",
		"cli:k5":                 "the api surface's only",
		"api:k1:0":               "not a whole number",
		"api:k1:-1":              "not a whole number",
		"api:k1:01":              "not a whole number",
		"api:k1:+1":              "not a whole number",
		"api:k1:2:message":       "not the last field",
		"api:k1:message:kill":    "two kinds",
		"api:k1:nosuchkind":      "unknown kind",
		"api:k1:":                "unknown kind",
		":k1":                    "unknown surface",
		"api::message":           "unknown point",
		"api:k1:message:message": "two kinds",
	}
	for raw, why := range rejected {
		if fp, err := parseOpsFailpoint(raw); err == nil || !strings.Contains(err.Error(), why) {
			t.Errorf("parseOpsFailpoint(%q) = %+v, %v; want an error naming %q", raw, fp, err, why)
		}
	}
}

func TestOpsFailpointGating(t *testing.T) {
	t.Run("without the stamp it is ignored, once, with no hook", func(t *testing.T) {
		h := newOpsFailpointHarness("api:k1", false)
		for range 3 {
			if h.hook(legacyop.SurfaceAPI) != nil {
				t.Fatal("a hook without the stamp")
			}
		}
		want := `gc: WARNING: GC_OPS_FAILPOINT="api:k1": ignored: this gc binary is not built with test hooks (test-only knob)` + "\n"
		if got := h.buf.String(); got != want {
			t.Fatalf("notice %q, want %q", got, want)
		}
	})
	t.Run("unset says nothing and hooks nothing", func(t *testing.T) {
		h := newOpsFailpointHarness("", true)
		if h.hook(legacyop.SurfaceAPI) != nil || h.hook(legacyop.SurfaceCLI) != nil || h.buf.Len() != 0 {
			t.Fatalf("notice %q", h.buf.String())
		}
	})
	t.Run("a malformed value is ignored with why", func(t *testing.T) {
		h := newOpsFailpointHarness("cli:k2", true)
		if h.hook(legacyop.SurfaceCLI) != nil || !strings.Contains(h.buf.String(), "ignored: point k2 is the api surface's only") {
			t.Fatalf("notice %q", h.buf.String())
		}
	})
	t.Run("armed: announced once, a hook for the armed surface only", func(t *testing.T) {
		h := newOpsFailpointHarness("api:k3:message:2", true)
		if h.buf.Len() != 0 {
			t.Fatal("notice before the first consultation")
		}
		if h.hook(legacyop.SurfaceCLI) != nil {
			t.Fatal("a cli hook for an api failpoint")
		}
		if h.hook(legacyop.SurfaceAPI) == nil {
			t.Fatal("no api hook")
		}
		want := `gc: WARNING: GC_OPS_FAILPOINT="api:k3:message:2": TEST HOOK ACTIVE: this process kills itself (SIGKILL) at k3 (effected) of its api ops of kind message (hit 2)` + "\n"
		if got := h.buf.String(); got != want {
			t.Fatalf("notice %q, want %q", got, want)
		}
	})
	t.Run("each action's armed notice", func(t *testing.T) {
		for raw, want := range map[string]string{
			"api:k2": "TEST HOOK ACTIVE: this process parks the request at k2 (answered) of its api ops",
			"api:k5": "TEST HOOK ACTIVE: this process fails every close of one op at k5 (closing) of its api ops",
			"cli:k1": "TEST HOOK ACTIVE: this process kills itself (SIGKILL) at k1 (admitted) of its cli ops",
		} {
			h := newOpsFailpointHarness(raw, true)
			h.load()
			if !strings.HasSuffix(h.buf.String(), want+"\n") {
				t.Errorf("%s: notice %q, want it to end %q", raw, h.buf.String(), want)
			}
		}
	})
}

func TestOpsFailpointKillsAtTheArmedPoint(t *testing.T) {
	for _, name := range []string{"k1", "k3", "k4"} {
		t.Run(name, func(t *testing.T) {
			h := newOpsFailpointHarness("api:"+name+":kill:3", true)
			at := h.hook(legacyop.SurfaceAPI)
			point := opsFailpointPoints[name].point
			other := legacyop.CrashPointAnswered
			for _, hit := range []struct {
				p    legacyop.CrashPoint
				kind opjournal.Kind
			}{
				{other, opjournal.KindKill},          // another point
				{point, opjournal.KindMessage},       // another kind
				{point, opjournal.KindKill},          // hit 1
				{legacyop.CrashPointClosing, "kill"}, // another point
				{point, opjournal.KindKill},          // hit 2
			} {
				if err := at(hit.p, fpOp("gc-op-1", hit.kind)); err != nil {
					t.Fatalf("hook error %v", err)
				}
			}
			if h.kills != 0 {
				t.Fatalf("fired before the third hit (%d kills)", h.kills)
			}
			_ = at(point, fpOp("gc-op-3", opjournal.KindKill)) // hit 3
			_ = at(point, fpOp("gc-op-4", opjournal.KindKill)) // hit 4
			if h.kills != 1 || h.parks != 0 {
				t.Fatalf("%d kills, %d parks; want exactly one kill at the third hit", h.kills, h.parks)
			}
		})
	}
}

func TestOpsFailpointParksAtK2(t *testing.T) {
	h := newOpsFailpointHarness("api:k2", true)
	at := h.hook(legacyop.SurfaceAPI)
	_ = at(legacyop.CrashPointAdmitted, fpOp("gc-op-1", opjournal.KindMessage))
	_ = at(legacyop.CrashPointAnswered, fpOp("gc-op-1", opjournal.KindMessage))
	_ = at(legacyop.CrashPointAnswered, fpOp("gc-op-2", opjournal.KindMessage))
	if h.parks != 1 || h.kills != 0 {
		t.Fatalf("%d parks, %d kills; want one park and no kill", h.parks, h.kills)
	}
	want := `gc: WARNING: GC_OPS_FAILPOINT="api:k2": TEST HOOK ACTIVE: op gc-op-1 parked after its answer, before its effect` + "\n"
	if !strings.HasSuffix(h.buf.String(), want) || strings.Count(h.buf.String(), "parked") != 1 {
		t.Fatalf("notice %q, want it to end %q", h.buf.String(), want)
	}
}

func TestOpsFailpointK5FaultsOneOpEveryTime(t *testing.T) {
	h := newOpsFailpointHarness("api:k5:message:2", true)
	at := h.hook(legacyop.SurfaceAPI)
	closing := legacyop.CrashPointClosing
	// The first op to reach a close, twice: it is not the faulted one, and
	// its second attempt counts no second op.
	for range 2 {
		if err := at(closing, fpOp("gc-op-1", opjournal.KindMessage)); err != nil {
			t.Fatalf("op 1: %v", err)
		}
	}
	if err := at(closing, fpOp("gc-op-k", opjournal.KindKill)); err != nil {
		t.Fatalf("an op of another kind: %v", err)
	}
	if err := at(legacyop.CrashPointClosed, fpOp("gc-op-2", opjournal.KindMessage)); err != nil {
		t.Fatalf("another point: %v", err)
	}
	for i := range 4 {
		if err := at(closing, fpOp("gc-op-2", opjournal.KindMessage)); !errors.Is(err, errOpsFailpointCloseFault) {
			t.Fatalf("close %d of op 2: %v, want the injected fault", i+1, err)
		}
	}
	for _, id := range []string{"gc-op-1", "gc-op-3"} {
		if err := at(closing, fpOp(id, opjournal.KindMessage)); err != nil {
			t.Fatalf("%s: %v; only the faulted op fails", id, err)
		}
	}
	if n := strings.Count(h.buf.String(), "every close of it fails"); n != 1 || !strings.Contains(h.buf.String(), "op gc-op-2: every close of it fails from now on") {
		t.Fatalf("notice %q, want one fault line naming gc-op-2", h.buf.String())
	}
	if h.kills != 0 || h.parks != 0 {
		t.Fatalf("%d kills, %d parks", h.kills, h.parks)
	}
}

// The process state wires the cli hook into newCLIOp's recorder and the api
// hook into controllerState, and neither when unarmed.
func TestOpsFailpointWiring(t *testing.T) {
	old := opsFailpointState
	t.Cleanup(func() { opsFailpointState = old })
	cfg := &config.City{}
	newOp := func() *cliOp {
		return newCLIOp(cfg, &beads.MemStore{}, cliOpRequest{verb: "gc session wake", kind: opjournal.KindWake, stderr: io.Discard})
	}

	opsFailpointState = newOpsFailpointHarness("", true).opsFailpoints
	if newOp().rec.CrashPoints != nil || (&controllerState{}).OpsCrashPoints() != nil {
		t.Fatal("a hook with no failpoint armed")
	}

	cli := newOpsFailpointHarness("cli:k1", true)
	opsFailpointState = cli.opsFailpoints
	if (&controllerState{}).OpsCrashPoints() != nil {
		t.Fatal("controllerState carries a cli failpoint")
	}
	hook := newOp().rec.CrashPoints
	if hook == nil {
		t.Fatal("newCLIOp's recorder carries no hook")
	}
	_ = hook(legacyop.CrashPointAdmitted, fpOp("gc-op-1", opjournal.KindWake))
	if cli.kills != 1 {
		t.Fatalf("the cli recorder's hook did not fire (%d kills)", cli.kills)
	}

	api := newOpsFailpointHarness("api:k1", true)
	opsFailpointState = api.opsFailpoints
	if newOp().rec.CrashPoints != nil {
		t.Fatal("the cli recorder carries an api failpoint")
	}
	hook = (&controllerState{}).OpsCrashPoints()
	if hook == nil {
		t.Fatal("controllerState carries no hook")
	}
	_ = hook(legacyop.CrashPointAdmitted, fpOp("gc-op-1", opjournal.KindMessage))
	if api.kills != 1 {
		t.Fatalf("the api hook did not fire (%d kills)", api.kills)
	}
}

// The lane failpoint's notices go through the shared helper, byte for byte.
func TestFailpointNoticeFormat(t *testing.T) {
	var buf bytes.Buffer
	failpointNotice(&buf, laneFailpointEnv, `ctl:"done"`, "msg")
	if want := `gc: WARNING: GC_LANE_FAILPOINT="ctl:\"done\"": msg` + "\n"; buf.String() != want {
		t.Fatalf("notice %q, want %q", buf.String(), want)
	}
}
