package main

import (
	"bytes"
	"context"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/api"
	"github.com/gastownhall/gascity/internal/citylayout"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/fsys"
	"github.com/gastownhall/gascity/internal/supervisor"
	"github.com/gastownhall/gascity/internal/suspensionstate"
)

// suspendDeadline is how long `gc suspend` may take against an API that
// never answers.
const suspendDeadline = 5 * time.Second

// neverAnswer accepts every connection on ln and never writes a byte back, as
// a wedged server does, and returns the count of connections it accepted. ln
// and the connections close on test cleanup.
func neverAnswer(t *testing.T, ln net.Listener) *atomic.Int64 {
	t.Helper()
	var accepted atomic.Int64
	var mu sync.Mutex
	var conns []net.Conn
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			accepted.Add(1)
			mu.Lock()
			conns = append(conns, c)
			mu.Unlock()
		}
	}()
	t.Cleanup(func() {
		ln.Close() //nolint:errcheck // test cleanup
		mu.Lock()
		defer mu.Unlock()
		for _, c := range conns {
			c.Close() //nolint:errcheck // test cleanup
		}
	})
	return &accepted
}

// hangingAPI is the URL of an API server that accepts every connection and
// never answers.
func hangingAPI(t *testing.T) string {
	t.Helper()
	ln := listenOnRandomPort(t)
	neverAnswer(t, ln)
	return "http://" + ln.Addr().String()
}

// routeToAPI routes the city's API client to c. Route resolution runs as in
// production up to the supervisor-managed client: the real controller
// identity probe asks the city's control socket, and a city without a live
// controller gets c.
func routeToAPI(t *testing.T, c *api.Client) {
	t.Helper()
	origIdentity, origSup := apiRouteControllerIdentityHook, apiRouteSupervisorClientHook
	t.Cleanup(func() { apiRouteControllerIdentityHook, apiRouteSupervisorClientHook = origIdentity, origSup })
	apiRouteControllerIdentityHook = probeControllerIdentity
	apiRouteSupervisorClientHook = func(string) *api.Client { return c }
}

// routeToHangingAPI routes the city's API client to a server that never
// answers.
func routeToHangingAPI(t *testing.T, cityName string) {
	t.Helper()
	routeToAPI(t, api.NewCityScopedClient(hangingAPI(t), cityName))
}

// awaitSuspendAPIAttempts keeps the answer channel of each API attempt gc
// suspend starts and, at cleanup, waits for every attempt to end, so an
// attempt the command stopped waiting for reads no hook after a cleanup
// restores it. Call it after setting the hooks the attempt reads, so its
// cleanup runs before theirs, and before starting a server the attempt can
// block on, so that server closes first.
func awaitSuspendAPIAttempts(t *testing.T) {
	t.Helper()
	orig := suspendThroughAPIHook
	var attempts []<-chan suspendAPIAnswer
	suspendThroughAPIHook = func(ctx context.Context, cityPath string) <-chan suspendAPIAnswer {
		answer := orig(ctx, cityPath)
		attempts = append(attempts, answer)
		return answer
	}
	t.Cleanup(func() {
		suspendThroughAPIHook = orig
		for _, answer := range attempts {
			select {
			case <-answer:
			case <-time.After(hangBudget):
				t.Errorf("gc suspend's API attempt did not end within %s of the test", hangBudget)
			}
		}
	})
}

