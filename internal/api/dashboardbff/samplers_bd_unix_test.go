//go:build unix

package dashboardbff

// Every case here installs a fake `bd` on the fixture PATH as a POSIX shell
// script, and the cancellation case additionally orders its handshake with a
// FIFO. Both are Unix file-system mechanisms with no direct Windows analog,
// so these live apart from the provider-neutral sampler tests rather than
// being skipped at runtime. The parsing, refresh and transport tests in
// samplers_test.go carry no such dependency and run everywhere.

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestExecBdPingUsesProviderNeutralArgsAndIsolatesSocket(t *testing.T) {
	root := t.TempDir()
	beadsPath := filepath.Join(root, ".beads")
	if err := os.Mkdir(beadsPath, 0o755); err != nil {
		t.Fatal(err)
	}
	argsFile := filepath.Join(root, "args")
	bin := filepath.Join(root, "bin")
	if err := os.Mkdir(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	script := "#!/bin/sh\nprintf '%s\\n' \"$@\" > \"" + argsFile + "\"\nprintf 'socket=%s\\n' \"${BEADS_DOLT_SERVER_SOCKET-unset}\" >> \"" + argsFile + "\"\nprintf '%s' '{\"status\":\"ok\"}'\n"
	if err := os.WriteFile(filepath.Join(bin, "bd"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ADMIN_PATH", bin)
	// Ambient transport must never leak into dashboard probes. The target
	// store's metadata (not the caller environment) owns transport selection.
	t.Setenv("BEADS_DOLT_SERVER_SOCKET", "/tmp/ambient-dolt.sock")
	// exec.Command resolves the executable using the parent PATH before the
	// runner applies its scrubbed child environment.
	t.Setenv("PATH", bin)
	before, err := snapshotProbeDir(beadsPath)
	if err != nil {
		t.Fatal(err)
	}
	res, err := newExecRunner().execBdPing(context.Background(), beadsPath)
	if err != nil {
		t.Fatalf("execBdPing() error = %v", err)
	}
	if res.exitCode != 0 {
		t.Fatalf("execBdPing() exit = %d stdout=%q stderr=%q", res.exitCode, res.stdout, res.stderr)
	}
	args, err := os.ReadFile(argsFile)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(args)), "\n")
	if len(lines) != 5 || !equalStrings(lines[:4], []string{"ping", "--db", beadsPath, "--json"}) || lines[4] != "socket=unset" {
		t.Fatalf("bd argv/environment = %v", lines)
	}
	after, err := snapshotProbeDir(beadsPath)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(before, after) {
		t.Fatalf("probe mutated .beads directory: before=%v after=%v", before, after)
	}
	var payload map[string]string
	if err := json.Unmarshal([]byte(res.stdout), &payload); err != nil || payload["status"] != "ok" {
		t.Fatalf("ping output = %q", res.stdout)
	}
}

func TestProbeRigExecutionFailuresFailClosedAndDoNotMutateStore(t *testing.T) {
	tests := []struct {
		name string
		// arrange installs the case's fake bd and returns the context to probe
		// with. They are one step because the post-spawn cancellation case
		// cancels on a signal from the child it installs.
		arrange func(t *testing.T, binDir string) context.Context
		// pingTimeout overrides the runner's default when non-zero, so the row
		// stays the single source of truth for its own case.
		pingTimeout time.Duration
		// queueFull occupies every exec slot before probing, leaving run's
		// entry select with only its ctx.Done arm ready.
		queueFull bool
		// Exactly one of these is set: the whole message when it is fixed, a
		// prefix when the tail is stdlib error text.
		wantMessage       string
		wantMessagePrefix string
	}{
		{
			name: "timeout",
			arrange: func(t *testing.T, binDir string) context.Context {
				writeFakeBd(t, binDir, "#!/bin/sh\n/bin/sleep 1\n")
				return context.Background()
			},
			pingTimeout: 20 * time.Millisecond,
			wantMessage: "bd ping execution failed: exec timed out",
		},
		{
			name: "spawn failure",
			arrange: func(*testing.T, string) context.Context {
				return context.Background()
			},
			wantMessagePrefix: "bd ping execution failed: spawn failed: ",
		},
		{
			// Cancellation while queued, before the probe holds a slot. A
			// saturated semaphore is what makes this arm deterministic: with a
			// free slot both select arms are ready and Go picks at random.
			name: "cancellation before start",
			arrange: func(t *testing.T, binDir string) context.Context {
				writeFakeBd(t, binDir, "#!/bin/sh\nexec /bin/sleep 1\n")
				ctx, cancel := context.WithCancel(context.Background())
				cancel()
				return ctx
			},
			queueFull:   true,
			wantMessage: "bd ping execution failed: exec canceled before start",
		},
		{
			// Cancellation after bd is already running, which is the only case
			// that reaches run's post-Run context.Canceled classification.
			name: "cancellation after spawn",
			arrange: func(t *testing.T, binDir string) context.Context {
				// The child announces itself through a FIFO rather than a
				// polled sentinel file: opening a FIFO for write blocks until
				// a reader opens it, so the handshake below orders the cancel
				// after the child is running off the child's own lifecycle
				// instead of elapsed wall time. ":" and ">" are shell builtins
				// and /bin/sleep is absolute, so both survive the runner's
				// scrubbed child environment. exec replaces the shell with the
				// sleep, so the kill lands on the process itself and leaves no
				// grandchild holding stdout.
				started := filepath.Join(t.TempDir(), "bd-started")
				if err := syscall.Mkfifo(started, 0o600); err != nil {
					t.Fatalf("Mkfifo(%q) = %v", started, err)
				}
				writeFakeBd(t, binDir, "#!/bin/sh\n: > '"+started+"'\nexec /bin/sleep 30\n")
				ctx, cancel := context.WithCancel(context.Background())
				go func() {
					defer cancel()
					// Returns once the child reaches its redirection.
					if f, err := os.Open(started); err == nil {
						_ = f.Close()
					}
				}()
				t.Cleanup(func() {
					// A child that never ran leaves the reader above blocked;
					// a non-blocking write open releases it. ENXIO here means
					// the handshake already completed, which is the normal path.
					if f, err := os.OpenFile(started, os.O_WRONLY|syscall.O_NONBLOCK, 0); err == nil {
						_ = f.Close()
					}
				})
				return ctx
			},
			wantMessage: "bd ping execution failed: exec canceled",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if (tt.wantMessage == "") == (tt.wantMessagePrefix == "") {
				t.Fatal("row must set exactly one of wantMessage/wantMessagePrefix")
			}
			rig, bin := newProbeRigFixture(t)
			ctx := tt.arrange(t, bin)
			beadsPath := filepath.Join(rig, ".beads")
			before, err := snapshotProbeDir(beadsPath)
			if err != nil {
				t.Fatal(err)
			}
			runner := newExecRunner()
			if tt.pingTimeout > 0 {
				runner.bdPingTimeout = tt.pingTimeout
			}
			if tt.queueFull {
				for range maxConcurrent {
					runner.sem <- struct{}{}
				}
			}
			rep := newSamplerManager(Deps{}, runner).probeRig(ctx, "r1", rig)
			if rep.Rollup != "down" || !rep.Reachable {
				t.Fatalf("probeRig() = %+v, want reachable/down", rep)
			}
			if len(rep.Problems) != 1 {
				t.Fatalf("probeRig problems = %+v, want one typed problem", rep.Problems)
			}
			problem := rep.Problems[0]
			if problem.Category != "Beads" || problem.Name != pingConnectivityCheck || problem.Status != "error" {
				t.Fatalf("probeRig problem = %+v, want typed connectivity error", problem)
			}
			if tt.wantMessage != "" {
				if problem.Message != tt.wantMessage {
					t.Fatalf("probeRig problem message = %q, want %q", problem.Message, tt.wantMessage)
				}
			} else if !strings.HasPrefix(problem.Message, tt.wantMessagePrefix) {
				t.Fatalf("probeRig problem message = %q, want prefix %q", problem.Message, tt.wantMessagePrefix)
			}
			if strings.ContainsAny(problem.Message, "\x00\x1b") {
				t.Fatalf("probeRig problem message contains unsanitized control bytes: %q", problem.Message)
			}
			if rep.DoltConnected == nil || *rep.DoltConnected {
				t.Fatalf("probeRig DoltConnected = %v, want false", rep.DoltConnected)
			}
			after, err := snapshotProbeDir(beadsPath)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(before, after) {
				t.Fatalf("probe mutated .beads directory: before=%v after=%v", before, after)
			}
		})
	}
}

