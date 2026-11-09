package exec

import (
	"errors"
	"os"
	osexec "os/exec"
	"path/filepath"
	goruntime "runtime"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/execenv"
	"github.com/gastownhall/gascity/internal/pidutil"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/runtime/proctable"
	"github.com/gastownhall/gascity/internal/runtime/runtimecontract/reference"
	"github.com/gastownhall/gascity/internal/runtime/runtimetest"
)

// scanPack is a golden reference pack for the scanner tests: hostFS selects
// the host-filesystem variant (locality local, fs.host), and bodies override
// its ops. Its `exec` runs nothing, so no test drives a host tmux. boxes is
// the city's box directory.
type scanPack struct {
	sp    *seamBackedProvider
	boxes string
}

func newScanPack(t *testing.T, hostFS bool, bodies map[string]string) scanPack {
	t.Helper()
	dir := t.TempDir()
	stateDir := filepath.Join(dir, "state")
	if err := os.Mkdir(stateDir, 0o755); err != nil {
		t.Fatal(err)
	}
	all := reference.Bodies()
	if hostFS {
		all = reference.HostFSBodies()
	}
	all["exec"] = `cat >/dev/null`
	for op, body := range bodies {
		all[op] = body
	}
	script := filepath.Join(dir, "golden")
	if err := os.WriteFile(script, []byte(reference.Script(stateDir, all)), 0o755); err != nil {
		t.Fatal(err)
	}
	cityPath := filepath.Join(dir, "city")
	return scanPack{
		sp:    seamBacked(NewProviderForCity(script, "scan-city", cityPath)),
		boxes: filepath.Join(stateDir, "city"+strings.ReplaceAll(cityPath, "/", "_")),
	}
}

// scanStartConfig is the config the scanner tests start a box with, its
// agent read alive by the golden reference.
func scanStartConfig(t *testing.T) runtime.Config {
	return runtime.Config{WorkDir: t.TempDir(), ProcessNames: []string{"sh", "sleep"}, AcceptStartupDialogs: new(bool)}
}

// Kills a scanner that is not conditional on the handshake: only a pack
// declaring fs.host with locality local runs its agents as processes of
// this host, so only it scans. fs.host alone, locality local alone (a
// container of this host), no declaration and a failed handshake read as no
// scanner, through the raw and the production seam-backed provider alike,
// and find nothing without reading the process table.
func TestProcessScanConditionalOnHandshake(t *testing.T) {
	const base = `"observe.v1","report-attachment","report-activity","conformance-faults","city-scoped","server-lifecycle"`
	for _, tc := range []struct {
		name     string
		protocol string // "" keeps the variant's own
		hostFS   bool
		want     bool
	}{
		{"fs.host with locality local", "", true, true},
		{"no declaration", "", false, false},
		{"fs.host without locality", `echo '{"version":0,"capabilities":[` + base + `,"fs.host"]}'`, true, false},
		{"locality local without fs.host", `echo '{"version":0,"capabilities":[` + base + `],"locality":"local"}'`, true, false},
		{"fs.host with locality remote", `echo '{"version":0,"capabilities":[` + base + `,"fs.host"],"locality":"remote"}'`, true, false},
		{"failed handshake", `echo "backend down" >&2; exit 1`, true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			bodies := map[string]string{}
			if tc.protocol != "" {
				bodies["protocol"] = tc.protocol
			}
			p := newScanPack(t, tc.hostFS, bodies)
			for label, sp := range map[string]runtime.Provider{"raw": p.sp.raw, "seam-backed": p.sp} {
				if _, ok := runtime.AsProcessTableScanner(sp); ok != tc.want {
					t.Errorf("%s: AsProcessTableScanner ok = %t, want %t", label, ok, tc.want)
				}
				if got := sp.(runtime.ConditionalProcessTableScanner).CanScanProcessTable(); got != tc.want {
					t.Errorf("%s: CanScanProcessTable = %t, want %t", label, got, tc.want)
				}
				if tc.want {
					continue
				}
				// Under go test a live scan is refused with an error, so a
				// scanner that read the process table would fail here.
				for _, id := range []string{"some-session", ""} {
					if found, err := sp.(runtime.ProcessTableScanner).FindRuntimesBySessionID(id); len(found) != 0 || err != nil {
						t.Errorf("%s: FindRuntimesBySessionID(%q) = %+v, %v; want nothing, without a scan", label, id, found, err)
					}
				}
			}
		})
	}
}

