package main

import (
	"context"
	"errors"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/doctor"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/runtime/auto"
	sessionexec "github.com/gastownhall/gascity/internal/runtime/exec"
)

// readinessLeaf is a provider that reports v2 readiness as given.
type readinessLeaf struct {
	runtime.Provider
	missing []string
	err     error
}

func (l readinessLeaf) V2Readiness() ([]string, error) { return l.missing, l.err }

// Kills (PAR-EXEC §5 item 7, C0.8) a boot that trusts the latch's probe over
// the provider the city built: a leaf whose own handshake lacks a v2
// capability refuses the boot, naming it; one whose handshake cannot be
// read is no verdict; a ready leaf, or one that reports no readiness
// (tmux), passes; under a composite every leaf is asked.
func TestC08ReadsTheBuiltProvidersHandshake(t *testing.T) {
	fake := runtime.NewFake()
	ready := readinessLeaf{Provider: fake}
	gapped := readinessLeaf{Provider: fake, missing: []string{runtime.ProtocolCapabilityReportAttachment}}
	unread := readinessLeaf{Provider: fake, err: errors.New("protocol: exit status 1")}
	if err := v2RuntimeReadiness(fake); err != nil {
		t.Fatalf("a provider that reports no readiness: %v, want nil", err)
	}
	if err := v2RuntimeReadiness(ready); err != nil {
		t.Fatalf("a ready leaf: %v, want nil", err)
	}
	err := v2RuntimeReadiness(gapped)
	var u *v2RuntimeUnreadError
	if err == nil || errors.As(err, &u) || !strings.Contains(err.Error(), "C0.8") || !strings.Contains(err.Error(), "report-attachment") {
		t.Fatalf("a gapped leaf: %v, want C0.8's refusal naming report-attachment", err)
	}
	if err := v2RuntimeReadiness(unread); !errors.As(err, &u) || !strings.Contains(err.Error(), "exit status 1") {
		t.Fatalf("an unreadable handshake: %v, want an unread error naming the cause", err)
	}
	if err := v2RuntimeReadiness(auto.New(ready, gapped)); err == nil || errors.As(err, &u) {
		t.Fatalf("a composite with a gapped leaf: %v, want the refusal", err)
	}
	// The registry-built provider over a script that dropped report-attachment.
	script := writeRPPScript(t, `[ "$1" = protocol ] && { echo '{"version":0,"capabilities":["observe.v1"]}'; exit 0; }; exit 2
`)
	if err := v2RuntimeReadiness(sessionexec.NewSeamBackedForCity(script, "c", t.TempDir())); err == nil || errors.As(err, &u) {
		t.Fatalf("the exec provider over a gapped script: %v, want the refusal", err)
	}
}

// Kills C0.8 skipped at boot, a handshake read failure read as a refusal or
// a pass, or a refusal that does not alert: boot asks runtimeReadiness after
// C0.7, retries an unread handshake with the boot backoff (boot state
// "runtime" meanwhile), and refuses with the boot-refused alert on a gap.
func TestBootRunsC08(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		stderr := &synchronizedBuffer{}
		rt := newDefaultPlanner(stderr)
		refusal := errors.New("v2 refuses to boot on an exec runtime that is not v2-ready (C0.8): gapped")
		calls, retryState := 0, ""
		rt.host.runtimeReadiness = func() error {
			calls++
			if calls == 1 {
				return &v2RuntimeUnreadError{Label: "the session runtime", Err: errors.New("timeout")}
			}
			retryState = rt.bootState()
			return refusal
		}
		start := time.Now()
		if err := rt.boot(context.Background()); !errors.Is(err, refusal) || calls != 2 {
			t.Fatalf("boot = %v after %d C0.8 reads, want the read retried once and then the refusal", err, calls)
		}
		if waited := time.Since(start); waited < v2LaneBaseBackoff*9/10 || waited > v2LaneBaseBackoff*11/10 {
			t.Errorf("boot retried the read after %s, want its first backoff", waited)
		}
		if retryState != plannerBootRuntime || rt.bootState() != plannerBootRuntime {
			t.Errorf("boot state %q while C0.8 retried, %q once it refused; want %q", retryState, rt.bootState(), plannerBootRuntime)
		}
		if !strings.Contains(stderr.String(), "C0.8 runtime handshake failed (retry in") || !strings.Contains(stderr.String(), alertBootRefused) {
			t.Fatalf("stderr %q, want the retry reported and the boot-refused alert", stderr.String())
		}
	})
}