func TestProbeRigReportsPingFailureAsDown(t *testing.T) {
	rig, bin := newProbeRigFixture(t)
	writeFakeBd(t, bin, "#!/bin/sh\nprintf '%s' '{\"status\":\"error\",\"error\":\"proxy unavailable\"}'\nexit 1\n")

	rep := newSamplerManager(Deps{}, newExecRunner()).probeRig(context.Background(), "r1", rig)
	if rep.Rollup != "down" || !rep.Reachable || len(rep.Problems) != 1 || rep.Problems[0].Status != "error" {
		t.Fatalf("probeRig() = %+v, want reachable/down with one error", rep)
	}
	if !strings.Contains(rep.Problems[0].Message, "proxy unavailable") {
		t.Fatalf("probeRig problem = %+v, want provider error", rep.Problems[0])
	}
	if rep.DoltConnected == nil || *rep.DoltConnected {
		t.Fatalf("probeRig DoltConnected = %v, want non-nil false", rep.DoltConnected)
	}
}

func TestProbeRigReportsPingErrorEnvelopeAsDown(t *testing.T) {
	rig, bin := newProbeRigFixture(t)
	writeFakeBd(t, bin, "#!/bin/sh\nprintf '%s' '{\"error\":\"proxy unavailable\",\"schema_version\":1}'\nexit 1\n")

	rep := newSamplerManager(Deps{}, newExecRunner()).probeRig(context.Background(), "r1", rig)
	if rep.Rollup != "down" || !rep.Reachable || len(rep.Problems) != 1 {
		t.Fatalf("probeRig() = %+v, want reachable/down with one problem", rep)
	}
	if rep.Problems[0].Status != "error" || rep.Problems[0].Name != pingConnectivityCheck {
		t.Fatalf("probeRig problem = %+v, want an error connectivity check", rep.Problems[0])
	}
	if !strings.Contains(rep.Problems[0].Message, "proxy unavailable") {
		t.Fatalf("probeRig problem message = %q, want stdout provider error", rep.Problems[0].Message)
	}
	if rep.DoltConnected == nil || *rep.DoltConnected {
		t.Fatalf("probeRig DoltConnected = %v, want non-nil false", rep.DoltConnected)
	}
}

