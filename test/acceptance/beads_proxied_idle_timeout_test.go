//go:build acceptance_a

// Proxied idle timeout acceptance: a gc-owned proxied scope initialized with a
// finite idle timeout retires its proxy and Dolt child after that much quiet,
// and the next bd command restarts them transparently, including one issued
// while the dying proxy is still running its shutdown GC. Doctor on the
// stopped city reports the configured value as matching and starts nothing,
// and gc stop on a reaped scope is a clean no-op.
package acceptance_test

import (
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads/proxyendpoint"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/doctor"
	helpers "github.com/gastownhall/gascity/test/acceptance/helpers"
)

// idleTimeoutUnderTest is short so the test finishes in about a minute; it is
// below config's 1m floor, which only the environment override may cross. bd's
// idle watcher samples every quarter of it, floored at 1s, so 5s still leaves
// the watcher several samples per window. No controller runs here, so nothing
// but the test's own commands can hold a connection open.
const idleTimeoutUnderTest = 5 * time.Second

// idleRetireWait bounds one idle retirement: bd's sampled watcher exits T to
// 1.5T after the last connection it saw, and then runs a shutdown GC before it
// removes proxy.pid.
const idleRetireWait = idleTimeoutUnderTest*3/2 + 45*time.Second

func TestProxiedIdleTimeoutReapAndTransparentRestart(t *testing.T) {
	// Several idle retirements of a real proxy and Dolt child take minutes:
	// not Tier A smoke material. Bazel's acceptance lane opts in and runs it
	// as a target of its own (test/acceptance/BUILD.bazel).
	helpers.RequireTopologyMatrix(t)
	bdPath, doltPath := requireProxiedTooling(t)
	env := proxiedEnv(t, bdPath, doltPath).With(config.ProxiedIdleTimeoutEnv, idleTimeoutUnderTest.String())

	city := helpers.NewCity(t, env)
	cityRoot := city.Dir
	rigDir := createGitRig(t)
	t.Cleanup(func() {
		// The city's own cleanup (helpers.City) has stopped it by now.
		stopIfPairsRemain(t, env, cityRoot, rigDir)
		for _, root := range []string{cityRoot, rigDir} {
			if leaked := waitForNoDoltProcesses(t, root, 20*time.Second); len(leaked) > 0 {
				t.Errorf("processes under %s outlived the test:\n%s", root, strings.Join(leaked, "\n"))
			}
		}
	})

	// No controller: nothing but this test may touch the scopes, so the only
	// thing that can retire a proxy is its own idle watcher.
	city.InitNoStart("claude")
	if out, err := city.GC("rig", "add", rigDir); err != nil {
		t.Fatalf("gc rig add: %v\n%s", err, out)
	}
	const rigName = "testrig"

	want := int(idleTimeoutUnderTest)
	for label, scope := range map[string]string{"city": cityRoot, "rig": rigDir} {
		var sidecar proxiedSidecar
		readJSONFile(t, filepath.Join(scope, ".beads", "proxied_server_client_info.json"), &sidecar)
		if sidecar.IdleTimeout != want {
			t.Fatalf("%s sidecar idle_timeout = %d, want %d", label, sidecar.IdleTimeout, want)
		}
	}

	if out, err := city.GC("bd", "--rig", rigName, "list", "--json"); err != nil {
		t.Fatalf("gc bd --rig %s list: %v\n%s", rigName, err, out)
	}
	proxyRoot := proxiedScopeProxyRoot(t, rigDir)
	first, err := proxyendpoint.Read(proxyRoot)
	if err != nil {
		t.Fatalf("read the rig's proxy record: %v", err)
	}
	argv, err := proxyendpoint.DefaultProcessTable().Argv(first.PID)
	if err != nil {
		t.Fatalf("read the rig proxy's argv: %v", err)
	}
	if policy := proxyendpoint.ArgvIdlePolicy(argv); policy.Kind != proxyendpoint.IdleFinite || policy.Timeout != idleTimeoutUnderTest {
		t.Fatalf("rig proxy runs with %s (argv %q), want finite(%s)", policy, argv, idleTimeoutUnderTest)
	}

	t.Run("reaped-after-the-idle-timeout", func(t *testing.T) {
		start := time.Now()
		waitForProxyRecordGone(t, proxyRoot, idleRetireWait)
		t.Logf("rig proxy retired %s after the last command", time.Since(start).Round(time.Second))
		if leaked := waitForNoDoltProcesses(t, rigDir, 15*time.Second); len(leaked) > 0 {
			t.Fatalf("the rig's proxy record is gone but its processes are not:\n%s", strings.Join(leaked, "\n"))
		}
		if n := countIdleExits(t, rigDir); n == 0 {
			t.Fatalf("no %q line in the rig's proxy.log", "idleWatcher expired after "+idleTimeoutUnderTest.String())
		}
	})

	t.Run("next-command-restarts-transparently", func(t *testing.T) {
		out, err := city.GCStdout("bd", "--rig", rigName, "create", "after the idle reap", "--json")
		if err != nil {
			t.Fatalf("gc bd create after the reap: %v\n%s", err, out)
		}
		var created struct {
			ID string `json:"id"`
		}
		lastJSONLine(t, out, &created)
		if created.ID == "" {
			t.Fatalf("gc bd create returned no id:\n%s", out)
		}
		if out, err := city.GC("bd", "--rig", rigName, "show", created.ID, "--json"); err != nil {
			t.Fatalf("gc bd show %s: %v\n%s", created.ID, err, out)
		}
		second, err := proxyendpoint.Read(proxyRoot)
		if err != nil {
			t.Fatalf("read the restarted proxy's record: %v", err)
		}
		if second.Birth == first.Birth {
			t.Fatalf("the proxy generation did not change across the reap (birth %q)", second.Birth)
		}
	})

	t.Run("read-in-the-exit-window", func(t *testing.T) {
		before := countIdleExits(t, rigDir)
		deadline := time.Now().Add(idleRetireWait)
		for countIdleExits(t, rigDir) == before {
			if time.Now().After(deadline) {
				t.Fatalf("the restarted proxy did not reach its idle exit within %s", idleRetireWait)
			}
			time.Sleep(50 * time.Millisecond)
		}
		start := time.Now()
		out, err := city.GC("bd", "--rig", rigName, "list", "--json")
		latency := time.Since(start)
		if err != nil {
			t.Fatalf("gc bd list issued in the idle-exit window failed after %s: %v\n%s", latency.Round(time.Millisecond), err, out)
		}
		// bd bounds the wait behind the dying proxy's shutdown GC with its 15s
		// open deadline; gc's own work rides on top.
		t.Logf("gc bd list issued in the idle-exit window succeeded in %s", latency.Round(time.Millisecond))
		if latency > 25*time.Second {
			t.Fatalf("gc bd list in the idle-exit window took %s, beyond bd's open deadline", latency)
		}
	})

	t.Run("doctor-on-the-stopped-city-starts-nothing", func(t *testing.T) {
		waitForProxyRecordGone(t, proxyRoot, idleRetireWait)
		for _, root := range []string{cityRoot, rigDir} {
			if leaked := waitForNoDoltProcesses(t, root, idleRetireWait); len(leaked) > 0 {
				t.Fatalf("processes under %s did not retire:\n%s", root, strings.Join(leaked, "\n"))
			}
		}
		out, _ := city.GC("doctor", "--json")
		var report doctorReport
		lastJSONLine(t, out, &report)
		var idle *doctorCheckResult
		for i := range report.Results {
			if report.Results[i].Name == "proxied-idle-timeout" {
				idle = &report.Results[i]
			}
		}
		if idle == nil || idle.Status != "ok" {
			t.Fatalf("proxied-idle-timeout = %+v, want ok: the scopes carry the value gc resolves", idle)
		}
		for _, r := range report.Results {
			if r.Name == "beads-store" && !strings.Contains(r.Message, doctor.StoreNotRunningMessage) {
				t.Errorf("beads-store on the stopped city = %q, want %q", r.Message, doctor.StoreNotRunningMessage)
			}
		}
		for _, root := range []string{cityRoot, rigDir} {
			if started := helpers.WaitForDoltProcesses(t, root, 3*time.Second); len(started) != 0 {
				t.Fatalf("gc doctor started %d process(es) under %s on a stopped city:\n%s", len(started), root, strings.Join(started, "\n"))
			}
		}
	})

	t.Run("stop-on-a-reaped-scope-is-a-no-op", func(t *testing.T) {
		for i := 1; i <= 2; i++ {
			if out, err := city.GC("stop", cityRoot); err != nil {
				t.Fatalf("gc stop #%d on reaped scopes: %v\n%s", i, err, out)
			}
		}
	})
}

// waitForProxyRecordGone waits for bd to remove a proxy root's record, which
// it does only after the proxy and its Dolt child have exited.
func waitForProxyRecordGone(t *testing.T, proxyRoot string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		if _, err := os.Stat(proxyendpoint.PIDPath(proxyRoot)); os.IsNotExist(err) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("proxy record under %s still present after %s", proxyRoot, timeout)
		}
		time.Sleep(250 * time.Millisecond)
	}
}

// countIdleExits counts bd's idle-exit lines in every proxy.log under a
// scope's .beads directory.
func countIdleExits(t *testing.T, scopeRoot string) int {
	t.Helper()
	needle := "idleWatcher expired after " + idleTimeoutUnderTest.String()
	count := 0
	err := filepath.WalkDir(filepath.Join(scopeRoot, ".beads"), func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || d.Name() != "proxy.log" {
			return nil
		}
		data, readErr := os.ReadFile(path)
		if readErr != nil {
			return nil
		}
		count += strings.Count(string(data), needle)
		return nil
	})
	if err != nil {
		t.Fatalf("scan proxy logs under %s: %v", scopeRoot, err)
	}
	return count
}