// TestExecProcessTableScannerContract runs RUNTIME-CONTRACT-024 against the
// production seam-backed exec provider over the host-filesystem golden
// reference, and its base variant as the pack whose boxes do not share the
// host.
func TestExecProcessTableScannerContract(t *testing.T) {
	runtimetest.RunProcessTableScannerContract(t, runtimetest.ProcessTableScannerFixtures{
		Scanning:    func(t *testing.T) runtime.Provider { return newScanPack(t, true, nil).sp },
		NotScanning: func(t *testing.T) runtime.Provider { return newScanPack(t, false, nil).sp },
		StartConfig: scanStartConfig,
	})
}

// startScanSession starts name on p with a command that leaves a process
// behind, carrying sessionID and name as its GC_SESSION_ID and
// GC_SESSION_NAME, and returns the leftover.
func startScanSession(t *testing.T, p scanPack, name, sessionID string) runtimetest.Leftover {
	t.Helper()
	if goruntime.GOOS != "linux" {
		t.Skip("the scanner tests read /proc snapshots, which only linux has")
	}
	command, wait := runtimetest.LeftoverCommand(t)
	cfg := scanStartConfig(t)
	cfg.Command = command
	cfg.Env = map[string]string{"GC_SESSION_ID": sessionID, "GC_SESSION_NAME": name}
	if err := p.sp.Start(t.Context(), name, cfg); err != nil {
		t.Fatalf("Start %s: %v", name, err)
	}
	t.Cleanup(func() { _ = p.sp.Stop(name) })
	return wait()
}

// scanOnly scans for id on a snapshot of the given leftovers alone.
func scanOnly(t *testing.T, sp runtime.Provider, id string, leftovers ...runtimetest.Leftover) ([]runtime.LiveRuntime, error) {
	t.Helper()
	pids := make([]int, 0, len(leftovers))
	for _, l := range leftovers {
		pids = append(pids, l.PID)
	}
	proctable.SetScanRootForTesting(t, runtimetest.SnapshotProcRoot(t, pids...))
	return sp.(runtime.ProcessTableScanner).FindRuntimesBySessionID(id)
}

// Kills tracking that reads a failed listing as "no box": a root the pack
// cannot list is LiveUnlisted, so a start refuses rather than reap it, and
// the failure is a whole-scan failure, so the sweep reaps only the roots of
// closed sessions.
func TestProcessScanFailedListingCannotRuleOutABox(t *testing.T) {
	p := newScanPack(t, true, nil)
	l := startScanSession(t, p, "live", "sid-live")
	t.Setenv("GC_RPP_FAULT", "unavailable")
	found, err := scanOnly(t, p.sp, "sid-live", l)
	if len(found) != 1 || found[0].PID != l.PID || found[0].IsTracked || !found[0].LiveUnlisted {
		t.Fatalf("found = %+v, want the leftover pid %d untracked and LiveUnlisted", found, l.PID)
	}
	if !proctable.HasWholeScanFailure(err) {
		t.Fatalf("err = %v, want a whole-scan failure (tracking unknown)", err)
	}
}