// TestProbeRigTruncatesPingErrorEnvelopeMessage pins the rune cap on the JSON
// error-envelope fold path. maxProbeErrorRunes exists so a chatty provider
// cannot bloat the snapshot the dashboard polls, and the envelope path is the
// one where the provider is healthy enough to be verbose — so it must truncate
// exactly like the stderr and note paths do.
func TestProbeRigTruncatesPingErrorEnvelopeMessage(t *testing.T) {
	rig, bin := newProbeRigFixture(t)
	long := strings.Repeat("A", maxProbeErrorRunes+128)
	writeFakeBd(t, bin, "#!/bin/sh\nprintf '%s' '{\"error\":\""+long+"\",\"schema_version\":1}'\nexit 1\n")

	rep := newSamplerManager(Deps{}, newExecRunner()).probeRig(context.Background(), "r1", rig)
	if len(rep.Problems) != 1 {
		t.Fatalf("probeRig() = %+v, want exactly one problem", rep)
	}
	got := rep.Problems[0].Message
	if n := len([]rune(got)); n != maxProbeErrorRunes {
		t.Fatalf("probeRig problem message rune count = %d, want the %d-rune cap", n, maxProbeErrorRunes)
	}
	if got != strings.Repeat("A", maxProbeErrorRunes) {
		t.Fatalf("probeRig problem message = %q, want the provider error truncated to the cap", got)
	}
}

