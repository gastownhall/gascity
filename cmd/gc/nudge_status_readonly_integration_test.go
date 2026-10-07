//go:build integration

package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/nudgequeue"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/session"
)

// TestCmdGCIntegrationNudgeStatusPreservesAvailableStore verifies repeated
// command reads preserve session records, runtime state and queue snapshots.
func TestCmdGCIntegrationNudgeStatusPreservesAvailableStore(t *testing.T) {
	for _, kind := range []string{"unmaterialized_named", "unmaterialized_named_empty_queue", "existing_named", "existing_named_empty_queue", "closed_named", "explicit_session", "unknown", "unavailable_read"} {
		t.Run(kind, func(t *testing.T) {
			f := newNudgeStatusReadonlyFixture(t)
			target := "status-reader"
			sessionID := ""
			existingNamed := kind == "existing_named" || kind == "existing_named_empty_queue"
			unmaterialized := kind == "unmaterialized_named" || kind == "unmaterialized_named_empty_queue"
			emptyQueue := kind == "existing_named_empty_queue" || kind == "unmaterialized_named_empty_queue"
			wantCount := 1
			if emptyQueue {
				wantCount = 0
			}
			if existingNamed || kind == "closed_named" || kind == "explicit_session" {
				meta := map[string]string{
					"template": "status-reader", "agent_name": "status-reader", "alias": "status-reader",
					"session_name": "status-reader", "provider": "codex", "transport": "fake", "state": "asleep",
				}
				if kind != "explicit_session" {
					meta[session.NamedSessionMetadataKey] = "true"
					meta[session.NamedSessionIdentityMetadata] = "status-reader"
					meta[session.NamedSessionModeMetadata] = "on-demand"
				}
				b, err := f.raw.Create(beads.Bead{
					ID: "session-existing", Title: "persisted session", Type: session.BeadType,
					Labels: []string{session.LabelSession}, Metadata: meta,
				})
				if err != nil {
					t.Fatal(err)
				}
				sessionID = b.ID
				if kind == "closed_named" {
					if err := f.raw.Close(b.ID); err != nil {
						t.Fatal(err)
					}
				}
				if kind == "explicit_session" {
					target = b.ID
				}
			}
			if kind == "unknown" {
				target = "genuinely-unknown-target"
			}
			if kind == "unavailable_read" {
				f.store.readErr = errors.New("store unavailable: injected read failure")
			}
			f.seedQueue(t, sessionID, emptyQueue)
			before := f.records(t)
			queueBefore := f.queueBytes(t)
			for _, jsonOutput := range []bool{true, false, true, false} {
				var stdout, stderr bytes.Buffer
				cmd := newNudgeCmd(&stdout, &stderr)
				cmd.SilenceErrors, cmd.SilenceUsage = true, true
				args := []string{"status", target}
				if jsonOutput {
					args = append(args, "--json")
				}
				cmd.SetArgs(args)
				err := cmd.Execute()
				if kind == "unknown" || kind == "unavailable_read" {
					if err == nil {
						t.Errorf("failed target rendered a healthy status: err=%v stdout=%q", err, stdout.String())
					}
					diagnostic := stderr.String()
					if stdout.Len() != 0 {
						// Preserve the existing typed JSON failure contract; an
						// error envelope is not a healthy queue status.
						var failure jsonSchemaErrorPayload
						if !jsonOutput || json.Unmarshal(stdout.Bytes(), &failure) != nil || failure.SchemaVersion != "1" || failure.OK || failure.Error.Code == "" || failure.Error.ExitCode <= 0 {
							t.Errorf("failed target rendered healthy or invalid output: %q", stdout.String())
						} else {
							diagnostic += "\n" + failure.Error.Message
						}
					}
					want := "session not found"
					if kind == "unavailable_read" {
						want = "store unavailable"
					}
					if !bytes.Contains([]byte(diagnostic), []byte(want)) {
						t.Errorf("diagnostic must distinguish %s: %q", kind, diagnostic)
					}
				} else {
					if err != nil || stderr.Len() != 0 {
						t.Errorf("configured/persisted target status: err=%v stderr=%q", err, stderr.String())
					}
					if jsonOutput {
						var status nudgeStatusJSON
						if err := json.Unmarshal(stdout.Bytes(), &status); err != nil {
							t.Errorf("decode status: %v, stdout=%q", err, stdout.String())
						} else {
							if status.SchemaVersion != "1" || status.Command != "nudge status" || status.Agent != "status-reader" {
								t.Errorf("status identity/header changed: %+v", status)
							}
							if status.Counts.Pending != wantCount || status.Counts.InFlight != wantCount || status.Counts.Dead != wantCount || len(status.Pending) != wantCount || len(status.InFlight) != wantCount || len(status.Dead) != wantCount {
								t.Errorf("queue buckets or unrelated-agent filtering changed: %+v", status)
							}
							if status.Pending == nil || status.InFlight == nil || status.Dead == nil {
								t.Errorf("empty queue buckets must remain arrays: %+v", status)
							}
							if status.DispatchSkips["not-running"] != 3 {
								t.Errorf("dispatch diagnostics changed: %v", status.DispatchSkips)
							}
							if unmaterialized && status.SessionID != "" {
								t.Errorf("status fabricated a persisted session id: %q", status.SessionID)
							}
							if (existingNamed || kind == "explicit_session") && status.SessionID != sessionID {
								t.Errorf("persisted target changed: got %q want %q", status.SessionID, sessionID)
							}
							if len(status.Pending) == 1 && len(status.InFlight) == 1 && len(status.Dead) == 1 &&
								(status.Pending[0].ID != "pending-own" || status.InFlight[0].ID != "in-flight-own" || status.Dead[0].ID != "dead-own") {
								t.Errorf("status replaced queued items: %+v", status)
							}
						}
					} else if !bytes.Contains(stdout.Bytes(), []byte("status-reader")) || !bytes.Contains(stdout.Bytes(), []byte("not-running=3")) {
						t.Errorf("text status lost target or skip totals: %q", stdout.String())
					}
				}
				if after := f.records(t); !bytes.Equal(after, before) {
					t.Errorf("status changed persisted bead records for %s (json=%t)", kind, jsonOutput)
				}
				if after := f.queueBytes(t); !bytes.Equal(after, queueBefore) {
					t.Errorf("status changed persisted queue bytes: before=%s after=%s", queueBefore, after)
				}
			}
			if len(f.store.writes) != 0 {
				t.Errorf("status entered session/store mutation paths: %v", f.store.writes)
			}
			for _, call := range f.provider.SnapshotCalls() {
				switch call.Method {
				case "Start", "Stop", "Relaunch", "Nudge", "NudgeNow", "SetMeta", "SendKeys", "CopyTo":
					t.Errorf("status mutated runtime/provider: %s(%q)", call.Method, call.Name)
				}
			}
		})
	}
}

