package exec

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/runtime/runtimecontract/reference"
	"github.com/gastownhall/gascity/internal/runtime/runtimetest"
)

// Handshakes the host-staging fixtures declare.
const (
	hostFSHandshake        = `{"version":0,"capabilities":["fs.host"],"locality":"local"}`
	hostFSNoLocality       = `{"version":0,"capabilities":["fs.host"]}`
	localWithoutFSHost     = `{"version":0,"capabilities":[],"locality":"local"}`
	hostFSSeparableLaunch  = `{"version":0,"capabilities":["fs.host","proc.provision","proc.exec"],"locality":"local"}`
	hostFSHandshakeRemote  = `{"version":0,"capabilities":["fs.host"],"locality":"remote"}`
	hostStageWorkDirMarker = "gc-prestart-ran"
	hostStageSession       = "s1"
)

// hostStageFixture is a pack that records the start config each `start` or
// `provision` op received ($S/<op>.<name>.json) and whether, when it ran,
// the work dir already held the pre_start marker and the session_setup
// marker. A session is running while $S/<name>.running exists, and
// `is-running` fails while $S/is-running.fails exists. Its `exec` answers
// `tmux has-session` with 1 (no agent session yet) and every other command
// with 0, so a separable launch succeeds without a tmux.
type hostStageFixture struct {
	script  string
	state   string
	workDir string
}

func newHostStageFixture(t *testing.T, handshake string) hostStageFixture {
	t.Helper()
	dir := t.TempDir()
	state := filepath.Join(dir, "state")
	workDir := filepath.Join(dir, "work")
	for _, d := range []string{state, workDir} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	body := `S='` + state + `'
W='` + workDir + `'
op="$1"
name="$2"
case "$op" in
  protocol) echo '` + handshake + `' ;;
  start|provision)
    cat > "$S/$op.$name.json"
    if [ -f "$W/` + hostStageWorkDirMarker + `" ]; then : > "$S/$op.$name.saw-prestart"; fi
    if [ -f "$W/gc-setup-ran" ]; then : > "$S/$op.$name.saw-setup"; fi
    : > "$S/$name.running" ;;
  stop) rm -f "$S/$name.running"; echo "$name" >> "$S/stops" ;;
  is-running)
    if [ -f "$S/is-running.fails" ]; then exit 1; fi
    if [ -f "$S/$name.running" ]; then echo true; else echo false; fi ;;
  exec) cmd=$(cat); case "$cmd" in *has-session*) exit 1 ;; *) exit 0 ;; esac ;;
  *) exit 2 ;;
esac
`
	return hostStageFixture{script: writeScript(t, dir, body), state: state, workDir: workDir}
}

// received returns the top-level field names of the config op received for
// hostStageSession, or nil when the op never ran.
func (f hostStageFixture) received(t *testing.T, op string) []string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(f.state, op+"."+hostStageSession+".json"))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		t.Fatalf("decoding the %s config: %v", op, err)
	}
	names := make([]string, 0, len(fields))
	for k := range fields {
		names = append(names, k)
	}
	sort.Strings(names)
	return names
}

func (f hostStageFixture) exists(name string) bool {
	_, err := os.Stat(filepath.Join(f.state, name))
	return err == nil
}

// hostWorkConfig is a start config carrying every kind of host work gc
// does for a pack sharing the host filesystem: a provider-aware pack
// overlay, an agent overlay, a copied file, pre_start, session_setup and
// session_live.
func (f hostStageFixture) hostWorkConfig(t *testing.T) runtime.Config {
	t.Helper()
	src := t.TempDir()
	packOverlay := filepath.Join(src, "pack-overlay")
	agentOverlay := filepath.Join(src, "agent-overlay")
	for path, body := range map[string]string{
		filepath.Join(packOverlay, "per-provider", "claude", "claude-only"): "claude",
		filepath.Join(packOverlay, "per-provider", "codex", "codex-only"):   "codex",
		filepath.Join(packOverlay, "shared"):                                "shared",
		filepath.Join(agentOverlay, "agent-file"):                           "agent",
		filepath.Join(src, "copied"):                                        "copied",
	} {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return runtime.Config{
		WorkDir:              f.workDir,
		Command:              "sleep 300",
		ProviderName:         "claude",
		Env:                  map[string]string{"GC_DIR": f.workDir},
		PackOverlayDirs:      []string{packOverlay},
		OverlayDir:           agentOverlay,
		CopyFiles:            []runtime.CopyEntry{{Src: filepath.Join(src, "copied"), RelDst: "staged/copied"}},
		PreStart:             []string{"touch " + filepath.Join(f.workDir, hostStageWorkDirMarker)},
		SessionSetup:         []string{`printf %s "$GC_SESSION" > ` + filepath.Join(f.workDir, "gc-setup-ran")},
		SessionSetupScript:   "touch " + filepath.Join(f.workDir, "gc-setup-script-ran"),
		SessionLive:          []string{"touch " + filepath.Join(f.workDir, "gc-live-ran")},
		AcceptStartupDialogs: new(bool),
	}
}

func fileIn(t *testing.T, dir, rel string) (string, bool) {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(dir, rel))
	if errors.Is(err, os.ErrNotExist) {
		return "", false
	}
	if err != nil {
		t.Fatal(err)
	}
	return string(data), true
}

