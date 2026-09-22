//go:build acceptance_a

// The proxied-native lane's lifecycle rows (design 5.2, read side).
//
// P2-14 proves the lane's steady state: on a healthy, gc-initialised proxied
// city `gc status --json` costs no bd fork at all. These rows prove the other
// half, which is the half that decides whether the lane is safe to ship: what
// happens when the proxy gc pinned goes away underneath it.
//
// Every row drives a REAL bd proxy and a REAL dolt child, disturbs it the way an
// operator or a crash does — `bd dolt stop`, SIGKILL and SIGTERM on the Dolt
// child, SIGKILL on the proxy itself, a foreign ownership record, a moved proxy
// root — and reads gc's answer back through two independent instruments:
//
//   - the recording BD_BIN shim, which counts EVERY bd fork whatever spawned it,
//     so "at most one ping and one recover per generation" is a measurement
//     rather than a claim about code; and
//   - the `beads-store` payload of the real `gc doctor --json` front door, which
//     carries the store gc opened, the generation it pinned, and the typed
//     verdict if it declined.
//
// What these rows deliberately do NOT do is script an endpoint. A scripted probe
// can produce any verdict on demand and proves only that the switch statement
// has that arm; internal/beads' admission table already pins those arms, with
// injected effects, on every host. What cannot be pinned there is which verdict a
// real bd proxy on a real host actually produces when its Dolt child is killed
// two different ways — and that is the fact the lane's safety argument rests on.
package acceptance_test

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	helpers "github.com/gastownhall/gascity/test/acceptance/helpers"
)

// proxiedNativeCity is one proxied city plus the two instruments every row reads
// it through.
type proxiedNativeCity struct {
	env      *helpers.Env
	lane     *helpers.Env
	calls    *helpers.RecordingBD
	bdPath   string
	city     *helpers.City
	root     string
	proxyDir string
}

// newProxiedNativeCity initialises a quiescent proxied city with the flag-on
// lane available.
//
// `--no-start` on purpose, for the reason P2-14's gate documents: the fork census
// counts every bd fork on the box, and a city with a live controller forks bd on
// its own schedule. bd's proxy still comes up, because the provider readiness op
// is what declares the scope ready.
func newProxiedNativeCity(t *testing.T, bdPath, doltPath string) *proxiedNativeCity {
	t.Helper()
	env, calls := proxiedEnvRecordingBD(t, bdPath, doltPath)
	city := helpers.NewCity(t, env)
	c := &proxiedNativeCity{
		env:    env,
		lane:   proxiedNativeLaneEnv(env),
		calls:  calls,
		bdPath: bdPath,
		city:   city,
		root:   city.Dir,
	}
	t.Cleanup(func() { c.retire(t) })
	city.InitNoStart("claude")
	assertProxiedScope(t, c.root, "the lifecycle city")
	c.proxyDir = proxiedScopeProxyRoot(t, c.root)
	return c
}

// retire takes the city down and makes sure nothing this test started is left
// running.
//
// It is deliberately more forceful than the other proxied fixtures' cleanup, and
// the reason is the test itself: these rows corrupt bd's OWN bookkeeping on
// purpose — a foreign root_id, a proxy root moved out from under a live proxy —
// and bd's `dolt stop` reads that bookkeeping to find what to stop. Observed:
// after the root-move row, `gc stop` and `bd dolt stop` both report success and
// leave the proxy and its Dolt child running, with a `proxy.stop-epoch` and two
// `proxy.pid.stale-*` files behind. That is a statement about a record this test
// deliberately broke, not about gc, so it is killed and LOGGED rather than
// failed — but it is never left running, because the next lane shares this box.
func (c *proxiedNativeCity) retire(t *testing.T) {
	helpers.RunGC(c.env, c.root, "stop", c.root) //nolint:errcheck // best effort
	c.bd(t, "dolt", "stop")                      //nolint:errcheck // best effort: the records may be the ones this test broke
	leaked := waitForNoDoltProcesses(t, c.root, 20*time.Second)
	if len(leaked) == 0 {
		return
	}
	proxies, servers := doltFamilyPIDs(t, c.proxyDir)
	signalPIDs(t, append(append([]int{}, servers...), proxies...), syscall.SIGKILL)
	t.Logf("the lifecycle city needed a hard kill after its records were deliberately corrupted (%d proxy, %d sql-server):\n%s",
		len(proxies), len(servers), strings.Join(leaked, "\n"))
	if still := waitForNoDoltProcesses(t, c.root, 20*time.Second); len(still) > 0 {
		t.Errorf("processes outlived even a SIGKILL sweep:\n%s", strings.Join(still, "\n"))
	}
}