// TestProbeRigReportsPingStderrFailureAsDown covers the failure shape ping
// actually produces when the store cannot be opened at all: plain text on
// stderr, empty stdout, non-zero exit. Nothing is parseable, so the probe must
// synthesize the connectivity check itself rather than reporting an empty
// "warn" and discarding the provider's error.
func TestProbeRigReportsPingStderrFailureAsDown(t *testing.T) {
	rig, bin := newProbeRigFixture(t)
	writeFakeBd(t, bin, "#!/bin/sh\nprintf '%s' 'failed to open store: connection refused' >&2\nexit 1\n")

	rep := newSamplerManager(Deps{}, newExecRunner()).probeRig(context.Background(), "r1", rig)
	if rep.Rollup != "down" || !rep.Reachable || len(rep.Problems) != 1 {
		t.Fatalf("probeRig() = %+v, want reachable/down with one problem", rep)
	}
	if rep.Problems[0].Status != "error" || rep.Problems[0].Name != pingConnectivityCheck {
		t.Fatalf("probeRig problem = %+v, want an error connectivity check", rep.Problems[0])
	}
	if !strings.Contains(rep.Problems[0].Message, "connection refused") {
		t.Fatalf("probeRig problem message = %q, want the stderr provider error", rep.Problems[0].Message)
	}
	if rep.DoltConnected == nil || *rep.DoltConnected {
		t.Fatalf("probeRig DoltConnected = %v, want non-nil false", rep.DoltConnected)
	}
}

// TestProbeRigReportsDoltConnectedFromPing pins the proxied case this change
// exists to serve: a rig with no dolt-server.port file has no endpoint to
// dial, so connectivity must come from the ping check itself instead of
// reporting "unknown" for a store whose connectivity was just proven.
func TestProbeRigReportsDoltConnectedFromPing(t *testing.T) {
	rig, bin := newProbeRigFixture(t)
	writeFakeBd(t, bin, "#!/bin/sh\nprintf '%s' '{\"status\":\"ok\"}'\n")

	rep := newSamplerManager(Deps{}, newExecRunner()).probeRig(context.Background(), "r1", rig)
	if rep.DoltEndpoint != nil {
		t.Fatalf("probeRig DoltEndpoint = %v, want nil without a dolt-server.port file", *rep.DoltEndpoint)
	}
	if rep.DoltConnected == nil || !*rep.DoltConnected {
		t.Fatalf("probeRig DoltConnected = %v, want non-nil true", rep.DoltConnected)
	}
	if rep.Rollup != "ok" || len(rep.Problems) != 0 || rep.Note != "" {
		t.Fatalf("probeRig() = %+v, want a clean ok report", rep)
	}
}

func TestProbeRigPingHealthOmitsLegacyIssueCount(t *testing.T) {
	rig, bin := newProbeRigFixture(t)
	writeFakeBd(t, bin, "#!/bin/sh\nprintf '%s' '{\"status\":\"ok\"}'\n")

	rep := newSamplerManager(Deps{}, newExecRunner()).probeRig(context.Background(), "r1", rig)
	wire, err := json.Marshal(rep)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(wire), `"issueCount"`) {
		t.Fatalf("probeRig wire payload = %s, contains removed issueCount field", wire)
	}
}

