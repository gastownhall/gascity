package worker

import (
	"context"
	"reflect"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/beads/beadstest"
	"github.com/gastownhall/gascity/internal/runtime"
	sessionpkg "github.com/gastownhall/gascity/internal/session"
)

func TestBootstrapInputFreshCreateIsNotReusedOnSameHandleWake(t *testing.T) {
	sp := runtime.NewFake()
	manager := sessionpkg.NewManagerWithOptions(beadstest.Sessions(beads.NewMemStore()), sp, sessionpkg.WithCityPath(t.TempDir()))
	h, err := NewSessionHandle(SessionHandleConfig{Manager: manager, Session: SessionSpec{
		Template: "codex", Title: "Fresh only", Command: "codex", Provider: "codex", WorkDir: t.TempDir(),
		Hints: runtime.Config{BootstrapInputOrigin: runtime.BootstrapInputFreshBuiltin, ProviderName: "codex"},
	}})
	if err != nil {
		t.Fatal(err)
	}
	info, err := h.Create(context.Background(), CreateModeStarted)
	if err != nil {
		t.Fatal(err)
	}
	first := sp.LastStartConfig(info.SessionName)
	if first == nil || first.BootstrapInputOrigin != runtime.BootstrapInputFreshBuiltin {
		t.Fatal("fresh create lost its per-call provenance")
	}
	startCalls := func() int {
		n := 0
		for _, call := range sp.SnapshotCalls() {
			if call.Method == "Start" && call.Name == info.SessionName {
				n++
			}
		}
		return n
	}
	before := startCalls()
	if again, err := h.Create(context.Background(), CreateModeStarted); err != nil || again.ID != info.ID {
		t.Fatalf("same handle create=%+v err=%v", again, err)
	}
	if startCalls() != before || !reflect.DeepEqual(sp.LastStartConfig(info.SessionName), first) {
		t.Fatal("reused handle create started or regranted a runtime")
	}
	actor := handleActor(t, h, sessionpkg.ActorOperator)
	if err := h.Stop(context.Background(), actor); err != nil {
		t.Fatal(err)
	}
	if err := h.Start(context.Background(), actor); err != nil {
		t.Fatal(err)
	}
	woke := sp.LastStartConfig(info.SessionName)
	if woke == nil || woke.BootstrapInputOrigin != "" {
		t.Fatalf("same handle wake reused first-input provenance: %+v", woke)
	}
}
