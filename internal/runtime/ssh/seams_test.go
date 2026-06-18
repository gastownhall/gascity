package ssh

import (
	"context"
	"errors"
	"testing"

	"github.com/gastownhall/gascity/internal/runtime"
)

// TestSeamsSshExecAndOpen proves Place.Exec delegates over the ssh connection
// (preserving the (output, code, err) contract) and Runtime.Open reflects tmux
// liveness.
func TestSeamsSshExecAndOpen(t *testing.T) {
	f := &fakeRunner{respond: func(argv []string) ([]byte, int, error) {
		switch {
		case isTmux("has-session")(argv):
			return nil, 0, nil // running
		case len(argv) > 0 && argv[0] == "echo":
			return []byte("hi\n"), 0, nil
		case len(argv) > 0 && argv[0] == "false":
			return nil, 7, nil // command's own non-zero exit
		case len(argv) > 0 && argv[0] == "boom":
			return nil, -1, errors.New("ssh transport failure")
		}
		return nil, 0, nil
	}}
	p := providerWith(f)
	rt, _ := p.Seams()
	ctx := context.Background()

	place, ok, err := rt.Open(ctx, "s")
	if err != nil || !ok {
		t.Fatalf("Open(live) = %v, %v; want true, nil", ok, err)
	}

	res, err := place.Exec(ctx, runtime.ExecRequest{Argv: []string{"echo", "hi"}})
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if string(res.Output) != "hi\n" || res.Code != 0 {
		t.Fatalf("Exec = %q, code %d; want hi, 0", res.Output, res.Code)
	}

	// A non-zero exit is the command's own result, not an error.
	res, err = place.Exec(ctx, runtime.ExecRequest{Argv: []string{"false"}})
	if err != nil {
		t.Fatalf("Exec(false) err: %v; want nil (command's own exit)", err)
	}
	if res.Code != 7 {
		t.Fatalf("Exec(false) code = %d; want 7", res.Code)
	}

	// A transport failure is surfaced as an error with an empty result.
	res, err = place.Exec(ctx, runtime.ExecRequest{Argv: []string{"boom"}})
	if err == nil {
		t.Fatal("Exec(boom) should return a transport error")
	}
	if res.Output != nil || res.Code != 0 {
		t.Fatalf("Exec transport-error result = %q, code %d; want empty", res.Output, res.Code)
	}
}

// TestSeamsSshOpenAbsent proves Open returns (nil,false,nil) when the remote
// tmux session is gone.
func TestSeamsSshOpenAbsent(t *testing.T) {
	f := &fakeRunner{code: 1} // has-session exits 1 → not running
	rt, _ := providerWith(f).Seams()
	if pl, ok, err := rt.Open(context.Background(), "ghost"); pl != nil || ok || err != nil {
		t.Fatalf("Open(absent) = %v, %v, %v; want nil, false, nil", pl, ok, err)
	}
}

// TestSeamsSshProvision proves Provision: the happy path yields a usable Place,
// a bad tmux name is rejected, and a duplicate surfaces ErrSessionExists — all
// through the seam.
func TestSeamsSshProvision(t *testing.T) {
	ctx := context.Background()

	// Happy path: name not yet running → new-session succeeds → usable Place.
	notRunning := &fakeRunner{respond: func(argv []string) ([]byte, int, error) {
		if isTmux("has-session")(argv) {
			return nil, 1, nil // not yet running
		}
		return nil, 0, nil // new-session (and everything else) ok
	}}
	rt, _ := providerWith(notRunning).Seams()
	place, err := rt.Provision(ctx, "s", runtime.ProvisionRequest{Config: runtime.Config{Command: "agent"}})
	if err != nil {
		t.Fatalf("Provision: %v", err)
	}
	if sp, ok := place.(*sshPlace); !ok || sp.name != "s" {
		t.Fatalf("Provision place = %#v; want *sshPlace name=s", place)
	}

	// A bad tmux name is rejected.
	if _, err := rt.Provision(ctx, "bad.name", runtime.ProvisionRequest{Config: runtime.Config{Command: "agent"}}); !errors.Is(err, ErrInvalidSessionName) {
		t.Fatalf("Provision(bad name) err = %v; want ErrInvalidSessionName", err)
	}

	// A duplicate (has-session exits 0) surfaces ErrSessionExists.
	running := &fakeRunner{code: 0}
	rtDup, _ := providerWith(running).Seams()
	if _, err := rtDup.Provision(ctx, "s", runtime.ProvisionRequest{Config: runtime.Config{Command: "agent"}}); !errors.Is(err, runtime.ErrSessionExists) {
		t.Fatalf("Provision(dup) err = %v; want ErrSessionExists", err)
	}
}

// TestSeamsSshCapabilitiesAndTransport proves the capability split and transport
// identity.
func TestSeamsSshCapabilitiesAndTransport(t *testing.T) {
	rt, tp := providerWith(&fakeRunner{}).Seams()

	if caps := rt.Capabilities(); !caps.ReportActivity {
		t.Fatalf("PlaceCapabilities = %+v; want ReportActivity true", caps)
	}
	if tp.Capabilities().ReportAttachment {
		t.Fatal("TransportCapabilities.ReportAttachment should be false for ssh")
	}
	if tp.Name() != "tmux" {
		t.Fatalf("Name = %q; want tmux", tp.Name())
	}
}