// TestProbeRigSkipsUnconfiguredStore pins the one case where the probe must
// not run bd at all. `bd ping` is read-only of the store but is a full
// provider open, so on a .beads directory with no beads configuration it
// CREATES an embedded store there and answers status=ok (verified on bd
// v1.3.0-rc.2); `bd doctor --readonly`, the probe this replaced, could not.
// Skip the ping for an unconfigured store rather than conjure one and report
// it healthy. A configured scope is still warmed by the probe, by design.
func TestProbeRigSkipsUnconfiguredStore(t *testing.T) {
	rig, bin := newProbeRigFixture(t)
	if err := os.Remove(filepath.Join(rig, ".beads", "embeddeddolt")); err != nil {
		t.Fatal(err)
	}
	// The fixture PATH holds only the fake bd, so the sentinel is written with a
	// shell redirect rather than an external command.
	sentinel := filepath.Join(t.TempDir(), "bd-was-invoked")
	writeFakeBd(t, bin, "#!/bin/sh\n: > \""+sentinel+"\"\nprintf '%s' '{\"status\":\"ok\"}'\n")

	rep := newSamplerManager(Deps{}, newExecRunner()).probeRig(context.Background(), "r1", rig)
	if _, err := os.Stat(sentinel); err == nil {
		t.Fatal("probeRig invoked bd against an unconfigured .beads store")
	}
	if rep.Rollup != "down" || !rep.Reachable {
		t.Fatalf("probeRig() = %+v, want reachable/down", rep)
	}
	if len(rep.Problems) != 1 || rep.Problems[0].Status != "error" || rep.Problems[0].Name != pingConnectivityCheck {
		t.Fatalf("probeRig problems = %+v, want one error connectivity check", rep.Problems)
	}
	if rep.DoltEndpoint != nil || rep.DoltConnected != nil {
		t.Fatalf("probeRig endpoint/connected = %v/%v, want both nil", rep.DoltEndpoint, rep.DoltConnected)
	}
	if entries, err := os.ReadDir(filepath.Join(rig, ".beads")); err != nil || len(entries) != 0 {
		t.Fatalf("ReadDir(.beads) = %v, %v; want an untouched empty store dir", entries, err)
	}
}