// AC-3: against an API that accepts connections but never responds, `gc
// suspend` exits 0 within 5 seconds with the city recorded as suspended: with
// a good city.toml, with one that does not parse (a warning), and over a
// malformed suspension file (replaced, its parse error printed).
func TestSuspendCannotBeBlockedByAHangingAPI(t *testing.T) {
	t.Setenv("GC_NO_API", "")
	for _, tc := range []struct {
		name       string
		cityTOML   string
		suspension string
		stderr     []string
	}{
		{name: "good config", cityTOML: "[workspace]\nname = \"hang\"\n"},
		{name: "config does not parse", cityTOML: "[workspace\nname = ", stderr: []string{"warning"}},
		{name: "malformed suspension file", cityTOML: "[workspace]\nname = \"hang\"\n", suspension: `{"city":{"susp`, stderr: []string{"replaced", "unexpected end of JSON input"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cityPath := t.TempDir()
			if err := os.WriteFile(cityPath+"/city.toml", []byte(tc.cityTOML), 0o644); err != nil {
				t.Fatal(err)
			}
			if tc.suspension != "" {
				writeSuspensionFile(t, cityPath, tc.suspension)
			}
			routeToHangingAPI(t, "hang")
			var stdout, stderr bytes.Buffer
			start := time.Now()
			code := cmdSuspend([]string{cityPath}, false, &stdout, &stderr)
			if took := time.Since(start); took >= suspendDeadline {
				t.Fatalf("gc suspend took %s against a hanging API, want under %s", took, suspendDeadline)
			} else if took < suspendAPITimeout-time.Second {
				t.Fatalf("gc suspend took %s, want about %s: the API route was not attempted before the direct write", took, suspendAPITimeout)
			}
			if code != 0 {
				t.Fatalf("gc suspend exit %d, want 0; stderr: %s", code, stderr.String())
			}
			st, err := suspensionstate.Load(fsys.OSFS{}, cityPath)
			if err != nil {
				t.Fatalf("suspension file after gc suspend: %v", err)
			}
			if !suspensionstate.IsCitySuspended(st) {
				t.Fatalf("suspension file %s does not record the city suspended: %+v", citylayout.SuspensionStateFile(cityPath), st)
			}
			for _, want := range tc.stderr {
				if !strings.Contains(stderr.String(), want) {
					t.Fatalf("stderr %q does not contain %q", stderr.String(), want)
				}
			}
		})
	}
}

// AC-3 for a supervisor-hosted city, #7170's deployment: the supervisor's API
// accepts connections and never answers. Route resolution runs as in
// production from the identity probe through the supervisor-managed client,
// with only the supervisor's liveness stubbed, so it asks that API which
// cities the supervisor runs. Resolution shares the command's deadline, so
// `gc suspend` still exits 0 within 5 seconds with the city recorded
// suspended.
func TestSuspendCannotBeBlockedByAHangingSupervisorAPI(t *testing.T) {
	t.Setenv("GC_NO_API", "")
	t.Setenv("GC_HOME", t.TempDir())
	cityPath := t.TempDir()
	if err := os.WriteFile(filepath.Join(cityPath, "city.toml"), []byte("[workspace]\nname = \"hosted\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := supervisor.NewRegistry(supervisor.RegistryPath()).Register(cityPath, "hosted"); err != nil {
		t.Fatalf("register city: %v", err)
	}
	origAlive := supervisorAliveHook
	t.Cleanup(func() { supervisorAliveHook = origAlive })
	supervisorAliveHook = func() int { return 4242 }
	awaitSuspendAPIAttempts(t)
	ln := listenOnRandomPort(t)
	asked := neverAnswer(t, ln)
	supervisorTOML := fmt.Sprintf("[supervisor]\nbind = \"127.0.0.1\"\nport = %d\n", ln.Addr().(*net.TCPAddr).Port)
	if err := os.WriteFile(supervisor.ConfigPath(), []byte(supervisorTOML), 0o644); err != nil {
		t.Fatal(err)
	}

	var stdout, stderr bytes.Buffer
	start := time.Now()
	code := cmdSuspend([]string{cityPath}, false, &stdout, &stderr)
	if took := time.Since(start); took >= suspendDeadline {
		t.Fatalf("gc suspend took %s against a hanging supervisor API, want under %s", took, suspendDeadline)
	}
	if code != 0 {
		t.Fatalf("gc suspend exit %d, want 0; stderr: %s", code, stderr.String())
	}
	if asked.Load() == 0 {
		t.Fatal("route resolution never asked the supervisor's API, so the test proves nothing about it")
	}
	st, err := suspensionstate.Load(fsys.OSFS{}, cityPath)
	if err != nil || !suspensionstate.IsCitySuspended(st) {
		t.Fatalf("suspension file after gc suspend: state %+v err %v, want the city suspended", st, err)
	}
}

// AC-3: `gc resume` still refuses a city.toml that does not parse and a
// malformed suspension file, naming the remedy, and leaves the file as it was.
func TestResumeRefusesBrokenConfigAndMalformedSuspensionFile(t *testing.T) {
	for _, tc := range []struct {
		name, cityTOML, suspension, remedy string
	}{
		{name: "config does not parse", cityTOML: "[workspace\nname = ", remedy: "city.toml"},
		{name: "malformed suspension file", cityTOML: "[workspace]\nname = \"r\"\n", suspension: `{"city":`, remedy: "remove the file"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cityPath := t.TempDir()
			if err := os.WriteFile(cityPath+"/city.toml", []byte(tc.cityTOML), 0o644); err != nil {
				t.Fatal(err)
			}
			if tc.suspension != "" {
				writeSuspensionFile(t, cityPath, tc.suspension)
			}
			var stdout, stderr bytes.Buffer
			if code := doSuspendCity(fsys.OSFS{}, cityPath, false, false, &stdout, &stderr); code == 0 {
				t.Fatalf("gc resume exit 0, want a refusal; stdout: %s", stdout.String())
			}
			if !strings.Contains(stderr.String(), tc.remedy) {
				t.Fatalf("stderr %q does not name the remedy %q", stderr.String(), tc.remedy)
			}
			if tc.suspension != "" {
				got, err := os.ReadFile(citylayout.SuspensionStateFile(cityPath))
				if err != nil || string(got) != tc.suspension {
					t.Fatalf("suspension file = %q (%v), want it left as %q", got, err, tc.suspension)
				}
			}
		})
	}
}

