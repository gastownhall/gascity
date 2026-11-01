package execenv

import (
	"os"
	"slices"
	"testing"
	"time"
)

func TestEnvironWaitsForAProjectionToRestore(t *testing.T) {
	t.Setenv("GC_EXECENV_PROJECTION_PROBE", "ambient")

	ProcessEnvLock.Lock()
	if err := os.Setenv("GC_EXECENV_PROJECTION_PROBE", "projected"); err != nil {
		ProcessEnvLock.Unlock()
		t.Fatal(err)
	}
	envCh := make(chan []string, 1)
	go func() { envCh <- Environ() }()
	select {
	case env := <-envCh:
		ProcessEnvLock.Unlock()
		t.Fatalf("Environ returned during a projection: %v", slices.DeleteFunc(env, func(e string) bool {
			return e != "GC_EXECENV_PROJECTION_PROBE=projected"
		}))
	case <-time.After(10 * time.Millisecond):
	}
	if err := os.Setenv("GC_EXECENV_PROJECTION_PROBE", "ambient"); err != nil {
		ProcessEnvLock.Unlock()
		t.Fatal(err)
	}
	ProcessEnvLock.Unlock()

	select {
	case env := <-envCh:
		if !slices.Contains(env, "GC_EXECENV_PROJECTION_PROBE=ambient") || slices.Contains(env, "GC_EXECENV_PROJECTION_PROBE=projected") {
			t.Fatalf("Environ after the projection restored = %v, want the ambient value only", env)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Environ did not return after the projection restored")
	}
}