func TestProbeRigProxiedModeIgnoresStalePortArtifact(t *testing.T) {
	rig, bin := newProbeRigFixture(t)
	if err := os.WriteFile(filepath.Join(rig, ".beads", "metadata.json"), []byte(`{"dolt_mode":"proxied-server"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(rig, ".beads", "dolt-server.port"), []byte("1"), 0o644); err != nil {
		t.Fatal(err)
	}
	writeFakeBd(t, bin, "#!/bin/sh\nprintf '%s' '{\"status\":\"ok\"}'\n")

	rep := newSamplerManager(Deps{}, newExecRunner()).probeRig(context.Background(), "r1", rig)
	if rep.DoltEndpoint != nil {
		t.Fatalf("proxied stale endpoint = %v, want nil", *rep.DoltEndpoint)
	}
	if rep.DoltConnected == nil || !*rep.DoltConnected || rep.Rollup != "ok" {
		t.Fatalf("proxied stale-port report = %+v, want ping-backed healthy status", rep)
	}
}

func TestProbeRigConfigMarkerIgnoresStalePortArtifact(t *testing.T) {
	rig, bin := newProbeRigFixture(t)
	if err := os.WriteFile(filepath.Join(rig, ".beads", "config.yaml"), []byte("dolt.mode: proxied-server\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(rig, ".beads", "dolt-server.port"), []byte("1"), 0o644); err != nil {
		t.Fatal(err)
	}
	writeFakeBd(t, bin, "#!/bin/sh\nprintf '%s' '{\"status\":\"ok\"}'\n")
	rep := newSamplerManager(Deps{}, newExecRunner()).probeRig(context.Background(), "r1", rig)
	if rep.DoltEndpoint != nil || rep.DoltConnected == nil || !*rep.DoltConnected || rep.Rollup != "ok" {
		t.Fatalf("config-marker report = %+v, want ping-backed healthy status without endpoint", rep)
	}
}

func TestProbeRigMalformedMetadataFailsClosedOnStalePort(t *testing.T) {
	rig, bin := newProbeRigFixture(t)
	if err := os.WriteFile(filepath.Join(rig, ".beads", "metadata.json"), []byte("{not-json"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(rig, ".beads", "dolt-server.port"), []byte("1"), 0o644); err != nil {
		t.Fatal(err)
	}
	writeFakeBd(t, bin, "#!/bin/sh\nprintf '%s' '{\"status\":\"ok\"}'\n")
	rep := newSamplerManager(Deps{}, newExecRunner()).probeRig(context.Background(), "r1", rig)
	if rep.DoltEndpoint != nil {
		t.Fatalf("malformed metadata endpoint = %v, want nil", *rep.DoltEndpoint)
	}
	if rep.DoltConnected == nil || !*rep.DoltConnected || rep.Rollup != "ok" {
		t.Fatalf("malformed metadata report = %+v, want ping-backed healthy status", rep)
	}
}

func TestProbeRigUnsafeModeMarkersIgnoreStalePortArtifact(t *testing.T) {
	tests := []struct {
		name     string
		filename string
		contents string
	}{
		{name: "unknown metadata mode", filename: "metadata.json", contents: `{"dolt_mode":"mystery"}`},
		{name: "embedded metadata mode", filename: "metadata.json", contents: `{"dolt_mode":"embedded"}`},
		{name: "malformed config without mode", filename: "config.yaml", contents: "dolt: [\n"},
		{name: "missing mode", filename: "config.yaml", contents: "issue_prefix: tc\n"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rig, bin := newProbeRigFixture(t)
			if err := os.WriteFile(filepath.Join(rig, ".beads", tt.filename), []byte(tt.contents), 0o644); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(rig, ".beads", "dolt-server.port"), []byte("1"), 0o644); err != nil {
				t.Fatal(err)
			}
			writeFakeBd(t, bin, "#!/bin/sh\nprintf '%s' '{\"status\":\"ok\"}'\n")

			rep := newSamplerManager(Deps{}, newExecRunner()).probeRig(context.Background(), "r1", rig)
			if rep.DoltEndpoint != nil {
				t.Fatalf("unsafe marker endpoint = %v, want nil", *rep.DoltEndpoint)
			}
			if rep.DoltConnected == nil || !*rep.DoltConnected || rep.Rollup != "ok" {
				t.Fatalf("unsafe marker report = %+v, want ping-backed healthy status without endpoint", rep)
			}
		})
	}
}

func TestProbeRigOnlyPersistedServerModeAuthorizesTCPProbe(t *testing.T) {
	tests := []struct {
		name         string
		metadata     string
		metadataDir  bool
		config       string
		configDir    bool
		wantEndpoint bool
	}{
		{
			name:         "server metadata",
			metadata:     `{"dolt_mode":"server"}`,
			wantEndpoint: true,
		},
		{
			name:         "server config without metadata",
			config:       "dolt.mode: server\n",
			wantEndpoint: true,
		},
		{
			// bd >= 1.3.1 writes `bd config set dolt.mode` nested.
			name:         "nested server config without metadata",
			config:       "dolt:\n    mode: server\n",
			wantEndpoint: true,
		},
		{
			name:   "flat config mode wins over nested",
			config: "dolt:\n    mode: server\ndolt.mode: embedded\n",
		},
		{
			name:     "malformed metadata overrides server config",
			metadata: "{not-json",
			config:   "dolt.mode: server\n",
		},
		{
			name:     "missing metadata mode overrides server config",
			metadata: `{"backend":"dolt"}`,
			config:   "dolt.mode: server\n",
		},
		{
			name:        "unreadable metadata overrides server config",
			metadataDir: true,
			config:      "dolt.mode: server\n",
		},
		{
			name:      "unreadable config",
			configDir: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rig, bin := newProbeRigFixture(t)
			beadsPath := filepath.Join(rig, ".beads")
			if tt.metadataDir {
				if err := os.Mkdir(filepath.Join(beadsPath, "metadata.json"), 0o755); err != nil {
					t.Fatal(err)
				}
			} else if tt.metadata != "" {
				if err := os.WriteFile(filepath.Join(beadsPath, "metadata.json"), []byte(tt.metadata), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			if tt.configDir {
				if err := os.Mkdir(filepath.Join(beadsPath, "config.yaml"), 0o755); err != nil {
					t.Fatal(err)
				}
			} else if tt.config != "" {
				if err := os.WriteFile(filepath.Join(beadsPath, "config.yaml"), []byte(tt.config), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			if err := os.WriteFile(filepath.Join(beadsPath, "dolt-server.port"), []byte("1"), 0o644); err != nil {
				t.Fatal(err)
			}
			writeFakeBd(t, bin, "#!/bin/sh\nprintf '%s' '{\"status\":\"ok\"}'\n")

			rep := newSamplerManager(Deps{}, newExecRunner()).probeRig(context.Background(), "r1", rig)
			if (rep.DoltEndpoint != nil) != tt.wantEndpoint {
				t.Fatalf("DoltEndpoint = %v, want present=%t (report=%+v)", rep.DoltEndpoint, tt.wantEndpoint, rep)
			}
		})
	}
}

// newProbeRigFixture builds a rig directory with a .beads store carrying an
// inert store marker (an embedded-dolt data directory, which readDoltMode
// ignores) and a PATH containing only a fake bd, so a probeRig test never
// reaches a real one. The marker is what lets the probe get as far as bd; the
// unmarked case is its own test.
func newProbeRigFixture(t *testing.T) (rigPath, binDir string) {
	t.Helper()
	root := t.TempDir()
	rigPath = filepath.Join(root, "rig")
	if err := os.MkdirAll(filepath.Join(rigPath, ".beads", "embeddeddolt"), 0o755); err != nil {
		t.Fatal(err)
	}
	binDir = filepath.Join(root, "bin")
	if err := os.Mkdir(binDir, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir)
	t.Setenv("ADMIN_PATH", binDir)
	return rigPath, binDir
}

// writeFakeBd installs script as the bd on the fixture PATH.
func writeFakeBd(t *testing.T, binDir, script string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(binDir, "bd"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
}

// A proxied rig whose proxy retired on its finite idle timeout is healthy and
// is not pinged; a never-idle rig with no proxy is still pinged.
func TestProbeRigDoesNotPingAnIdleRetiredProxiedRig(t *testing.T) {
	for _, tc := range []struct {
		name, sidecar string
		wantPing      bool
	}{
		{name: "finite", sidecar: `{"idle_timeout":1800000000000}`},
		{name: "never", sidecar: `{"idle_timeout":-1}`, wantPing: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rig, bin := newProbeRigFixture(t)
			if err := os.WriteFile(filepath.Join(rig, ".beads", "metadata.json"), []byte(`{"dolt_mode":"proxied-server"}`), 0o644); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(rig, ".beads", "proxied_server_client_info.json"), []byte(tc.sidecar), 0o644); err != nil {
				t.Fatal(err)
			}
			marker := filepath.Join(t.TempDir(), "pinged")
			writeFakeBd(t, bin, "#!/bin/sh\n: > "+marker+"\nprintf '%s' '{\"status\":\"ok\"}'\n")

			rep := newSamplerManager(Deps{}, newExecRunner()).probeRig(context.Background(), "r1", rig)
			_, err := os.Stat(marker)
			if pinged := err == nil; pinged != tc.wantPing {
				t.Fatalf("pinged = %v, want %v (report %+v)", pinged, tc.wantPing, rep)
			}
			if rep.Rollup != "ok" {
				t.Fatalf("rollup = %q, want ok", rep.Rollup)
			}
			if !tc.wantPing && rep.Note != rigIdleNote {
				t.Fatalf("note = %q, want the idle note", rep.Note)
			}
		})
	}
}