// AC-3 under a controller that is alive but does not answer: the city's
// control socket accepts connections but never replies, so the real identity
// probe alone (an identify read, then a ping read) outlasts the API deadline,
// which bounds route resolution as well as the request. `gc suspend` still
// exits 0 within 5 seconds, and it leaves the city's stores to that
// controller, which retires them once the sessions it still runs have
// drained, instead of stopping them under it. With nothing listening on the
// socket no controller runs, and gc suspend stops them itself.
func TestSuspendLeavesStoresToAControllerThatDoesNotAnswer(t *testing.T) {
	t.Run("a controller that never answers keeps its stores", func(t *testing.T) {
		t.Setenv("GC_NO_API", "")
		city, _, logPath := providerOwnedHealthFixture(t, "")
		neverAnswer(t, listenControllerSocketForTest(t, city))
		routeToHangingAPI(t, "wedged")
		awaitSuspendAPIAttempts(t)
		var stdout, stderr bytes.Buffer
		start := time.Now()
		code := cmdSuspend([]string{city}, false, &stdout, &stderr)
		if took := time.Since(start); took >= suspendDeadline {
			t.Fatalf("gc suspend took %s against a wedged controller, want under %s", took, suspendDeadline)
		}
		if code != 0 {
			t.Fatalf("gc suspend exit %d, want 0; stderr: %s", code, stderr.String())
		}
		if !strings.Contains(stdout.String(), "City suspended") {
			t.Fatalf("stdout %q, want the suspension reported", stdout.String())
		}
		if ops := providerOpsLogged(t, logPath); ops != "" {
			t.Fatalf("provider ops = %q, want no store stopped under a running controller", ops)
		}
		st, err := suspensionstate.Load(fsys.OSFS{}, city)
		if err != nil || !suspensionstate.IsCitySuspended(st) {
			t.Fatalf("suspension file after gc suspend: state %+v err %v, want the city suspended", st, err)
		}
	})
	t.Run("no controller: gc suspend stops them", func(t *testing.T) {
		t.Setenv("GC_NO_API", "1")
		city, _, logPath := providerOwnedHealthFixture(t, "")
		var stdout, stderr bytes.Buffer
		if code := cmdSuspend([]string{city}, false, &stdout, &stderr); code != 0 {
			t.Fatalf("gc suspend exit %d, want 0; stderr: %s", code, stderr.String())
		}
		if ops := providerOpsLogged(t, logPath); !strings.Contains(ops, "stop") {
			t.Fatalf("provider ops = %q, want the suspended city's stores stopped", ops)
		}
	})
}