// proxiedScopeProxyRoot resolves the directory bd keeps its proxy record in, with
// bd's own precedence: the sidecar's root_path when it has one, `.beads/dolt`
// otherwise.
func proxiedScopeProxyRoot(t *testing.T, scopeRoot string) string {
	t.Helper()
	var sidecar proxiedSidecar
	readJSONFile(t, filepath.Join(scopeRoot, ".beads", "proxied_server_client_info.json"), &sidecar)
	root := filepath.Join(scopeRoot, ".beads", "dolt")
	if path := strings.TrimSpace(sidecar.RootPath); path != "" {
		if filepath.IsAbs(path) {
			root = path
		} else {
			root = filepath.Join(scopeRoot, ".beads", path)
		}
	}
	if _, err := os.Stat(filepath.Join(root, "proxy.pid")); err != nil {
		t.Fatalf("no proxy record under %s: %v", root, err)
	}
	return root
}

// account runs the real `gc doctor --json` front door in the flag-on lane and
// returns what the beads-store check says about the store it opened.
//
// Every method here takes the SUBTEST's t rather than using the fixture's.
// t.Fatalf on a parent from inside a subtest calls Goexit on the wrong
// goroutine, and `go test` then reports "subtest may have called FailNow on a
// parent test" instead of the failure — which is exactly what the first draft
// of this file did.
func (c *proxiedNativeCity) account(t *testing.T, label string) (beadsStorePayloadDoc, doctorCheckResult) {
	t.Helper()
	return readBeadsStorePayload(t, c.lane, c.root, label)
}

// tryAccount is account for the rows that deliberately break the scope.
//
// A proxy record gc refuses is also a record BD cannot use, so those rows take
// the whole store offline: doctor's bead-store-preflight fails, the 16 store
// checks it gates are skipped, and there is no beads-store result to read at
// all. That is a legitimate outcome to observe and not a reason to fail, so this
// form reports absence instead of failing on it.
func (c *proxiedNativeCity) tryAccount(t *testing.T, label string) (beadsStorePayloadDoc, doctorCheckResult, bool) {
	t.Helper()
	out, err := helpers.RunGC(c.lane, c.root, "doctor", "--json")
	if err != nil {
		t.Logf("%s: gc doctor --json exited non-zero (expected while the scope is disturbed): %v", label, err)
	}
	var report doctorReport
	lastJSONLine(t, out, &report)
	for _, r := range report.Results {
		if r.Name != "beads-store" || len(r.Payload) == 0 {
			continue
		}
		var payload beadsStorePayloadDoc
		if err := json.Unmarshal(r.Payload, &payload); err != nil {
			t.Fatalf("parse beads-store payload on %s: %v\n%s", label, err, r.Payload)
		}
		return payload, r, true
	}
	return beadsStorePayloadDoc{}, doctorCheckResult{}, false
}

// laneServesNatively answers the question without opening a store diagnostic at
// all: on this city `gc status --json` costs ZERO bd forks when the split store
// serves the session snapshot and eight when the bd front door does.
//
// It exists for the rows that break the scope badly enough that doctor cannot
// report a beads-store result. The fork census is the one instrument that still
// works when bd itself cannot read the scope's proxy record, and the count is
// the same fact the P2-14 gate is built on.
func (c *proxiedNativeCity) laneServesNatively(t *testing.T, label string) (native bool, census proxiedForkCensus) {
	t.Helper()
	c.reset(t)
	out, err := helpers.RunGC(c.lane, c.root, "status", "--json")
	if err != nil {
		t.Logf("%s: gc status --json exited non-zero: %v\n%s", label, err, out)
	}
	census = c.census(t)
	return census.Total == 0, census
}