// handledStartFields are the start-config fields gc leaves out when it did
// their work on the host.
var handledStartFields = []string{"copy_files", "overlay_dir", "pack_overlay_dirs", "pre_start", "session_setup", "session_setup_script"}

// TestHostStagedStartOmitsHandledFields pins fs.host on the welded start:
// gc stages the provider-aware overlays and copied files, runs pre_start on
// the host before the start op, runs session_setup after it, leaves out of
// the start config exactly the work it did, and runs no session_live.
func TestHostStagedStartOmitsHandledFields(t *testing.T) {
	f := newHostStageFixture(t, hostFSHandshake)
	p := NewProviderForCity(f.script, "city", t.TempDir())
	cfg := f.hostWorkConfig(t)
	if err := p.Start(context.Background(), hostStageSession, cfg); err != nil {
		t.Fatalf("Start: %v", err)
	}

	got := f.received(t, "start")
	for _, field := range handledStartFields {
		if slices.Contains(got, field) {
			t.Errorf("start config carries %q, which gc already handled on the host; fields = %v", field, got)
		}
	}
	for _, field := range []string{"work_dir", "command", "env", "session_live", "work_dir_exclude"} {
		if !slices.Contains(got, field) {
			t.Errorf("start config lacks %q, which gc did not handle; fields = %v", field, got)
		}
	}

	if body, ok := fileIn(t, f.workDir, "claude-only"); !ok || body != "claude" {
		t.Errorf("the provider's per-provider overlay was not staged (claude-only = %q, %v)", body, ok)
	}
	if _, ok := fileIn(t, f.workDir, "codex-only"); ok {
		t.Error("another provider's per-provider overlay was staged")
	}
	for _, rel := range []string{"shared", "agent-file", "staged/copied"} {
		if _, ok := fileIn(t, f.workDir, rel); !ok {
			t.Errorf("%s was not staged into the work dir", rel)
		}
	}
	if !f.exists("start.s1.saw-prestart") {
		t.Error("pre_start had not run on the host when the start op ran")
	}
	if f.exists("start.s1.saw-setup") {
		t.Error("session_setup ran before the start op")
	}
	if body, ok := fileIn(t, f.workDir, "gc-setup-ran"); !ok || body != hostStageSession {
		t.Errorf("session_setup did not run on the host with GC_SESSION (marker = %q, %v)", body, ok)
	}
	if _, ok := fileIn(t, f.workDir, "gc-setup-script-ran"); !ok {
		t.Error("session_setup_script did not run on the host")
	}
	if _, ok := fileIn(t, f.workDir, "gc-live-ran"); ok {
		t.Error("session_live ran; gc does not run it for an exec runtime")
	}
}

// TestFSHostWithoutLocalityIsUndeclared pins the narrowing: fs.host counts
// only with locality=local, and locality=local alone is not fs.host. Either
// way gc does no host work and sends the full start config, as before.
func TestFSHostWithoutLocalityIsUndeclared(t *testing.T) {
	for name, handshake := range map[string]string{
		"fs.host without locality": hostFSNoLocality,
		"fs.host, remote":          hostFSHandshakeRemote,
		"local without fs.host":    localWithoutFSHost,
	} {
		t.Run(name, func(t *testing.T) {
			f := newHostStageFixture(t, handshake)
			p := NewProviderForCity(f.script, "city", t.TempDir())
			if p.LocalTranscripts() || seamBacked(p).LocalTranscripts() {
				t.Error("LocalTranscripts = true without locality=local and fs.host")
			}
			if got := HostCapabilities(mustProtocol(t, p)); got != (runtime.HostCapabilities{}) {
				t.Errorf("HostCapabilities = %+v, want none", got)
			}
			cfg := f.hostWorkConfig(t)
			if err := p.Start(context.Background(), hostStageSession, cfg); err != nil {
				t.Fatalf("Start: %v", err)
			}
			got := f.received(t, "start")
			for _, field := range handledStartFields {
				if !slices.Contains(got, field) {
					t.Errorf("start config lacks %q, but gc did no host work; fields = %v", field, got)
				}
			}
			for _, rel := range []string{hostStageWorkDirMarker, "claude-only", "agent-file", "gc-setup-ran"} {
				if _, ok := fileIn(t, f.workDir, rel); ok {
					t.Errorf("gc did host work (%s) for a pack that does not share the host filesystem", rel)
				}
			}
		})
	}
}