// Kills a default v2 an exec handshake chose in silence (owner decision D6):
// with session_reconciler unset, a v2-ready exec runtime resolves to v2,
// the boot log says the handshake opted the city in, and doctor warns
// until the key is set; an explicit v2, or another runtime, says nothing.
// Doctor's detail names the runtime v2-ready, and warns when v2 runs on it
// without input-fence (D2), or names why it is not v2-ready.
func TestExecOptInIsAnnounced(t *testing.T) {
	ready := writeRPPScript(t, `[ "$1" = protocol ] && { echo '{"version":0,"capabilities":["observe.v1","report-attachment"]}'; exit 0; }; exit 2
`)
	fenced := writeRPPScript(t, `[ "$1" = protocol ] && { echo '{"version":0,"capabilities":["observe.v1","report-attachment","proc.exec","input-fence"]}'; exit 0; }; exit 2
`)
	gapped := writeRPPScript(t, `[ "$1" = protocol ] && { echo '{"version":0,"capabilities":["observe.v1"]}'; exit 0; }; exit 2
`)
	city := func(provider, mode string) *config.City {
		return v2DefaultCity(func(c *config.City) { c.Session.Provider, c.Daemon.SessionReconciler = provider, mode })
	}
	cfg := city("exec:"+ready, "")
	res, err := resolveReconcilerMode(cfg, nil)
	if err != nil || res.Mode != reconcilerV2 || res.Reason != reconcilerDefaultV2 {
		t.Fatalf("resolution %+v, %v; want the default v2", res, err)
	}
	boot := strings.Join(reconcilerBootWarnings(cfg, nil, res), "\n")
	if !strings.Contains(boot, "v2 by default on the exec session runtime") || !strings.Contains(boot, `session_reconciler = "v2"`) {
		t.Fatalf("boot warnings %q, want the opt-in announced", boot)
	}
	r := newSessionReconcilerDoctorCheck(cfg, nil).Run(&doctor.CheckContext{})
	if r.Status != doctor.StatusWarning || !strings.Contains(r.Message, "v2 by default on the exec session runtime") {
		t.Fatalf("doctor %v %q, want a warning naming the opt-in", r.Status, r.Message)
	}
	if !strings.Contains(strings.Join(r.Details, "\n"), "v2-ready (observe.v1, report-attachment)") || !strings.Contains(r.Message, "input-fence") {
		t.Fatalf("doctor details %q message %q, want the runtime v2-ready and the missing input fence", r.Details, r.Message)
	}
	explicit := city("exec:"+fenced, "v2")
	res, _ = resolveReconcilerMode(explicit, nil)
	if boot := reconcilerBootWarnings(explicit, nil, res); len(boot) != 0 {
		t.Fatalf("explicit v2 on a fenced exec runtime: boot warnings %q, want none", boot)
	}
	if r := newSessionReconcilerDoctorCheck(explicit, nil).Run(&doctor.CheckContext{}); r.Status != doctor.StatusOK {
		t.Fatalf("explicit v2 on a fenced exec runtime: doctor %v %q, want OK", r.Status, r.Message)
	}
	notReady := city("exec:"+gapped, "")
	r = newSessionReconcilerDoctorCheck(notReady, nil).Run(&doctor.CheckContext{})
	if !strings.Contains(strings.Join(r.Details, "\n"), "not v2-ready: its handshake does not declare report-attachment") || r.Status != doctor.StatusWarning {
		t.Fatalf("a gapped exec runtime: doctor %v %q %q, want the fallback warning and the gap named", r.Status, r.Message, r.Details)
	}
}
