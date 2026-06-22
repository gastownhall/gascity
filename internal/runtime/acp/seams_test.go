package acp

import (
	"context"
	"errors"
	"testing"

	"github.com/gastownhall/gascity/internal/runtime"
)

// TestSeamsAcpLifecycle drives the acp provider through the typed seams against
// the fake ACP server: provision, observe liveness, deliver a prompt via Nudge,
// confirm Exec is unsupported, then tear down.
func TestSeamsAcpLifecycle(t *testing.T) {
	rt, tp := newTestProvider(t).Seams()
	ctx := context.Background()
	name := testName()

	place, err := rt.Provision(ctx, name, runtime.ProvisionRequest{Config: runtime.Config{
		Command: fakeACPShellCommand(),
		WorkDir: t.TempDir(),
	}})
	if err != nil {
		t.Fatalf("provision: %v", err)
	}
	t.Cleanup(func() { _ = place.Teardown(ctx) })

	if ok, err := place.IsRunning(ctx); err != nil || !ok {
		t.Fatalf("IsRunning = %v, %v; want true, nil", ok, err)
	}
	if _, err := place.Exec(ctx, runtime.ExecRequest{Argv: []string{"echo", "hi"}}); !errors.Is(err, runtime.ErrExecUnsupported) {
		t.Fatalf("Exec err = %v; want ErrExecUnsupported", err)
	}
	if _, ok, err := rt.Open(ctx, name); err != nil || !ok {
		t.Fatalf("Open(live) = %v, %v; want true, nil", ok, err)
	}

	att, err := tp.Launch(ctx, place, runtime.LaunchSpec{})
	if err != nil {
		t.Fatalf("launch: %v", err)
	}
	if obs, err := att.Observe(ctx, nil); err != nil || !obs.ProcessAlive {
		t.Fatalf("Observe = %+v, %v; want ProcessAlive true", obs, err)
	}
	if err := att.Nudge(ctx, runtime.TextContent("hello")); err != nil {
		t.Fatalf("Nudge: %v", err)
	}
	if err := att.SendKeys(ctx, "Enter"); err != nil {
		t.Fatalf("SendKeys: %v", err)
	}
	if err := att.ClearScrollback(ctx); err != nil {
		t.Fatalf("ClearScrollback: %v", err)
	}
	if err := att.Close(ctx); err != nil {
		t.Fatalf("Close: %v", err)
	}

	if err := place.Teardown(ctx); err != nil {
		t.Fatalf("teardown: %v", err)
	}
	if ok, _ := place.IsRunning(ctx); ok {
		t.Fatal("session still running after teardown")
	}
}

// TestSeamsAcpTransportAndCaps pins the bespoke "acp" transport identity and the
// (empty) capability mapping.
func TestSeamsAcpTransportAndCaps(t *testing.T) {
	rt, tp := newTestProvider(t).Seams()

	if caps := rt.Capabilities(); caps.ReportActivity {
		t.Fatalf("PlaceCapabilities = %+v; want ReportActivity false (acp declares none)", caps)
	}
	if tp.Capabilities().ReportAttachment {
		t.Fatal("TransportCapabilities.ReportAttachment should be false for acp")
	}
	if tp.Name() != "acp" {
		t.Fatalf("Name = %q; want acp", tp.Name())
	}
	if err := tp.Attach(context.Background(), nil, "x"); err == nil {
		t.Fatal("Attach should be unsupported for acp")
	}
}

// TestSeamsAcpMetaStore proves the MetaStore seam round-trips through acp's
// sidecar-file meta.
func TestSeamsAcpMetaStore(t *testing.T) {
	rt, _ := newTestProvider(t).Seams()
	ms, ok := rt.(runtime.MetaStore)
	if !ok {
		t.Fatal("acp Runtime should implement runtime.MetaStore")
	}
	name := testName()
	if err := ms.SetMeta(name, "k", "v"); err != nil {
		t.Fatalf("SetMeta: %v", err)
	}
	if got, err := ms.GetMeta(name, "k"); err != nil || got != "v" {
		t.Fatalf("GetMeta = %q, %v; want v, nil", got, err)
	}
	if err := ms.RemoveMeta(name, "k"); err != nil {
		t.Fatalf("RemoveMeta: %v", err)
	}
	if got, _ := ms.GetMeta(name, "k"); got != "" {
		t.Fatalf("GetMeta after remove = %q; want empty", got)
	}
}

// TestSeamsAcpOpenAbsent proves Open returns (nil,false,nil) for an unknown session.
func TestSeamsAcpOpenAbsent(t *testing.T) {
	rt, _ := newTestProvider(t).Seams()
	if pl, ok, err := rt.Open(context.Background(), "ghost"); pl != nil || ok || err != nil {
		t.Fatalf("Open(absent) = %v, %v, %v; want nil, false, nil", pl, ok, err)
	}
}