func mustProtocol(t *testing.T, p *Provider) runtime.ProtocolInfo {
	t.Helper()
	info, err := p.Protocol()
	if err != nil {
		t.Fatalf("Protocol: %v", err)
	}
	return info
}

// TestHostPreStartFailureIsFatal pins a failing host pre_start: Start fails
// with the command's redacted output, the start op never runs, and nothing
// is left behind to stop.
func TestHostPreStartFailureIsFatal(t *testing.T) {
	f := newHostStageFixture(t, hostFSHandshake)
	p := NewProviderForCity(f.script, "city", t.TempDir())
	const secret = "sk-ant-gc-test-secret-value-0001"
	cfg := runtime.Config{
		WorkDir:              f.workDir,
		Command:              "sleep 300",
		Env:                  map[string]string{"ANTHROPIC_API_KEY": secret},
		PreStart:             []string{"true", `echo "failing with $ANTHROPIC_API_KEY"; exit 3`},
		AcceptStartupDialogs: new(bool),
	}
	err := p.Start(context.Background(), hostStageSession, cfg)
	if err == nil {
		t.Fatal("Start with a failing pre_start = nil, want an error")
	}
	if !strings.Contains(err.Error(), "pre_start[1]") {
		t.Errorf("error %q does not name the failing pre_start entry", err)
	}
	if strings.Contains(err.Error(), secret) || !strings.Contains(err.Error(), runtime.RedactedValue) {
		t.Errorf("error %q is not the redacted output tail", err)
	}
	if got := f.received(t, "start"); got != nil {
		t.Errorf("the start op ran after a fatal pre_start; it received %v", got)
	}
	if f.exists("stops") || p.IsRunning(hostStageSession) {
		t.Error("a fatal pre_start left a box or stopped one")
	}
}

// TestHostPreStartHonorsSetupTimeouts pins the [session] setup budget: a
// pre_start that outlives setup_timeout fails the start with the deadline.
func TestHostPreStartHonorsSetupTimeouts(t *testing.T) {
	f := newHostStageFixture(t, hostFSHandshake)
	p := NewProviderForCity(f.script, "city", t.TempDir(), WithSetupTimeouts(300*time.Millisecond, 0))
	cfg := runtime.Config{WorkDir: f.workDir, Command: "sleep 300", PreStart: []string{"sleep 10"}, AcceptStartupDialogs: new(bool)}
	began := time.Now()
	err := p.Start(context.Background(), hostStageSession, cfg)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Start = %v, want the setup deadline", err)
	}
	if took := time.Since(began); took > 5*time.Second {
		t.Errorf("Start took %s, past the 300ms setup timeout plus the kill grace", took)
	}
}

// TestHostSessionSetupFailureIsNonFatal pins session_setup: a failing
// command is a warning, the rest still run, and the start succeeds.
func TestHostSessionSetupFailureIsNonFatal(t *testing.T) {
	f := newHostStageFixture(t, hostFSHandshake)
	p := NewProviderForCity(f.script, "city", t.TempDir())
	var warnings bytes.Buffer
	p.setupWarnings = &warnings
	cfg := runtime.Config{
		WorkDir:              f.workDir,
		Command:              "sleep 300",
		SessionSetup:         []string{"exit 4", "touch " + filepath.Join(f.workDir, "gc-setup-ran")},
		AcceptStartupDialogs: new(bool),
	}
	if err := p.Start(context.Background(), hostStageSession, cfg); err != nil {
		t.Fatalf("Start with a failing session_setup = %v, want nil", err)
	}
	if _, ok := fileIn(t, f.workDir, "gc-setup-ran"); !ok {
		t.Error("the session_setup command after the failing one did not run")
	}
	if !strings.Contains(warnings.String(), "session_setup[0]") {
		t.Errorf("warnings = %q, want one naming session_setup[0]", warnings.String())
	}
	if !p.IsRunning(hostStageSession) {
		t.Error("a failing session_setup tore the session down")
	}
}