// Kills tracking that reads a box whose identity the pack cannot report as
// owning nothing, or everything: a root carrying the box's session name is
// that box's, a root naming another session is not, and a root naming none
// cannot be ruled out.
func TestProcessScanUnattributedBoxTracksByName(t *testing.T) {
	// The box reports no identity: observe and inventory answer an empty
	// identity, get-meta nothing.
	p := newScanPack(t, true, map[string]string{"get-meta": `:`})
	l := startScanSession(t, p, "live", "sid-live")
	for _, f := range []string{"meta.GC_SESSION_ID", "meta.GC_RUNTIME_EPOCH", "meta.GC_INSTANCE_TOKEN", "meta.GT_PROCESS_NAMES"} {
		if err := os.Remove(filepath.Join(p.boxes, "live", f)); err != nil && !errors.Is(err, os.ErrNotExist) {
			t.Fatal(err)
		}
	}
	found, err := scanOnly(t, p.sp, "sid-live", l)
	if err != nil || len(found) != 1 || !found[0].IsTracked || found[0].ProviderName != "live" {
		t.Fatalf("found = %+v, %v; want the leftover tracked as live by its session name", found, err)
	}

	other := startScanSession(t, newScanPack(t, true, nil), "elsewhere", "sid-elsewhere")
	found, err = scanOnly(t, p.sp, "sid-elsewhere", other)
	if err != nil || len(found) != 1 || found[0].IsTracked || found[0].LiveUnlisted {
		t.Fatalf("found = %+v, %v; want the other session's leftover untracked: it names a session this pack does not list", found, err)
	}
}

// Kills a TerminateRuntime that signals what it cannot bind to the scanned
// session: a pack that cannot scan, a root without a start identity or
// session id, and a pid that no longer carries the session (gone, so nil)
// are never signaled.
func TestProcessScanTerminateRefusals(t *testing.T) {
	if goruntime.GOOS != "linux" {
		t.Skip("reads /proc snapshots, which only linux has")
	}
	child := osexec.Command("sleep", "600")
	child.Env = append(execenv.Environ(), "GC_SESSION_ID=sid-mine")
	if err := child.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = child.Process.Kill()
		_ = child.Wait()
	})
	pid := child.Process.Pid
	identity, err := proctable.ProcessIdentity(pid)
	if err != nil {
		t.Fatal(err)
	}
	proctable.SetScanRootForTesting(t, runtimetest.SnapshotProcRoot(t, pid))

	host := newScanPack(t, true, nil).sp
	offHost := newScanPack(t, false, nil).sp
	for _, tc := range []struct {
		name    string
		sp      runtime.Provider
		live    runtime.LiveRuntime
		wantErr bool
	}{
		{"pack that cannot scan", offHost, runtime.LiveRuntime{SessionID: "sid-mine", PID: pid, StartIdentity: identity}, true},
		{"no start identity", host, runtime.LiveRuntime{SessionID: "sid-mine", PID: pid}, true},
		{"no session id", host, runtime.LiveRuntime{PID: pid, StartIdentity: identity}, true},
		{"init", host, runtime.LiveRuntime{SessionID: "sid-mine", PID: 1, StartIdentity: identity}, true},
		{"another session's pid", host, runtime.LiveRuntime{SessionID: "sid-other", PID: pid, StartIdentity: identity}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.sp.(runtime.ProcessTableScanner).TerminateRuntime(tc.live)
			if (err != nil) != tc.wantErr {
				t.Fatalf("TerminateRuntime(%+v) = %v, want error %t", tc.live, err, tc.wantErr)
			}
			if !pidutil.AliveWithStartTime(pid, identity) {
				t.Fatalf("TerminateRuntime(%+v) ended pid %d", tc.live, pid)
			}
		})
	}
}

// Kills an op that inherits the session identity of a gc run in an agent's
// shell: a server such an op starts and leaves running would carry that
// session's GC_SESSION_ID, read to the scanner as its leftover, and be
// ended by the orphan sweep with the session, or carry its
// GC_INSTANCE_TOKEN into a pack's own identity scans. Every op, of a
// provider built for a city or not, runs without any key of
// execenv.SessionScopedEnvKeys, the list gc's other detached processes are
// started without, and keeps the rest of gc's environment.
func TestOpsRunWithoutTheCallersSessionIdentity(t *testing.T) {
	for _, k := range execenv.SessionScopedEnvKeys {
		t.Setenv(k, "from-the-caller")
	}
	t.Setenv("GC_SCAN_KEEP", "kept")
	for _, tc := range []struct {
		name string
		new  func(script string) *Provider
	}{
		{"city provider", func(script string) *Provider { return NewProviderForCity(script, "c", "/c") }},
		{"provider", func(script string) *Provider { return NewProvider(script) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			seen := filepath.Join(dir, "env")
			script := writeScript(t, dir, `{ echo "op=$1"; env; } >> '`+seen+`'`+"\n")
			p := tc.new(script)
			_ = p.IsRunning("box")
			if _, err := p.GetMeta("box", "k"); err != nil {
				t.Fatal(err)
			}
			data, err := os.ReadFile(seen)
			if err != nil {
				t.Fatal(err)
			}
			lines := strings.Split(string(data), "\n")
			if n := strings.Count(string(data), "GC_SCAN_KEEP=kept"); n != 2 {
				t.Fatalf("ops saw gc's GC_SCAN_KEEP %d times, want once per op (2):\n%s", n, data)
			}
			for _, k := range execenv.SessionScopedEnvKeys {
				for _, line := range lines {
					if strings.HasPrefix(line, k+"=") {
						t.Errorf("an op saw the caller's %s", line)
					}
				}
			}
		})
	}
}

