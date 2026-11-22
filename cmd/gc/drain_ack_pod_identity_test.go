package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/reconcilekey"
	"github.com/gastownhall/gascity/internal/runtime"
	sessionpkg "github.com/gastownhall/gascity/internal/session"
)

// podDrainAckEnv is a generated pod's session environment for drainAckCity's
// row "worker": the runtime name inside the pod is "main", the durable session
// name is "worker", and the display identity is an alias naming neither.
func podDrainAckEnv(t *testing.T, cityPath string) {
	t.Helper()
	t.Setenv("GC_CITY", cityPath)
	t.Setenv("GC_ALIAS", "worker-alias")
	t.Setenv("GC_SESSION_NAME", "worker")
	t.Setenv("GC_TMUX_SESSION", "main")
	t.Setenv("GC_INSTANCE_TOKEN", "tok-a")
	t.Setenv("GC_SESSION", "fake")
}

// TestDrainAckInPodResolvesTheDurableSessionIdentity: a pod's worker, whose
// GC_TMUX_SESSION is "main" and whose GC_SESSION_NAME names its session row,
// acknowledges a drain through the hook, the runtime command and the shared
// ack. In a legacy and a v2 city the row check, the held-claim release and the
// controller poke resolve the row by its durable name, while the runtime ack
// is written to "main" and the report names "main" as the runtime session. A
// seat holding started work is refused before any ack write: no row ack, no
// runtime ack, no poke, and the claim stays held. Kills: drain-ack passing the
// pod-local "main" to the row, the release or the poke.
func TestDrainAckInPodResolvesTheDurableSessionIdentity(t *testing.T) {
	type outcome struct {
		code    int
		stdout  string
		stderr  string
		runtime *runtime.Fake // nil when the entry point constructs its own
	}
	entries := []struct {
		name string
		ack  func(t *testing.T) outcome
	}{
		{name: "hook", ack: func(*testing.T) outcome {
			var stderr bytes.Buffer
			code := 0
			if err := hookRuntimeDrainAck(&stderr); errors.Is(err, errDrainAckRefused) {
				code = drainAckRefused
			} else if err != nil {
				code = 1
			}
			return outcome{code: code, stderr: stderr.String()}
		}},
		{name: "runtime command", ack: func(*testing.T) outcome {
			var stdout, stderr bytes.Buffer
			code := cmdRuntimeDrainAck(nil, true, false, &stdout, &stderr)
			return outcome{code: code, stdout: stdout.String(), stderr: stderr.String()}
		}},
		{name: "current-session ack", ack: func(t *testing.T) outcome {
			target, err := currentSessionRuntimeTarget()
			if err != nil {
				t.Fatalf("currentSessionRuntimeTarget: %v", err)
			}
			sp := runtime.NewFake()
			var stdout, stderr bytes.Buffer
			code := runtimeDrainAck(newDrainOps(sp), target, false, "tok-a", true, &stdout, &stderr)
			return outcome{code: code, stdout: stdout.String(), stderr: stderr.String(), runtime: sp}
		}},
	}
	for _, mode := range []struct {
		name       string
		reconciler string
	}{{name: "legacy"}, {name: "v2", reconciler: "v2"}} {
		for _, entry := range entries {
			for _, started := range []bool{false, true} {
				name := mode.name + "/" + entry.name + "/held claim released"
				if started {
					name = mode.name + "/" + entry.name + "/started claim refuses"
				}
				t.Run(name, func(t *testing.T) {
					clearGCEnv(t)
					cityPath, store, id := drainAckCity(t, mode.reconciler)
					claim := mustCreateDrainAckBead(t, store, beads.Bead{Title: "claimed", Type: "task", Metadata: routedToWorker()}, "in_progress", id)
					if started {
						resolvedCASSetMeta(t, store, id, sessionpkg.WorkStartedKey, startedCell(sessionpkg.WorkStartedEntry{Bead: claim.ID}))
					}
					podDrainAckEnv(t, cityPath)
					originalCaller, originalPoke := drainAckCallerSessionID, drainAckPokeController
					t.Cleanup(func() { drainAckCallerSessionID, drainAckPokeController = originalCaller, originalPoke })
					drainAckCallerSessionID = func() string { return "" }
					var pokes []reconcilekey.Key
					drainAckPokeController = func(_ string, key reconcilekey.Key) error {
						pokes = append(pokes, key)
						return nil
					}

					got := entry.ack(t)

					row := freshDrainAckRow(t, cityPath, id)
					ack := row.Metadata[sessionpkg.DrainAckIncarnationKey] + "@" + row.Metadata[sessionpkg.DrainAckAtKey]
					status, assignee := drainAckBeadStatus(t, mustOpenFreshCityStore(t, cityPath), claim.ID)
					if started {
						if got.code != drainAckRefused || len(pokes) != 0 || ack != noRowAck || status != "in_progress" || assignee != id {
							t.Fatalf("started-work ack: code %d (want %d), pokes %v, row ack %q, claim %s/%s; want a refusal with nothing written; stderr=%s",
								got.code, drainAckRefused, pokes, ack, status, assignee, got.stderr)
						}
						if got.runtime != nil {
							for _, c := range got.runtime.Calls {
								if c.Method == "SetMeta" {
									t.Fatalf("started-work ack wrote runtime metadata %+v before refusing", c)
								}
							}
						}
						return
					}
					if got.code != 0 {
						t.Fatalf("drain-ack = %d, want 0; stderr=%s", got.code, got.stderr)
					}
					if status != "open" || assignee != "" {
						t.Fatalf("held claim is %s/%q after the ack; want released by the durable session name; stderr=%s", status, assignee, got.stderr)
					}
					if mode.reconciler == "v2" && ack == noRowAck {
						t.Fatalf("v2 row ack missing on row %q; stderr=%s", id, got.stderr)
					}
					if len(pokes) != 1 || pokes[0] != reconcilekey.SessionRef("", "worker") {
						t.Fatalf("pokes = %+v; want one for the durable session %q", pokes, "worker")
					}
					if got.runtime != nil {
						var acked []string
						for _, c := range got.runtime.Calls {
							if c.Method == "SetMeta" && c.Key == "GC_DRAIN_ACK" {
								acked = append(acked, c.Name)
							}
						}
						if len(acked) != 1 || acked[0] != "main" {
							t.Fatalf("runtime ack written to %q; want only the pod-local runtime %q", acked, "main")
						}
					}
					if got.stdout != "" {
						var report runtimeActionJSON
						if err := json.Unmarshal([]byte(got.stdout), &report); err != nil {
							t.Fatalf("decoding report %q: %v", got.stdout, err)
						}
						if report.Session != "main" || report.Target != "worker-alias" {
							t.Fatalf("report session %q target %q; want the runtime name %q and the display %q", report.Session, report.Target, "main", "worker-alias")
						}
					}
				})
			}
		}
	}
}

// mustOpenFreshCityStore opens the store of the city at cityPath through a new
// handle, which sees the writes a CLI run made through its own.
func mustOpenFreshCityStore(t *testing.T, cityPath string) beads.Store {
	t.Helper()
	cfg, err := loadCityConfigWithoutBuiltinPackRefresh(cityPath, io.Discard)
	if err != nil {
		t.Fatalf("load city: %v", err)
	}
	store, err := openCityStoreAtWithConfig(cityPath, cfg)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	return store
}

// resolvedCASSetMeta writes key=value on bead id by compare-and-swap through
// the conditional writer store resolves to, as the city store's policy wrapper
// hides it from a direct type assertion.
func resolvedCASSetMeta(t *testing.T, store beads.Store, id, key, value string) {
	t.Helper()
	w, _, err := beads.ResolveConditionalWriter(store)
	if err != nil || w == nil {
		t.Fatalf("store %T resolves no conditional writer: %v", store, err)
	}
	b, err := store.Get(id)
	if err != nil {
		t.Fatal(err)
	}
	if err := w.UpdateIfMatch(id, b.Revision, beads.UpdateOpts{Metadata: map[string]string{key: value}}); err != nil {
		t.Fatal(err)
	}
}