// bd runs the pinned bd directly in the city, the way an operator does.
func (c *proxiedNativeCity) bd(t *testing.T, args ...string) (string, error) {
	t.Helper()
	cmd := exec.Command(c.bdPath, args...) //nolint:gosec // resolved test binary
	cmd.Dir = c.root
	cmd.Env = c.env.List()
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// reset clears the fork census so the next row's count is attributable to it,
// and proves the city is quiescent first: a background fork would be counted
// against whatever ran next.
func (c *proxiedNativeCity) reset(t *testing.T) {
	t.Helper()
	c.calls.Reset()
}

// heal brings the city back to a served native lane, so each row starts from the
// same place. It returns the generation the lane settled on.
func (c *proxiedNativeCity) heal(t *testing.T, label string) string {
	t.Helper()
	deadline := time.Now().Add(90 * time.Second)
	var last beadsStorePayloadDoc
	var lastResult doctorCheckResult
	for time.Now().Before(deadline) {
		if out, err := c.bd(t, "ping"); err != nil {
			t.Logf("%s: bd ping: %v\n%s", label, err, out)
		}
		var ok bool
		last, lastResult, ok = c.tryAccount(t, label+" (healing)")
		if !ok {
			time.Sleep(time.Second)
			continue
		}
		if last.Store == "NativeDoltStore" && last.Proxied != nil && !last.Proxied.Demoted {
			return last.Proxied.Endpoint.Generation
		}
		time.Sleep(time.Second)
	}
	t.Fatalf("%s: the city never came back to a served native lane: store=%q %s\n  %s",
		label, last.Store, describeProxiedAccount(last), lastResult.Message)
	return ""
}

// doltFamilyPIDs splits the live processes under root into bd's proxy children
// and the dolt sql-servers behind them.
//
// The process table, not a pid file: the whole point of several rows below is a
// process whose record no longer describes it.
func doltFamilyPIDs(t *testing.T, root string) (proxies, servers []int) {
	t.Helper()
	out, err := exec.Command("ps", "-eo", "pid=,args=").Output()
	if err != nil {
		t.Fatalf("read process table: %v", err)
	}
	for _, line := range strings.Split(string(out), "\n") {
		line = strings.TrimSpace(line)
		if !strings.Contains(line, root) {
			continue
		}
		pidText, args, ok := strings.Cut(line, " ")
		if !ok {
			continue
		}
		pid, convErr := strconv.Atoi(pidText)
		if convErr != nil {
			continue
		}
		switch {
		case strings.Contains(args, "db-proxy-child"):
			proxies = append(proxies, pid)
		case strings.Contains(args, "sql-server"):
			servers = append(servers, pid)
		}
	}
	sort.Ints(proxies)
	sort.Ints(servers)
	return proxies, servers
}

// signalPIDs delivers sig to each pid, reporting how many landed.
func signalPIDs(t *testing.T, pids []int, sig syscall.Signal) int {
	t.Helper()
	delivered := 0
	for _, pid := range pids {
		proc, err := os.FindProcess(pid)
		if err != nil {
			continue
		}
		if err := proc.Signal(sig); err == nil {
			delivered++
		}
	}
	return delivered
}

// waitForPIDsGone polls until none of pids is alive, or timeout.
func waitForPIDsGone(t *testing.T, pids []int, timeout time.Duration) []int {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		var alive []int
		for _, pid := range pids {
			if proc, err := os.FindProcess(pid); err == nil && proc.Signal(syscall.Signal(0)) == nil {
				alive = append(alive, pid)
			}
		}
		if len(alive) == 0 || time.Now().After(deadline) {
			return alive
		}
		time.Sleep(200 * time.Millisecond)
	}
}

// proxiedForkCensus is the shape of one row's bd spend.
type proxiedForkCensus struct {
	Total     int
	Pings     int
	DoltStops int
}

func (c *proxiedNativeCity) census(t *testing.T) proxiedForkCensus {
	t.Helper()
	return proxiedForkCensus{
		Total:     c.calls.Count(),
		Pings:     c.calls.Count("ping"),
		DoltStops: c.calls.Count("dolt", "stop"),
	}
}

func (f proxiedForkCensus) String() string {
	return fmt.Sprintf("%d fork(s), %d ping(s), %d `dolt stop`", f.Total, f.Pings, f.DoltStops)
}