// AC-3 through a responsive API: `gc suspend` repairs a malformed suspension
// file through the controller's API as it does when it writes the file
// itself. The controller replaces the file, records the city suspended and
// names the discarded parse error in the city.suspended event. `gc resume`
// through the same API still refuses such a file and leaves it as it was.
func TestSuspendThroughAResponsiveAPIReplacesAMalformedFile(t *testing.T) {
	t.Setenv("GC_NO_API", "")
	h := newCharCity(t, charCityBasic, nil)
	for _, l := range h.lanes(t) {
		if l.name == "alive" {
			routeToAPI(t, l.client)
		}
	}
	const malformed = `{"city":{"susp`
	writeSuspensionFile(t, h.cityPath, malformed)

	var stdout, stderr bytes.Buffer
	if code := cmdSuspend([]string{h.cityPath}, false, &stdout, &stderr); code != 0 {
		t.Fatalf("gc suspend exit %d, want 0; stderr: %s", code, stderr.String())
	}
	st, err := suspensionstate.Load(fsys.OSFS{}, h.cityPath)
	if err != nil || !suspensionstate.IsCitySuspended(st) {
		t.Fatalf("suspension file after gc suspend: state %+v err %v, want the city suspended", st, err)
	}
	rec, ok := h.cs.eventProv.(*events.Fake)
	if !ok {
		t.Fatalf("controller event provider is %T, want *events.Fake", h.cs.eventProv)
	}
	suspended, err := rec.List(events.Filter{Type: events.CitySuspended})
	if err != nil {
		t.Fatalf("listing %s events: %v", events.CitySuspended, err)
	}
	var noted bool
	for _, ev := range suspended {
		if strings.Contains(ev.Message, "unexpected end of JSON input") {
			noted = true
		}
	}
	if !noted {
		t.Fatalf("%s events %+v: want one naming the discarded parse error", events.CitySuspended, suspended)
	}

	writeSuspensionFile(t, h.cityPath, malformed)
	stdout.Reset()
	stderr.Reset()
	if code := cmdResume([]string{h.cityPath}, false, &stdout, &stderr); code == 0 {
		t.Fatalf("gc resume through the API exit 0 over a malformed file, want a refusal; stdout: %s", stdout.String())
	}
	if got, err := os.ReadFile(citylayout.SuspensionStateFile(h.cityPath)); err != nil || string(got) != malformed {
		t.Fatalf("suspension file after the refused resume = %q (%v), want it left as %q", got, err, malformed)
	}
}

// AC-3 through a responsive API over a city.toml that does not parse: the
// suspension file is runtime state, not config, so the controller's API
// suspends the city without reloading city.toml. `gc suspend` exits 0, the
// controller records city.suspended and is woken for a pass, which re-reads
// the file, as when gc suspend writes the file itself. `gc resume` through the
// same API refuses that config before it writes: the city stays suspended
// and no city.resumed is recorded.
func TestSuspendThroughAResponsiveAPIOverAnUnparseableCityTOML(t *testing.T) {
	t.Setenv("GC_NO_API", "")
	h := newCharCity(t, charCityBasic, nil)
	h.cs.pokeCh = make(chan struct{}, 1) // before lanes starts the server that reads it
	var alive charLane
	for _, l := range h.lanes(t) {
		if l.name == "alive" {
			alive = l
		}
	}
	routeToAPI(t, alive.client)
	if err := os.WriteFile(filepath.Join(h.cityPath, "city.toml"), []byte("[workspace\nname = "), 0o644); err != nil {
		t.Fatal(err)
	}
	rec, ok := h.cs.eventProv.(*events.Fake)
	if !ok {
		t.Fatalf("controller event provider is %T, want *events.Fake", h.cs.eventProv)
	}
	countEvents := func(eventType string) int {
		got, err := rec.List(events.Filter{Type: eventType})
		if err != nil {
			t.Fatalf("listing %s events: %v", eventType, err)
		}
		return len(got)
	}

	var stdout, stderr bytes.Buffer
	before := alive.reqs.Load()
	if code := cmdSuspend([]string{h.cityPath}, false, &stdout, &stderr); code != 0 {
		t.Fatalf("gc suspend exit %d, want 0; stderr: %s", code, stderr.String())
	}
	if alive.reqs.Load() == before {
		t.Fatal("gc suspend did not go through the controller's API")
	}
	st, err := suspensionstate.Load(fsys.OSFS{}, h.cityPath)
	if err != nil || !suspensionstate.IsCitySuspended(st) {
		t.Fatalf("suspension file after gc suspend: state %+v err %v, want the city suspended", st, err)
	}
	if n := countEvents(events.CitySuspended); n != 1 {
		t.Fatalf("controller recorded %d %s events, want 1", n, events.CitySuspended)
	}
	select {
	case <-h.cs.pokeCh:
	default:
		t.Fatal("the API suspend did not wake the controller")
	}

	stdout.Reset()
	stderr.Reset()
	before = alive.reqs.Load()
	if code := cmdResume([]string{h.cityPath}, false, &stdout, &stderr); code == 0 {
		t.Fatalf("gc resume through the API exit 0 over a city.toml that does not parse, want a refusal; stdout: %s", stdout.String())
	}
	if alive.reqs.Load() == before {
		t.Fatal("gc resume did not go through the controller's API")
	}
	st, err = suspensionstate.Load(fsys.OSFS{}, h.cityPath)
	if err != nil || !suspensionstate.IsCitySuspended(st) {
		t.Fatalf("suspension file after the refused resume: state %+v err %v, want the city still suspended", st, err)
	}
	if n := countEvents(events.CityResumed); n != 0 {
		t.Fatalf("controller recorded %d %s events after the refused resume, want 0", n, events.CityResumed)
	}
}