// TestSeamsSshMetaStore proves the MetaStore seam round-trips through the tmux
// session environment.
func TestSeamsSshMetaStore(t *testing.T) {
	f := &fakeRunner{respond: func(argv []string) ([]byte, int, error) {
		if isTmux("show-environment")(argv) {
			if len(argv) > 0 && argv[len(argv)-1] == "missing" {
				return []byte("-missing"), 0, nil // tmux prints "-KEY" when unset
			}
			return []byte("k=v"), 0, nil
		}
		return nil, 0, nil
	}}
	rt, _ := providerWith(f).Seams()

	ms, ok := rt.(runtime.MetaStore)
	if !ok {
		t.Fatal("ssh Runtime should implement runtime.MetaStore")
	}
	if err := ms.SetMeta("s", "k", "v"); err != nil {
		t.Fatalf("SetMeta: %v", err)
	}
	if got, err := ms.GetMeta("s", "k"); err != nil || got != "v" {
		t.Fatalf("GetMeta = %q, %v; want v, nil", got, err)
	}
	// An explicitly-unset key parses to empty.
	if got, err := ms.GetMeta("s", "missing"); err != nil || got != "" {
		t.Fatalf("GetMeta(unset) = %q, %v; want empty, nil", got, err)
	}
	if err := ms.RemoveMeta("s", "k"); err != nil {
		t.Fatalf("RemoveMeta: %v", err)
	}
}

// TestSeamsSshObserve proves Attachment.Observe folds the liveness reads:
// ProcessAlive via pgrep on the box, Attached from #{session_attached}.
func TestSeamsSshObserve(t *testing.T) {
	f := &fakeRunner{respond: func(argv []string) ([]byte, int, error) {
		last := ""
		if len(argv) > 0 {
			last = argv[len(argv)-1]
		}
		switch {
		case isTmux("has-session")(argv):
			return nil, 0, nil
		case len(argv) > 0 && argv[0] == "pgrep":
			return []byte("1234\n"), 0, nil
		case last == "#{session_attached}":
			return []byte("1"), 0, nil // attached
		case last == "#{session_activity}":
			return []byte("1700000000"), 0, nil // last-activity epoch seconds
		}
		return nil, 0, nil
	}}
	p := providerWith(f)
	rt, tp := p.Seams()
	ctx := context.Background()

	place, ok, err := rt.Open(ctx, "s")
	if err != nil || !ok {
		t.Fatalf("Open: %v, %v", ok, err)
	}
	att, err := tp.Launch(ctx, place, runtime.LaunchSpec{Config: runtime.Config{ProcessNames: []string{"claude"}}})
	if err != nil {
		t.Fatalf("launch: %v", err)
	}

	// Observe folds all three reads: ProcessAlive from pgrep, Attached parsed
	// from #{session_attached}=1, LastActivity parsed from #{session_activity}.
	obs, err := att.Observe(ctx)
	if err != nil {
		t.Fatalf("Observe: %v", err)
	}
	if !obs.ProcessAlive {
		t.Fatal("Observe ProcessAlive = false; want true (pgrep found it)")
	}
	if !obs.Attached {
		t.Fatal("Observe Attached = false; want true (#{session_attached}=1)")
	}
	if obs.LastActivity.Unix() != 1700000000 {
		t.Fatalf("Observe LastActivity = %d; want 1700000000", obs.LastActivity.Unix())
	}
}

// TestSeamsSshTransportOpen proves Transport.Open (reconnect) returns a live
// Attachment for a running session and (nil,false,nil) for a dead one.
func TestSeamsSshTransportOpen(t *testing.T) {
	ctx := context.Background()

	live := providerWith(&fakeRunner{code: 0}) // has-session exits 0
	rt, tp := live.Seams()
	place, ok, err := rt.Open(ctx, "s")
	if err != nil || !ok {
		t.Fatalf("Open(live): %v, %v", ok, err)
	}
	att, aok, aerr := tp.Open(ctx, place, "s")
	if att == nil || !aok || aerr != nil {
		t.Fatalf("Transport.Open(live) = %v, %v, %v; want attachment, true, nil", att, aok, aerr)
	}
	// A reconnect carries no processNames, so Observe falls back to box liveness
	// (ProcessAlive true via the empty-names contract).
	if obs, oerr := att.Observe(ctx); oerr != nil || !obs.ProcessAlive {
		t.Fatalf("Observe(reconnect) = %+v, %v; want ProcessAlive true", obs, oerr)
	}

	dead := &sshPlace{p: providerWith(&fakeRunner{code: 1}), name: "ghost"} // has-session exits 1
	if att, ok, err := tp.Open(ctx, dead, "ghost"); att != nil || ok || err != nil {
		t.Fatalf("Transport.Open(dead) = %v, %v, %v; want nil, false, nil", att, ok, err)
	}
}

// TestSeamsSshStageAndTeardown proves Stage is a no-op (v0 ssh has no CopyTo) and
// Teardown kills the remote tmux session.
func TestSeamsSshStageAndTeardown(t *testing.T) {
	f := &fakeRunner{}
	p := providerWith(f)
	place := &sshPlace{p: p, name: "s"}
	ctx := context.Background()

	if err := place.Stage(ctx, []runtime.CopyEntry{{Src: "/a", RelDst: "x"}}); err != nil {
		t.Fatalf("Stage = %v; want nil (v0 no-op)", err)
	}
	if err := place.Teardown(ctx); err != nil {
		t.Fatalf("Teardown: %v", err)
	}
	if firstCall(f, isTmux("kill-session")) == nil {
		t.Fatal("Teardown should issue tmux kill-session")
	}
}