// TestHostStagingNeedsAReportedVacancy pins the start into a name the pack
// does not report vacant, on both create paths of the production
// seam-backed provider (the welded start, and the separable provision then
// launch): a name a box already holds, and one whose `is-running` read
// fails. gc does no host work there, which would re-run pre_start in a live
// session's work dir: it stages nothing, runs no pre_start or session_setup
// on the host, and sends the full config, so the pack does what it carries.
func TestHostStagingNeedsAReportedVacancy(t *testing.T) {
	for _, tc := range []struct {
		name, handshake, op string
		occupancy           string // the state file that makes the read occupied or failed
	}{
		{"welded, occupied", hostFSHandshake, "start", hostStageSession + ".running"},
		{"welded, unknown", hostFSHandshake, "start", "is-running.fails"},
		{"separable, occupied", hostFSSeparableLaunch, "provision", hostStageSession + ".running"},
		{"separable, unknown", hostFSSeparableLaunch, "provision", "is-running.fails"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newHostStageFixture(t, tc.handshake)
			if err := os.WriteFile(filepath.Join(f.state, tc.occupancy), nil, 0o644); err != nil {
				t.Fatal(err)
			}
			sp := NewSeamBackedForCity(f.script, "city", t.TempDir())
			if err := sp.Start(context.Background(), hostStageSession, f.hostWorkConfig(t)); err != nil {
				t.Fatalf("Start: %v", err)
			}
			got := f.received(t, tc.op)
			if got == nil {
				t.Fatalf("the %s op never ran", tc.op)
			}
			for _, field := range handledStartFields {
				if !slices.Contains(got, field) {
					t.Errorf("%s config lacks %q, but gc may do no host work into this name; fields = %v", tc.op, field, got)
				}
			}
			for _, rel := range []string{hostStageWorkDirMarker, "claude-only", "shared", "agent-file", "staged/copied", "gc-setup-ran", "gc-setup-script-ran"} {
				if _, ok := fileIn(t, f.workDir, rel); ok {
					t.Errorf("gc did host work (%s) into a name the pack did not report vacant", rel)
				}
			}
		})
	}
}

// TestHostStagedSeparableLaunch pins fs.host on the un-welded path: gc does
// the host work before `provision`, leaves it out of the provision config,
// and runs session_setup after the agent launch.
func TestHostStagedSeparableLaunch(t *testing.T) {
	f := newHostStageFixture(t, hostFSSeparableLaunch)
	sp := seamBacked(NewProviderForCity(f.script, "city", t.TempDir()))
	cfg := f.hostWorkConfig(t)
	if err := sp.Start(context.Background(), hostStageSession, cfg); err != nil {
		t.Fatalf("Start: %v", err)
	}
	got := f.received(t, "provision")
	if got == nil {
		t.Fatal("the provision op never ran")
	}
	for _, field := range handledStartFields {
		if slices.Contains(got, field) {
			t.Errorf("provision config carries %q, which gc already handled; fields = %v", field, got)
		}
	}
	if !f.exists("provision.s1.saw-prestart") || f.exists("provision.s1.saw-setup") {
		t.Error("pre_start must run before provision, and session_setup after the launch")
	}
	if _, ok := fileIn(t, f.workDir, "gc-setup-ran"); !ok {
		t.Error("session_setup did not run after the launch")
	}
}

// TestLocalTranscriptsFollowSharesHostFS pins LocalTranscriptProvider: it
// is present on every exec provider and answers SharesHostFS.
func TestLocalTranscriptsFollowSharesHostFS(t *testing.T) {
	var _ runtime.LocalTranscriptProvider = (*Provider)(nil)
	var _ runtime.LocalTranscriptProvider = (*seamBackedProvider)(nil)
	local := NewProviderForCity(newHostStageFixture(t, hostFSHandshake).script, "city", t.TempDir())
	if !local.LocalTranscripts() || !seamBacked(local).LocalTranscripts() {
		t.Error("LocalTranscripts = false under locality=local and fs.host")
	}
	if !runtime.LocalTranscriptsFor(seamBacked(local), hostStageSession) {
		t.Error("runtime.LocalTranscriptsFor = false for an fs.host exec session")
	}
	remote := NewProviderForCity(newHostStageFixture(t, localWithoutFSHost).script, "city", t.TempDir())
	if remote.LocalTranscripts() || runtime.LocalTranscriptsFor(seamBacked(remote), hostStageSession) {
		t.Error("LocalTranscripts = true without fs.host")
	}
	failing := NewProviderForCity(writeScript(t, t.TempDir(), `[ "$1" = protocol ] && exit 1; exit 2`), "city", t.TempDir())
	if failing.LocalTranscripts() {
		t.Error("LocalTranscripts = true with a failed handshake")
	}
}

