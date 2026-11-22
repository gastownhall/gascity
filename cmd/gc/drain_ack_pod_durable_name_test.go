package main

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/reconcilekey"
	"github.com/gastownhall/gascity/internal/runtime"
	sessionpkg "github.com/gastownhall/gascity/internal/session"
)

// TestCurrentSessionRuntimeTargetDerivesDurableName pins the pod shape: the
// runtime session name may be the container-local tmux "main"
// (GC_TMUX_SESSION), while the durable row name this session's row and claims
// live under is GC_SESSION_NAME, else the display identity — never the
// container-local name. Without GC_TMUX_SESSION the ordinary local shape
// keeps the two equal.
func TestCurrentSessionRuntimeTargetDerivesDurableName(t *testing.T) {
	cityDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(cityDir, ".gc"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GC_CITY", cityDir)
	t.Setenv("GC_ALIAS", "worker-7")
	t.Setenv("GC_SESSION_ID", "gc-42")
	t.Setenv("GC_TMUX_SESSION", "main")
	t.Setenv("GC_SESSION_NAME", "worker-7")

	got, err := currentSessionRuntimeTarget()
	if err != nil {
		t.Fatalf("currentSessionRuntimeTarget(): %v", err)
	}
	if got.sessionName != "main" {
		t.Fatalf("sessionName = %q, want the container-local main", got.sessionName)
	}
	if got.durableRowName() != "worker-7" {
		t.Fatalf("durableRowName = %q, want the GC_SESSION_NAME row name", got.durableRowName())
	}

	t.Run("no GC_SESSION_NAME falls back to the display identity", func(t *testing.T) {
		t.Setenv("GC_SESSION_NAME", "")
		got, err := currentSessionRuntimeTarget()
		if err != nil {
			t.Fatalf("currentSessionRuntimeTarget(): %v", err)
		}
		if got.durableRowName() != "worker-7" {
			t.Fatalf("durableRowName = %q, want the display identity, never the container-local name", got.durableRowName())
		}
	})

	t.Run("local shape keeps runtime and durable equal", func(t *testing.T) {
		t.Setenv("GC_TMUX_SESSION", "")
		t.Setenv("GC_SESSION_NAME", "s-gc-42")
		got, err := currentSessionRuntimeTarget()
		if err != nil {
			t.Fatalf("currentSessionRuntimeTarget(): %v", err)
		}
		if got.sessionName != "s-gc-42" || got.durableRowName() != "s-gc-42" {
			t.Fatalf("sessionName %q, durableRowName %q: want both the logical session name", got.sessionName, got.durableRowName())
		}
	})
}

// podDrainAckTarget is a worker pod's own drain-ack target: the runtime name
// the provider's metadata operations use is the container-local "main", the
// durable row name is the logical session the row and claims live under.
func podDrainAckTarget(cityPath string) sessionRuntimeTarget {
	return sessionRuntimeTarget{
		cityPath:    cityPath,
		display:     "worker",
		sessionName: "main",
		durableName: "worker",
	}
}

// runPodDrainAckCity seeds one unexecuted claim on the seat and runs a
// pod-shaped drain-ack over drainAckCity's session row ("worker", instance
// token tok-a, generation 3), in either reconciler mode. It returns the exit
// code, the fake provider's recorded calls, the poke key, the row ack, and
// the claim read back through a fresh store handle.
func runPodDrainAckCity(t *testing.T, reconciler string) (code int, calls []runtime.Call, poke *reconcilekey.Key, row string, released beads.Bead, stderr string) {
	t.Helper()
	cityPath, store, id := drainAckCity(t, reconciler)
	claim := mustCreateDrainAckBead(t, store, beads.Bead{Title: "claimed, never executed", Type: "task", Metadata: routedToWorker()}, "in_progress", id)

	originalPoke := drainAckPokeController
	t.Cleanup(func() { drainAckPokeController = originalPoke })
	drainAckPokeController = func(_ string, key reconcilekey.Key) error {
		k := key
		poke = &k
		return nil
	}
	sp := runtime.NewFake()
	var out, errBuf bytes.Buffer
	t.Setenv("GC_INSTANCE_TOKEN", "tok-a")
	t.Setenv("GC_TMUX_SESSION", "main")
	code = runtimeDrainAck(newDrainOps(sp), podDrainAckTarget(cityPath), false, "tok-a", false, &out, &errBuf)

	cfg, loadErr := loadCityConfigWithoutBuiltinPackRefresh(cityPath, io.Discard)
	if loadErr != nil {
		t.Fatalf("load city: %v", loadErr)
	}
	fresh, openErr := openCityStoreAtWithConfig(cityPath, cfg)
	if openErr != nil {
		t.Fatalf("open fresh store: %v", openErr)
	}
	released, _ = fresh.Get(claim.ID)
	return code, sp.Calls, poke, rowAck(t, store, id), released, errBuf.String()
}

// TestPodDrainAckResolvesDurableRowAndWritesRuntimeMeta drives both
// reconciler modes for a worker pod whose runtime name (main) differs from
// its durable session name: the held-claim release and the v2 row
// acknowledgement resolve by the durable name — resolving by "main" would
// find no row, release nothing and refuse the v2 check — while the
// controller-visible ack metadata is written to "main", the session the
// controller observes through pod exec, and the poke names the durable row.
func TestPodDrainAckResolvesDurableRowAndWritesRuntimeMeta(t *testing.T) {
	for _, reconciler := range []string{"legacy", "v2"} {
		t.Run(reconciler, func(t *testing.T) {
			code, calls, poke, row, claim, stderr := runPodDrainAckCity(t, reconciler)
			if code != 0 {
				t.Fatalf("pod drain-ack = %d, want 0; stderr=%s", code, stderr)
			}
			if assignee := claim.Assignee; assignee != "" {
				t.Fatalf("claim assignee after the pod drain-ack = %q, want released (empty): the pass must resolve the durable session name", assignee)
			}
			// Every controller-visible metadata write went to "main"...
			for _, c := range calls {
				if c.Method == "SetMeta" && c.Name != "main" {
					t.Fatalf("SetMeta call on %q, want every ack write on the container-local main: %+v", c.Name, c)
				}
			}
			metaAck := slices.ContainsFunc(calls, func(c runtime.Call) bool {
				return c.Method == "SetMeta" && c.Name == "main" && c.Key == "GC_DRAIN_ACK" && c.Value == "1"
			})
			if !metaAck {
				t.Fatalf("no GC_DRAIN_ACK write on main: %+v", calls)
			}
			// ...and the poke names the durable row, not "main".
			if poke == nil {
				t.Fatal("no poke recorded")
			}
			if poke.SessionName != "worker" {
				t.Fatalf("poke session name = %q, want the durable row name worker", poke.SessionName)
			}
			if reconciler == "v2" && row == noRowAck {
				t.Fatal("v2 row ack did not land by the durable name")
			}
			if reconciler == "legacy" && row != noRowAck {
				t.Fatalf("legacy city wrote a row ack: %q", row)
			}
		})
	}
}

// TestPodDrainAckRefusesStartedHeldWorkBeforeAckWrite pins the refusal order
// for a pod-shaped seat: when the durable-named seat holds started work, the
// release pass refuses before any ack write — no metadata on "main", no v2
// row ack — in both reconciler modes.
func TestPodDrainAckRefusesStartedHeldWorkBeforeAckWrite(t *testing.T) {
	for _, reconciler := range []string{"legacy", "v2"} {
		t.Run(reconciler, func(t *testing.T) {
			cityPath, store, id := drainAckCity(t, reconciler)
			started := mustCreateDrainAckBead(t, store, beads.Bead{Title: "started, unfinished", Type: "task", Metadata: routedToWorker()}, "in_progress", id)
			// The work_started cell is a CAS-only key; the city store is a
			// policy store that resolves its conditional writer through the
			// wrapped target, not through ConditionalWriterFor.
			writer, ok := beads.ConditionalWriterForTarget(store)
			if !ok {
				t.Fatalf("store %T exposes no conditional writer", store)
			}
			row, err := store.Get(id)
			if err != nil {
				t.Fatal(err)
			}
			if err := writer.UpdateIfMatch(id, row.Revision, beads.UpdateOpts{Metadata: map[string]string{sessionpkg.WorkStartedKey: startedCell(sessionpkg.WorkStartedEntry{Bead: started.ID})}}); err != nil {
				t.Fatalf("writing the work_started cell: %v", err)
			}

			originalPoke := drainAckPokeController
			t.Cleanup(func() { drainAckPokeController = originalPoke })
			drainAckPokeController = func(string, reconcilekey.Key) error { return nil }
			sp := runtime.NewFake()
			var out, errBuf bytes.Buffer
			t.Setenv("GC_INSTANCE_TOKEN", "tok-a")
			t.Setenv("GC_TMUX_SESSION", "main")
			code := runtimeDrainAck(newDrainOps(sp), podDrainAckTarget(cityPath), false, "tok-a", false, &out, &errBuf)

			if code != drainAckRefused {
				t.Fatalf("pod drain-ack holding started work = %d, want %d (refused); stderr=%s", code, drainAckRefused, errBuf.String())
			}
			if len(sp.Calls) != 0 {
				t.Fatalf("refused ack wrote runtime metadata: %+v", sp.Calls)
			}
			if ack := rowAck(t, store, id); ack != noRowAck {
				t.Fatalf("refused ack wrote the row ack: %q", ack)
			}
			if !bytes.Contains(errBuf.Bytes(), []byte("this session holds")) {
				t.Fatalf("stderr %q: want the started-work refusal named", errBuf.String())
			}
			if status, assignee := drainAckBeadStatus(t, store, started.ID); assignee == "" {
				t.Fatalf("started claim %s: assignee emptied by a refused ack; status %q", started.ID, status)
			}
		})
	}
}