// scopeFileSnapshot lists the names under a scope's .beads directory, one level
// deep plus the proxy root's own entries.
//
// It exists to pin the claim that gc writes nothing into a scope bd owns: no
// port file, no data-dir claim, no runtime state of its own. Sizes and contents
// are deliberately not compared — bd's own records change on every respawn, and
// what matters is that gc added no NAME.
func scopeFileSnapshot(t *testing.T, scopeRoot, proxyRoot string) []string {
	t.Helper()
	var names []string
	for _, dir := range []string{filepath.Join(scopeRoot, ".beads"), proxyRoot} {
		entries, err := os.ReadDir(dir)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			t.Fatalf("read %s: %v", dir, err)
		}
		rel, _ := filepath.Rel(scopeRoot, dir)
		for _, e := range entries {
			names = append(names, filepath.Join(rel, e.Name()))
		}
	}
	sort.Strings(names)
	return names
}

func TestProxiedNativeLifecycle(t *testing.T) {
	bdPath, doltPath := requireProxiedTooling(t)
	c := newProxiedNativeCity(t, bdPath, doltPath)

	before := scopeFileSnapshot(t, c.root, c.proxyDir)

	var baseGeneration string
	t.Run("healthy-baseline", func(t *testing.T) {
		// The control every other row is read against: on an undisturbed city
		// the lane serves natively and spends no bd verb to decide it.
		c.reset(t)
		payload, result := c.account(t, "an undisturbed proxied city")
		census := c.census(t)
		t.Logf("healthy baseline: %s; store=%q %s", census, payload.Store, describeProxiedAccount(payload))
		if payload.Store != "NativeDoltStore" {
			t.Fatalf("an undisturbed proxied city did not serve natively: store=%q %s\n  %s",
				payload.Store, describeProxiedAccount(payload), result.Message)
		}
		if census.Pings != 0 {
			t.Errorf("admission spent %d ping(s) on a healthy proxy, want 0:\n%s", census.Pings, c.calls.Describe())
		}
		baseGeneration = payload.Proxied.Endpoint.Generation
		if baseGeneration == "" {
			t.Fatalf("a served open pinned no generation: %s", describeProxiedAccount(payload))
		}
	})

	t.Run("stop-ping-repin", func(t *testing.T) {
		// The operator move. `bd dolt stop` retires the proxy AND its Dolt child
		// and removes the record, which is the "absent record" arm: one ping, and
		// the lane re-pins to whatever bd produced rather than demoting.
		if out, err := c.bd(t, "dolt", "stop"); err != nil {
			t.Fatalf("bd dolt stop: %v\n%s", err, out)
		}
		if leaked := waitForNoDoltProcesses(t, c.root, 30*time.Second); len(leaked) > 0 {
			t.Fatalf("bd dolt stop left processes behind:\n%s", strings.Join(leaked, "\n"))
		}

		c.reset(t)
		payload, result := c.account(t, "a city whose proxy was stopped")
		census := c.census(t)
		t.Logf("stop-ping-repin: %s; store=%q %s", census, payload.Store, describeProxiedAccount(payload))

		if payload.Store != "NativeDoltStore" {
			t.Fatalf("the lane did not re-pin after a stop: store=%q %s\n  %s",
				payload.Store, describeProxiedAccount(payload), result.Message)
		}
		if census.Pings != 1 {
			t.Errorf("a stopped proxy cost %d ping(s), want exactly 1 — bd either adopts or restarts its proxy, and one ask is the whole escalation an absent record is worth:\n%s",
				census.Pings, c.calls.Describe())
		}
		if census.DoltStops != 0 {
			t.Errorf("a stopped proxy cost %d `bd dolt stop`; recover is for a zombie, not for an absent record:\n%s",
				census.DoltStops, c.calls.Describe())
		}
		if got := payload.Proxied.Endpoint.Generation; got == baseGeneration {
			t.Errorf("the lane re-pinned to the same generation %q across a full stop/start; the generation is {pid,birth} and cannot survive one", got)
		}
		baseGeneration = payload.Proxied.Endpoint.Generation
	})

	t.Run("dead-record-one-ping", func(t *testing.T) {
		// SIGKILL the proxy itself. Nothing cleans up, so the record survives and
		// names a pid that is gone: the "dead record" arm, decided from the
		// process table with no dial at all, and worth exactly one ping.
		proxies, _ := doltFamilyPIDs(t, c.proxyDir)
		if len(proxies) == 0 {
			t.Fatalf("no bd proxy under %s to kill", c.proxyDir)
		}
		if signalPIDs(t, proxies, syscall.SIGKILL) == 0 {
			t.Fatalf("could not signal the proxy %v", proxies)
		}
		if alive := waitForPIDsGone(t, proxies, 20*time.Second); len(alive) > 0 {
			t.Fatalf("the proxy survived SIGKILL: %v", alive)
		}
		if _, err := os.Stat(filepath.Join(c.proxyDir, "proxy.pid")); err != nil {
			t.Skipf("bd removed its record on SIGKILL (%v), so this host cannot produce the dead-record arm", err)
		}

		c.reset(t)
		payload, result := c.account(t, "a city whose proxy was killed")
		census := c.census(t)
		t.Logf("dead-record-one-ping: %s; store=%q %s", census, payload.Store, describeProxiedAccount(payload))

		if payload.Store != "NativeDoltStore" {
			t.Fatalf("the lane did not recover from a dead record: store=%q %s\n  %s",
				payload.Store, describeProxiedAccount(payload), result.Message)
		}
		if census.Pings != 1 {
			t.Errorf("a dead record cost %d ping(s), want exactly 1:\n%s", census.Pings, c.calls.Describe())
		}
		if census.DoltStops != 0 {
			t.Errorf("a dead record cost %d `bd dolt stop`, want 0:\n%s", census.DoltStops, c.calls.Describe())
		}
		if got := payload.Proxied.Endpoint.Generation; got == baseGeneration {
			t.Errorf("the lane re-pinned to the killed generation %q", got)
		}
		baseGeneration = c.heal(t, "after the dead-record row")
	})

	// Both child-crash exits, because they are different endpoint states and
	// which one a real bd produces is not knowable from the source. Measured
	// here, twice, on this host:
	//
	//   kill -9   the proxy goes with its child, so the record it left behind
	//             names a pid that is gone: the DEAD-RECORD arm, decided from the
	//             process table with no dial, worth exactly one ping.
	//   kill -TERM the proxy survives its child and its data port accepts and
	//             never greets: the ZOMBIE ladder, which asks again three times
	//             across two seconds, then spends one ping, then one recover —
	//             and a recover is `bd dolt stop` followed by `bd ping`, so it
	//             shows up as one `dolt stop` and a second ping.
	t.Run("child-kill9", func(t *testing.T) {
		baseGeneration = c.childCrashRow(t, childCrashExpectation{
			signal:    syscall.SIGKILL,
			label:     "kill -9",
			pings:     1,
			doltStops: 0,
			mechanism: "the proxy goes down with its child, so its record names a dead pid and one ping is the whole escalation a dead record is worth",
		}, baseGeneration)
	})

	t.Run("child-term-zombie", func(t *testing.T) {
		baseGeneration = c.childCrashRow(t, childCrashExpectation{
			signal:    syscall.SIGTERM,
			label:     "kill -TERM",
			pings:     2,
			doltStops: 1,
			mechanism: "the proxy survives and its data port accepts without greeting, so the ladder spends one ping and then one recover (`bd dolt stop` plus a ping) — once per generation, ever",
		}, baseGeneration)
	})

	t.Run("foreign-root", func(t *testing.T) {
		// A proxy.pid whose root_id belongs to somebody else's workspace: the
		// copied-record shape. It must be decided from the record alone — no
		// dial, no bd verb — because dialing a port named by a record that does
		// not validate for this root is talking to whatever is listening there.
		recordPath := filepath.Join(c.proxyDir, "proxy.pid")
		original, err := os.ReadFile(recordPath)
		if err != nil {
			t.Fatalf("read the proxy record: %v", err)
		}
		t.Cleanup(func() {
			if err := os.WriteFile(recordPath, original, 0o600); err != nil {
				t.Errorf("restore the proxy record: %v", err)
			}
		})
		var record map[string]any
		if err := json.Unmarshal(original, &record); err != nil {
			t.Fatalf("parse the proxy record: %v\n%s", err, original)
		}
		record["root_id"] = strings.Repeat("ab", 32)
		foreign, err := json.Marshal(record)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(recordPath, foreign, 0o600); err != nil {
			t.Fatal(err)
		}

		// The measurement is the fork census, not the doctor payload.
		//
		// A record gc refuses is also a record BD cannot use: with a foreign
		// root_id the bd CLI cannot reach the scope either, doctor's
		// bead-store-preflight fails, and the 16 store checks it gates — including
		// beads-store — are not reported at all. The census still answers the
		// question that matters, because `gc status --json` costs zero forks on
		// this city when the split store serves it and eight when the bd front
		// door does.
		native, census := c.laneServesNatively(t, "a city whose proxy record names a foreign root")
		t.Logf("foreign-root: %s; native=%v", census, native)
		if native {
			t.Fatalf("the lane served natively over a record that does not validate for this root: %s\n%s",
				census, c.calls.Describe())
		}
		if census.Pings != 0 || census.DoltStops != 0 {
			t.Errorf("a foreign record cost %s; a record that does not validate for this root is never worth a bd verb, because asking bd to fix somebody else's proxy is not a move gc has:\n%s",
				census, c.calls.Describe())
		}
		// Best effort, because the scope is offline for bd too: when doctor does
		// manage to report a beads-store result, its verdict must name the
		// refusal rather than some generic unavailability.
		if payload, result, ok := c.tryAccount(t, "the foreign-root city"); ok {
			t.Logf("foreign-root doctor account: store=%q %s", payload.Store, describeProxiedAccount(payload))
			if payload.Store == "NativeDoltStore" {
				t.Errorf("doctor reports a native store over a foreign record: %s", result.Message)
			}
			if payload.Proxied != nil && payload.Proxied.Verdict != "" && payload.Proxied.Verdict != "not_ours" {
				t.Errorf("a foreign root_id produced verdict %q, want not_ours", payload.Proxied.Verdict)
			}
			// doctor's own independent endpoint account is where "no dial" is
			// visible from outside: a record it classified foreign must not have
			// been probed.
			if payload.Endpoint != nil && payload.Endpoint.Probe == "served" {
				t.Errorf("doctor probed a port named by a record it classified %q: %s",
					payload.Endpoint.Verdict, describeEndpointAccount(payload))
			}
		}
	})

	t.Run("root-move", func(t *testing.T) {
		// U22's shape, as far as a city scope can express it: the proxy root is
		// moved out from under a live proxy. The pinned endpoint keeps listening
		// and keeps serving the moved database, so the only thing standing
		// between gc and a read from a root nobody can name is the record read
		// admission does before it pins.
		//
		// What is asserted is the safety property, not a particular verdict: gc
		// must not serve natively out of a root whose record it cannot read, and
		// it must come back when the root does.
		moved := c.proxyDir + ".moved"
		if err := os.Rename(c.proxyDir, moved); err != nil {
			t.Fatalf("move the proxy root: %v", err)
		}
		restored := false
		t.Cleanup(func() {
			if restored {
				return
			}
			_ = os.RemoveAll(c.proxyDir)
			if err := os.Rename(moved, c.proxyDir); err != nil {
				t.Errorf("restore the proxy root: %v", err)
			}
		})

		native, census := c.laneServesNatively(t, "a city whose proxy root was moved")
		t.Logf("root-move: %s; native=%v", census, native)
		if native {
			t.Errorf("the lane served natively out of a proxy root that is no longer there:\n%s", c.calls.Describe())
		}

		// And it re-pins when the root comes back, rather than staying demoted
		// for the life of the city.
		if err := os.RemoveAll(c.proxyDir); err != nil {
			t.Fatal(err)
		}
		if err := os.Rename(moved, c.proxyDir); err != nil {
			t.Fatalf("restore the proxy root: %v", err)
		}
		restored = true
		baseGeneration = c.heal(t, "after the root came back")
		t.Logf("root-move: re-pinned to %s after the root was restored", baseGeneration)
	})

	t.Run("scope-artifacts-no-gc-claim", func(t *testing.T) {
		// The whole lifecycle above ran with the native lane on, and the claim
		// this row pins is that gc wrote nothing into a scope bd owns.
		//
		// It is stated as FORBIDDEN names rather than an allowlist of permitted
		// ones. The allowlist form was tried first and is the wrong shape: bd and
		// dolt legitimately add files across a stop/start cycle (`machine-id`,
		// `eventsData`, `proxy.stop-epoch`, `proxy.pid.stale-<epoch>` were all
		// observed), so an allowlist has to grow with every beads release and each
		// entry weakens the assertion it is supposed to make. The names below are
		// the ones that would mean a SECOND owner for bd's Dolt process — gc's own
		// server pid, its port, its socket, its managed runtime state — and none
		// of them may ever appear.
		after := scopeFileSnapshot(t, c.root, c.proxyDir)
		t.Logf("scope artifacts added across the lifecycle (bd's and dolt's own): %v", namesNotIn(after, before))
		if removed := namesNotIn(before, after); len(removed) > 0 {
			t.Logf("scope artifacts removed across the lifecycle: %v", removed)
		}
		for _, name := range after {
			switch base := filepath.Base(name); base {
			case "dolt-server.pid", "dolt-server.port", "dolt-server.socket", "dolt-state.json":
				t.Errorf("the native lane left %q in a scope bd owns; that file is gc claiming a Dolt lifecycle it does not have", name)
			}
		}
		// And gc's managed-Dolt runtime state, which lives outside .beads, must
		// not exist either: writing it would mean two owners for one process.
		if _, err := os.Stat(filepath.Join(c.root, ".gc", "runtime", "packs", "dolt", "dolt-state.json")); err == nil {
			t.Error("the native lane published managed-Dolt runtime state for a bd-owned scope")
		} else if !os.IsNotExist(err) {
			t.Fatalf("probe managed dolt state: %v", err)
		}
	})
}