// Kills a GC_SESSION_NAME read failure counted as one unreadable entry:
// with a listed box whose identity reads empty, a root whose session name
// cannot be read may be that box's, so tracking is unknown (a whole-scan
// failure) and the root is LiveUnlisted, as for a root naming no session.
func TestProcessScanUnreadableSessionNameIsUndecided(t *testing.T) {
	p := newScanPack(t, true, map[string]string{"get-meta": `:`})
	l := startScanSession(t, p, "live", "sid-live")
	for _, f := range []string{"meta.GC_SESSION_ID", "meta.GC_RUNTIME_EPOCH", "meta.GC_INSTANCE_TOKEN", "meta.GT_PROCESS_NAMES"} {
		if err := os.Remove(filepath.Join(p.boxes, "live", f)); err != nil && !errors.Is(err, os.ErrNotExist) {
			t.Fatal(err)
		}
	}
	other := startScanSession(t, newScanPack(t, true, nil), "elsewhere", "sid-elsewhere")
	prev := processEnvValue
	processEnvValue = func(pid int, key string) (string, error) {
		if key == "GC_SESSION_NAME" {
			return "", errors.New("environ unreadable")
		}
		return prev(pid, key)
	}
	t.Cleanup(func() { processEnvValue = prev })
	found, err := scanOnly(t, p.sp, "sid-elsewhere", l, other)
	if len(found) != 1 || found[0].PID != other.PID || found[0].IsTracked || !found[0].LiveUnlisted {
		t.Fatalf("found = %+v, want the root pid %d untracked and LiveUnlisted", found, other.PID)
	}
	if !proctable.HasWholeScanFailure(err) {
		t.Fatalf("err = %v, want a whole-scan failure (tracking unknown)", err)
	}
}

// Kills an unbounded tracking listing: the orphan sweep scans every tick,
// so a listing slower than the scan's bound fails the tracking (a
// whole-scan failure, the root LiveUnlisted) instead of holding the tick
// for the whole op timeout.
func TestProcessScanListingIsBounded(t *testing.T) {
	for _, v2 := range []bool{true, false} {
		t.Run(map[bool]string{true: "inventory", false: "list-running"}[v2], func(t *testing.T) {
			bodies := map[string]string{"inventory": `sleep 10; exit 1`}
			if !v2 {
				bodies = map[string]string{
					"protocol":     `echo '{"version":0,"capabilities":["fs.host"],"locality":"local"}'`,
					"list-running": `sleep 10`,
				}
			}
			p := newScanPack(t, true, bodies)
			p.sp.raw.scanListingTimeout = 200 * time.Millisecond
			if p.sp.raw.v2Ready() != v2 {
				t.Fatalf("v2Ready = %t, want %t", !v2, v2)
			}
			l := startScanSession(t, newScanPack(t, true, nil), "live", "sid-live")
			start := time.Now()
			found, err := scanOnly(t, p.sp, "sid-live", l)
			if elapsed := time.Since(start); elapsed > 5*time.Second {
				t.Fatalf("the scan took %s, want it bounded near 200ms", elapsed)
			}
			if len(found) != 1 || found[0].IsTracked || !found[0].LiveUnlisted || !proctable.HasWholeScanFailure(err) {
				t.Fatalf("found = %+v, %v; want the root LiveUnlisted under a whole-scan failure", found, err)
			}
		})
	}
}