type nudgeStatusReadonlyFixture struct {
	root     string
	raw      beads.Store
	store    *nudgeStatusMutationSpy
	provider *runtime.Fake
}

func newNudgeStatusReadonlyFixture(t *testing.T) nudgeStatusReadonlyFixture {
	t.Helper()
	clearGCEnv(t)
	disableManagedDoltRecoveryForTest(t)
	f := nudgeStatusReadonlyFixture{root: t.TempDir(), provider: runtime.NewFake()}
	t.Setenv("GC_BEADS", "file")
	t.Setenv("GC_SESSION", "fake")
	t.Setenv("GC_CITY_PATH", f.root)
	for path, body := range map[string]string{
		"pack.toml":     "[pack]\nname = \"status-fixture\"\nschema = 2\n\n[[named_session]]\ntemplate = \"status-reader\"\n",
		"city.toml":     "[workspace]\n\n[beads]\nprovider = \"file\"\n\n[session]\nprovider = \"fake\"\n",
		".gc/site.toml": "workspace_name = \"status-fixture\"\n",
	} {
		full := filepath.Join(f.root, path)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	writeBuiltinImportsFixture(t, f.root, "core")
	writeCatalogFile(t, f.root, "agents/status-reader/agent.toml", "provider = \"codex\"\nstart_command = \"echo\"\n")
	f.raw = openNudgeBeadStore(f.root).Store
	if f.raw == nil {
		t.Fatal("fixture file store must be available")
	}
	if _, err := f.raw.Create(beads.Bead{Title: "unrelated existing task"}); err != nil {
		t.Fatal(err)
	}
	f.store = &nudgeStatusMutationSpy{Store: f.raw}
	oldOpen, oldBuild := openNudgeBeadStore, buildSessionProviderByName
	openNudgeBeadStore = func(root string) beads.NudgesStore {
		if root != f.root {
			t.Fatalf("status escaped test city: %q", root)
		}
		return beads.NudgesStore{Store: f.store}
	}
	buildSessionProviderByName = func(*config.City, string, config.SessionConfig, string, string) (runtime.Provider, error) {
		return f.provider, nil
	}
	t.Cleanup(func() { openNudgeBeadStore, buildSessionProviderByName = oldOpen, oldBuild })
	return f
}

func (f nudgeStatusReadonlyFixture) seedQueue(t *testing.T, sessionID string, empty bool) {
	t.Helper()
	now := time.Now().UTC()
	item := func(id, agent string) nudgequeue.Item {
		return nudgequeue.Item{ID: id, Agent: agent, SessionID: sessionID, Source: "test", Message: id, CreatedAt: now, DeliverAfter: now, ExpiresAt: now.Add(24 * time.Hour)}
	}
	if err := nudgequeue.WithState(f.root, func(state *nudgequeue.State) error {
		state.DispatchSkips = map[string]int64{"not-running": 3}
		if empty {
			return nil
		}
		state.Pending = []nudgequeue.Item{item("pending-own", "status-reader"), item("pending-unrelated", "another-agent")}
		state.Pending[1].SessionID = ""
		state.InFlight = []nudgequeue.Item{item("in-flight-own", "status-reader")}
		state.InFlight[0].ClaimedAt, state.InFlight[0].LeaseUntil = now, now.Add(time.Hour)
		state.Dead = []nudgequeue.Item{item("dead-own", "status-reader")}
		state.Dead[0].DeadAt = now
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func (f nudgeStatusReadonlyFixture) records(t *testing.T) []byte {
	t.Helper()
	rows, err := f.raw.List(beads.ListQuery{AllowScan: true, IncludeClosed: true})
	if err != nil {
		t.Fatal(err)
	}
	body, err := json.Marshal(rows)
	if err != nil {
		t.Fatal(err)
	}
	return body
}

func (f nudgeStatusReadonlyFixture) queueBytes(t *testing.T) []byte {
	t.Helper()
	body, err := os.ReadFile(nudgequeue.StatePath(f.root))
	if err != nil {
		t.Fatal(err)
	}
	return body
}

type nudgeStatusMutationSpy struct {
	beads.Store
	writes  []string
	readErr error
}

func (s *nudgeStatusMutationSpy) Get(id string) (beads.Bead, error) {
	if s.readErr != nil {
		return beads.Bead{}, s.readErr
	}
	return s.Store.Get(id)
}

func (s *nudgeStatusMutationSpy) Create(b beads.Bead) (beads.Bead, error) {
	s.writes = append(s.writes, "Create")
	return s.Store.Create(b)
}

func (s *nudgeStatusMutationSpy) Update(id string, opts beads.UpdateOpts) error {
	s.writes = append(s.writes, "Update")
	return s.Store.Update(id, opts)
}

func (s *nudgeStatusMutationSpy) Reopen(id string) error {
	s.writes = append(s.writes, "Reopen")
	return s.Store.Reopen(id)
}

func (s *nudgeStatusMutationSpy) SetMetadata(id, key, value string) error {
	s.writes = append(s.writes, fmt.Sprintf("SetMetadata(%s)", key))
	return s.Store.SetMetadata(id, key, value)
}

func (s *nudgeStatusMutationSpy) SetMetadataBatch(id string, kvs map[string]string) error {
	s.writes = append(s.writes, "SetMetadataBatch")
	return s.Store.SetMetadataBatch(id, kvs)
}