// childCrashExpectation is one crash exit and the bd spend it is allowed.
type childCrashExpectation struct {
	signal    syscall.Signal
	label     string
	pings     int
	doltStops int
	mechanism string
}

// childCrashRow kills the Dolt child behind bd's proxy and reads back both how
// the lane classified it and what it spent doing so.
//
// The counts are asserted EXACTLY, not as upper bounds, and both directions
// matter. Over budget means the ladder is spending a rung twice — the failure the
// generation sets exist to prevent. Under budget means the escalation this host
// actually produces has changed shape, which is the fact this row exists to
// record and not something to wave through.
func (c *proxiedNativeCity) childCrashRow(t *testing.T, want childCrashExpectation, baseGeneration string) string {
	t.Helper()
	_, servers := doltFamilyPIDs(t, c.proxyDir)
	if len(servers) == 0 {
		t.Fatalf("no dolt sql-server under %s to kill", c.proxyDir)
	}
	if signalPIDs(t, servers, want.signal) == 0 {
		t.Fatalf("could not %s the dolt child %v", want.label, servers)
	}
	if alive := waitForPIDsGone(t, servers, 30*time.Second); len(alive) > 0 {
		t.Logf("%s: the dolt child %v is still alive; the row measures whatever the proxy now answers", want.label, alive)
	}

	c.reset(t)
	payload, result := c.account(t, "a city whose dolt child took "+want.label)
	census := c.census(t)
	t.Logf("%s: %s; store=%q %s", want.label, census, payload.Store, describeProxiedAccount(payload))

	if census.Pings != want.pings || census.DoltStops != want.doltStops {
		t.Errorf("%s cost %s, want %d ping(s) and %d `bd dolt stop`: %s\n%s",
			want.label, census, want.pings, want.doltStops, want.mechanism, c.calls.Describe())
	}
	if payload.Store != "NativeDoltStore" {
		t.Fatalf("%s: the lane did not come back after the escalation: store=%q %s\n  %s",
			want.label, payload.Store, describeProxiedAccount(payload), result.Message)
	}
	if got := payload.Proxied.Endpoint.Generation; got == baseGeneration {
		t.Errorf("%s: the lane is still pinned to generation %q, which no longer has a Dolt child behind it", want.label, got)
	}
	return payload.Proxied.Endpoint.Generation
}

// namesNotIn returns the members of a that b does not contain.
func namesNotIn(a, b []string) []string {
	have := make(map[string]bool, len(b))
	for _, name := range b {
		have[name] = true
	}
	var out []string
	for _, name := range a {
		if !have[name] {
			out = append(out, name)
		}
	}
	return out
}