// TestHostCapabilitiesFollowSharesHostFS pins what an exec runtime reports
// to the skill and MCP gates: both host capabilities exactly when the
// handshake shares the host filesystem (gc then runs pre_start there).
func TestHostCapabilitiesFollowSharesHostFS(t *testing.T) {
	both := runtime.HostCapabilities{SharesHostFilesystem: true, RunsHostPreStart: true}
	for _, tc := range []struct {
		info runtime.ProtocolInfo
		want runtime.HostCapabilities
	}{
		{runtime.ProtocolInfo{Locality: "local", Capabilities: []string{"fs.host"}}, both},
		{runtime.ProtocolInfo{Capabilities: []string{"fs.host"}}, runtime.HostCapabilities{}},
		{runtime.ProtocolInfo{Locality: "local"}, runtime.HostCapabilities{}},
	} {
		if got := HostCapabilities(tc.info); got != tc.want {
			t.Errorf("HostCapabilities(%+v) = %+v, want %+v", tc.info, got, tc.want)
		}
	}
}

// hostFSGoldenFixture is the conformance golden reference in its
// host-filesystem variant (locality local, fs.host), with an `exec` that
// runs nothing, so no test drives a host tmux. received reads the start
// config a box kept (start.json).
func hostFSGoldenFixture(t *testing.T) (*seamBackedProvider, func(name string) []string) {
	t.Helper()
	dir := t.TempDir()
	stateDir := filepath.Join(dir, "state")
	if err := os.Mkdir(stateDir, 0o755); err != nil {
		t.Fatal(err)
	}
	bodies := reference.HostFSBodies()
	bodies["exec"] = `cat >/dev/null`
	script := filepath.Join(dir, "golden")
	if err := os.WriteFile(script, []byte(reference.Script(stateDir, bodies)), 0o755); err != nil {
		t.Fatal(err)
	}
	cityPath := filepath.Join(dir, "city")
	sp := seamBacked(NewProviderForCity(script, "host-fs", cityPath))
	if !sp.raw.v2Ready() || !sp.raw.sharesHostFS() {
		t.Fatal("the host-filesystem golden reference is not v2-ready and fs.host through the exec provider")
	}
	boxes := filepath.Join(stateDir, "city"+strings.ReplaceAll(cityPath, "/", "_"))
	received := func(name string) []string {
		data, err := os.ReadFile(filepath.Join(boxes, name, "start.json"))
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		if err != nil {
			t.Fatal(err)
		}
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(data, &fields); err != nil {
			t.Fatalf("decoding %s's start config: %v", name, err)
		}
		names := make([]string, 0, len(fields))
		for k := range fields {
			names = append(names, k)
		}
		sort.Strings(names)
		return names
	}
	return sp, received
}

// TestExecHostStagingContract runs RUNTIME-CONTRACT-022 against the
// production seam-backed exec provider over the host-filesystem golden
// reference.
func TestExecHostStagingContract(t *testing.T) {
	runtimetest.RunHostStagingContract(t, runtimetest.HostStagingFixture{
		New: func(t *testing.T) (runtime.Provider, func(string) []string) {
			return hostFSGoldenFixture(t)
		},
	})
}

// TestHostStagedFreshStart pins fs.host on a v2 fresh-only start, which
// creates the box through its own path (createFresh): the host work comes
// first and is left out of the start config.
func TestHostStagedFreshStart(t *testing.T) {
	sp, received := hostFSGoldenFixture(t)
	f := hostStageFixture{workDir: t.TempDir()}
	cfg := f.hostWorkConfig(t)
	cfg.FreshOnly = true
	if err := sp.Start(context.Background(), hostStageSession, cfg); err != nil {
		t.Fatalf("fresh Start: %v", err)
	}
	t.Cleanup(func() { _ = sp.Stop(hostStageSession) })
	got := received(hostStageSession)
	if !slices.Contains(got, "fresh_only") {
		t.Fatalf("the start config %v is not a fresh-only start's", got)
	}
	for _, field := range handledStartFields {
		if slices.Contains(got, field) {
			t.Errorf("fresh start config carries %q, which gc already handled; fields = %v", field, got)
		}
	}
	for _, rel := range []string{hostStageWorkDirMarker, "claude-only", "gc-setup-ran"} {
		if _, ok := fileIn(t, f.workDir, rel); !ok {
			t.Errorf("%s is missing: the fresh start did not do the host work", rel)
		}
	}
}
