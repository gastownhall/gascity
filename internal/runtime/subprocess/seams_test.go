package subprocess

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/runtime"
)

// TestSeamsLifecycle drives the subprocess provider through the de-conflated
// typed seams (Runtime → Place → Transport → Attachment) to prove the new
// contract hosts a real session: provision (welded launch), observe liveness,
// reject a duplicate, re-resolve, confirm the null driving surface no-ops, then
// tear down and confirm the box is gone.
func TestSeamsLifecycle(t *testing.T) {
	rt, tp := NewProviderWithDir(t.TempDir()).Seams()
	ctx := context.Background()

	place, err := rt.Provision(ctx, "seam", runtime.ProvisionRequest{
		Config: runtime.Config{Command: "sleep 3600"},
	})
	if err != nil {
		t.Fatalf("provision: %v", err)
	}
	t.Cleanup(func() { _ = place.Teardown(ctx) })

	if ok, err := place.IsRunning(ctx); err != nil || !ok {
		t.Fatalf("IsRunning = %v, %v; want true, nil", ok, err)
	}

	// Subprocess exposes no in-box exec op.
	if _, err := place.Exec(ctx, runtime.ExecRequest{Argv: []string{"echo", "hi"}}); !errors.Is(err, runtime.ErrExecUnsupported) {
		t.Fatalf("Exec err = %v; want ErrExecUnsupported", err)
	}

	// A duplicate Provision of a live session is rejected (←Start ErrSessionExists).
	if _, err := rt.Provision(ctx, "seam", runtime.ProvisionRequest{
		Config: runtime.Config{Command: "sleep 3600"},
	}); !errors.Is(err, runtime.ErrSessionExists) {
		t.Fatalf("duplicate Provision err = %v; want ErrSessionExists", err)
	}

	// Open re-resolves the same live box by name; an absent name is (nil,false,nil).
	if _, ok, err := rt.Open(ctx, "seam"); err != nil || !ok {
		t.Fatalf("Open(live) = %v, %v; want true, nil", ok, err)
	}
	if pl, ok, err := rt.Open(ctx, "no-such-session"); pl != nil || ok || err != nil {
		t.Fatalf("Open(absent) = %v, %v, %v; want nil, false, nil", pl, ok, err)
	}
	if names, err := rt.List(ctx, "seam"); err != nil || len(names) != 1 {
		t.Fatalf("List = %v, %v; want one name", names, err)
	}

	// Transport.Launch is a no-op over the running box; the Attachment observes it.
	att, err := tp.Launch(ctx, place, runtime.LaunchSpec{})
	if err != nil {
		t.Fatalf("launch: %v", err)
	}
	if obs, err := att.Observe(ctx, nil); err != nil || !obs.ProcessAlive {
		t.Fatalf("Observe = %+v, %v; want ProcessAlive true", obs, err)
	}

	// Null driving verbs are best-effort no-ops.
	if _, err := att.Peek(ctx, 10); err != nil {
		t.Fatalf("Peek: %v", err)
	}
	if err := att.Nudge(ctx, runtime.TextContent("x")); err != nil {
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
		t.Fatalf("session still running after teardown")
	}
	// Observe and Open both reflect the torn-down box.
	if obs, err := att.Observe(ctx, nil); err != nil || obs.ProcessAlive {
		t.Fatalf("Observe after teardown = %+v, %v; want ProcessAlive false", obs, err)
	}
	if _, ok, err := rt.Open(ctx, "seam"); ok || err != nil {
		t.Fatalf("Open after teardown = ok %v, err %v; want false, nil", ok, err)
	}
}

// TestSeamsInterrupt proves Attachment.Interrupt actually delegates to the
// provider (SIGINT to the process group) rather than no-op'ing like its sibling
// verbs: the session must die.
func TestSeamsInterrupt(t *testing.T) {
	rt, tp := NewProviderWithDir(t.TempDir()).Seams()
	ctx := context.Background()

	place, err := rt.Provision(ctx, "intr", runtime.ProvisionRequest{
		Config: runtime.Config{Command: "sleep 3600"},
	})
	if err != nil {
		t.Fatalf("provision: %v", err)
	}
	t.Cleanup(func() { _ = place.Teardown(ctx) })

	att, err := tp.Launch(ctx, place, runtime.LaunchSpec{})
	if err != nil {
		t.Fatalf("launch: %v", err)
	}
	if err := att.Interrupt(ctx); err != nil {
		t.Fatalf("Interrupt: %v", err)
	}

	deadline := time.Now().Add(3 * time.Second)
	for {
		alive, _ := place.IsRunning(ctx)
		if !alive {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("session still alive after Interrupt (verb did not delegate)")
		}
		time.Sleep(20 * time.Millisecond)
	}
	if obs, err := att.Observe(ctx, nil); err != nil || obs.ProcessAlive {
		t.Fatalf("Observe after Interrupt = %+v, %v; want ProcessAlive false", obs, err)
	}
}

// TestSeamsStage proves Place.Stage delegates to CopyTo and actually copies a
// file into the session workdir.
func TestSeamsStage(t *testing.T) {
	rt, _ := NewProviderWithDir(t.TempDir()).Seams()
	ctx := context.Background()

	work := t.TempDir()
	src := filepath.Join(t.TempDir(), "payload.txt")
	if err := os.WriteFile(src, []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}

	place, err := rt.Provision(ctx, "stage", runtime.ProvisionRequest{
		Config: runtime.Config{Command: "sleep 3600", WorkDir: work},
	})
	if err != nil {
		t.Fatalf("provision: %v", err)
	}
	t.Cleanup(func() { _ = place.Teardown(ctx) })

	if err := place.Stage(ctx, []runtime.CopyEntry{{Src: src, RelDst: "copied.txt"}}); err != nil {
		t.Fatalf("Stage: %v", err)
	}
	if got, err := os.ReadFile(filepath.Join(work, "copied.txt")); err != nil || string(got) != "hello" {
		t.Fatalf("staged file = %q, %v; want hello", got, err)
	}
}

// TestSeamsMetaStore proves the Runtime also satisfies the MetaStore seam and
// round-trips meta through the same sidecar-file backing as the legacy view.
func TestSeamsMetaStore(t *testing.T) {
	rt, _ := NewProviderWithDir(t.TempDir()).Seams()
	ms, ok := rt.(runtime.MetaStore)
	if !ok {
		t.Fatal("subprocess Runtime should implement runtime.MetaStore")
	}
	if err := ms.SetMeta("s", "k", "v"); err != nil {
		t.Fatalf("SetMeta: %v", err)
	}
	if got, err := ms.GetMeta("s", "k"); err != nil || got != "v" {
		t.Fatalf("GetMeta = %q, %v; want v, nil", got, err)
	}
	if err := ms.RemoveMeta("s", "k"); err != nil {
		t.Fatalf("RemoveMeta: %v", err)
	}
	if got, _ := ms.GetMeta("s", "k"); got != "" {
		t.Fatalf("GetMeta after remove = %q; want empty", got)
	}
}

// TestSeamsTransportShape pins the detached transport's degenerate properties.
func TestSeamsTransportShape(t *testing.T) {
	_, tp := NewProviderWithDir(t.TempDir()).Seams()
	if tp.Name() != "detached" {
		t.Fatalf("Name = %q; want detached", tp.Name())
	}
	if err := tp.Attach(context.Background(), nil, "x"); err == nil {
		t.Fatal("Attach should be unsupported for subprocess")
	}
}
