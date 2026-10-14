package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/doctor"
	"github.com/gastownhall/gascity/internal/resilience"
)

// fleetControlCity makes a minimal city directory with a short path (unix
// socket limit), points GC_CITY at it, and publishes guard as its fleet
// breaker for the test's duration. A nil guard publishes nothing.
func fleetControlCity(t *testing.T, guard *fleetStartGuard) string {
	t.Helper()
	dir := shortSocketTempDir(t, "gc-fleet-breaker-")
	if err := os.MkdirAll(filepath.Join(dir, ".gc"), 0o755); err != nil {
		t.Fatalf("MkdirAll(.gc): %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "city.toml"), []byte("[workspace]\n"), 0o644); err != nil {
		t.Fatalf("WriteFile(city.toml): %v", err)
	}
	t.Setenv("GC_CITY", dir)
	publishFleetGuard(dir, guard)
	t.Cleanup(func() { publishFleetGuard(dir, nil) })
	return dir
}

// serveFleetBreakerSocket answers fleet breaker commands on the city's
// controller socket the way the controller loop does.
func serveFleetBreakerSocket(t *testing.T, cityDir string) {
	t.Helper()
	startFakeUnixSocket(t, controllerSocketPath(cityDir), func(line string) string {
		command, ok := strings.CutPrefix(line, fleetBreakerCommandPrefix)
		if !ok {
			return ""
		}
		server, client := net.Pipe()
		defer client.Close() //nolint:errcheck
		go func() {
			handleFleetBreakerSocketCmd(server, cityDir, command)
			server.Close() //nolint:errcheck
		}()
		reply, _ := io.ReadAll(client)
		return string(reply)
	})
}

// silentController accepts controller connections and never answers, as a
// wedged controller does.
func silentController(t *testing.T, cityDir string) {
	t.Helper()
	release := make(chan struct{})
	startFakeUnixSocket(t, controllerSocketPath(cityDir), func(string) string {
		<-release
		return ""
	})
	t.Cleanup(func() { close(release) })
}

// openFleetGuard returns a guard whose breaker is open.
func openFleetGuard(t *testing.T) *fleetStartGuard {
	t.Helper()
	g, clk := newFleetTestGuard(t)
	tripFleet(t, g, clk)
	return g
}

// fleetSocketReply runs handleFleetBreakerSocketCmd over a pipe.
func fleetSocketReply(t *testing.T, cityDir, command string) fleetBreakerReply {
	t.Helper()
	server, client := net.Pipe()
	defer client.Close() //nolint:errcheck
	go func() {
		handleFleetBreakerSocketCmd(server, cityDir, command)
		server.Close() //nolint:errcheck
	}()
	var reply fleetBreakerReply
	if err := json.NewDecoder(client).Decode(&reply); err != nil {
		t.Fatalf("decode %q reply: %v", command, err)
	}
	return reply
}

func TestFleetGuardDirectory_PublishAndLookup(t *testing.T) {
	dir := fleetControlCity(t, nil)
	if got := publishedFleetGuard(dir); got != nil {
		t.Fatalf("publishedFleetGuard before publishing = %v, want nil", got)
	}
	g, _ := newFleetTestGuard(t)
	publishFleetGuard(dir, g)
	if got := publishedFleetGuard(dir); got != g {
		t.Fatalf("publishedFleetGuard = %p, want the published guard %p", got, g)
	}
	if got := publishedFleetGuard(dir + "/"); got != g {
		t.Fatalf("publishedFleetGuard(%q) = %p, want the same city's guard (paths are normalized)", dir+"/", got)
	}
	publishFleetGuard(dir, nil)
	if got := publishedFleetGuard(dir); got != nil {
		t.Fatalf("publishedFleetGuard after publishing nil = %v, want the entry removed", got)
	}
}

func TestEnsureFleetStartGuard(t *testing.T) {
	off := false
	cr := &CityRuntime{cityPath: shortSocketTempDir(t, "gc-fleet-ensure-"), cfg: &config.City{}}
	t.Cleanup(func() { publishFleetGuard(cr.cityPath, nil) })

	g := cr.ensureFleetStartGuard()
	if g == nil {
		t.Fatal("ensureFleetStartGuard with the default config = nil, want a guard (the breaker is on by default)")
	}
	if got := publishedFleetGuard(cr.cityPath); got != g {
		t.Fatalf("published guard = %p, want the runtime's guard %p", got, g)
	}
	if again := cr.ensureFleetStartGuard(); again != g {
		t.Fatalf("second ensureFleetStartGuard = %p, want the same guard %p (state must survive ticks)", again, g)
	}

	cr.cfg = &config.City{Session: config.SessionConfig{StartupTimeout: "3m"}}
	rebuilt := cr.ensureFleetStartGuard()
	if rebuilt == nil || rebuilt == g {
		t.Fatalf("ensureFleetStartGuard after startup_timeout changed = %p (was %p), want a rebuilt guard", rebuilt, g)
	}
	if got := publishedFleetGuard(cr.cityPath); got != rebuilt {
		t.Fatalf("published guard after rebuild = %p, want %p", got, rebuilt)
	}

	cr.cfg = &config.City{Daemon: config.DaemonConfig{FleetStartBreaker: &off}}
	if got := cr.ensureFleetStartGuard(); got != nil {
		t.Fatalf("ensureFleetStartGuard with the kill switch off = %p, want nil", got)
	}
	if got := publishedFleetGuard(cr.cityPath); got != nil {
		t.Fatalf("published guard with the kill switch off = %p, want it withdrawn", got)
	}
}

func TestHandleFleetBreakerSocketCmd_Status(t *testing.T) {
	t.Run("no guard published", func(t *testing.T) {
		dir := fleetControlCity(t, nil)
		reply := fleetSocketReply(t, dir, fleetBreakerStatusCommand)
		if reply.Outcome != "ok" || reply.Enabled {
			t.Fatalf("status with no guard = %+v, want ok and disabled", reply)
		}
	})

	t.Run("open guard", func(t *testing.T) {
		g := openFleetGuard(t)
		dir := fleetControlCity(t, g)
		reply := fleetSocketReply(t, dir, fleetBreakerStatusCommand)
		if reply.Outcome != "ok" || !reply.Enabled || reply.State != resilience.StateOpen.String() {
			t.Fatalf("status of an open guard = %+v, want ok, enabled, open", reply)
		}
		if reply.Reason != fleetReasonFailures || reply.Trips != 1 || reply.NextProbeAt == "" || reply.OpenSince == "" || reply.BackoffCapMs <= 0 {
			t.Fatalf("status of an open guard = %+v, want reason %q, 1 trip, a next probe, an open-since and a backoff cap", reply, fleetReasonFailures)
		}
	})

	t.Run("unknown command", func(t *testing.T) {
		dir := fleetControlCity(t, nil)
		reply := fleetSocketReply(t, dir, "explode")
		if reply.Outcome != "failed" || !strings.Contains(reply.Error, `unknown fleet breaker command "explode"`) {
			t.Fatalf("unknown command reply = %+v, want a failed reply naming the command", reply)
		}
	})
}

func TestHandleFleetBreakerSocketCmd_ResetReportsThePostResetState(t *testing.T) {
	t.Run("open guard", func(t *testing.T) {
		g := openFleetGuard(t)
		dir := fleetControlCity(t, g)
		reply := fleetSocketReply(t, dir, fleetBreakerResetCommand)
		if reply.Outcome != "ok" || !reply.Enabled || !reply.WasOpen {
			t.Fatalf("reset of an open guard = %+v, want ok, enabled, was_open", reply)
		}
		if reply.State != resilience.StateClosed.String() {
			t.Fatalf("reset reply state = %q, want closed", reply.State)
		}
		if reply.Reason != "" || reply.NextProbeAt != "" || reply.OpenSince != "" || reply.Trips != 0 {
			t.Fatalf("reset reply = %+v, want the post-reset state: no reason, probe time, open-since or trips from the episode it just cleared", reply)
		}
		if got := fleetState(g); got != resilience.StateClosed {
			t.Fatalf("breaker after reset = %v, want closed", got)
		}
	})

	t.Run("already closed guard", func(t *testing.T) {
		g, _ := newFleetTestGuard(t)
		dir := fleetControlCity(t, g)
		reply := fleetSocketReply(t, dir, fleetBreakerResetCommand)
		if reply.Outcome != "ok" || !reply.Enabled || reply.WasOpen || reply.State != resilience.StateClosed.String() {
			t.Fatalf("reset of a closed guard = %+v, want ok, enabled, closed, not was_open", reply)
		}
	})

	t.Run("no guard published", func(t *testing.T) {
		dir := fleetControlCity(t, nil)
		reply := fleetSocketReply(t, dir, fleetBreakerResetCommand)
		if reply.Outcome != "ok" || reply.Enabled || reply.WasOpen {
			t.Fatalf("reset with no guard = %+v, want ok and disabled", reply)
		}
		if reply.State != "" {
			t.Fatalf("reset with no guard reports state %q, want none: there is no breaker to be closed", reply.State)
		}
	})
}

func TestFleetBreakerReplyDescribe(t *testing.T) {
	tests := []struct {
		name  string
		reply fleetBreakerReply
		want  []string // substrings; nil means the line is empty
	}{
		{"disabled", fleetBreakerReply{Outcome: "ok"}, nil},
		{"no state", fleetBreakerReply{Outcome: "ok", Enabled: true}, nil},
		{"closed", fleetBreakerReply{Outcome: "ok", Enabled: true, State: "closed"}, nil},
		{
			"open",
			fleetBreakerReply{Outcome: "ok", Enabled: true, State: "open", Reason: "failures", NextProbeAt: "2026-10-14T12:00:00Z"},
			[]string{"fleet start breaker open (failures)", "next probe 2026-10-14T12:00:00Z", "gc breaker reset"},
		},
		{
			"half-open without a probe time",
			fleetBreakerReply{Outcome: "ok", Enabled: true, State: "half-open", Reason: "state-cache"},
			[]string{"fleet start breaker half-open (state-cache)", "gc breaker reset"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := tc.reply.describe()
			if tc.want == nil {
				if got != "" {
					t.Fatalf("describe() = %q, want empty", got)
				}
				return
			}
			for _, sub := range tc.want {
				if !strings.Contains(got, sub) {
					t.Fatalf("describe() = %q, want it to contain %q", got, sub)
				}
			}
			if tc.reply.NextProbeAt == "" && strings.Contains(got, "next probe") {
				t.Fatalf("describe() = %q, must not name a probe time it does not have", got)
			}
		})
	}
}

func TestCmdBreakerStatus(t *testing.T) {
	t.Run("controller not running", func(t *testing.T) {
		fleetControlCity(t, nil)
		var stdout, stderr bytes.Buffer
		if rc := cmdBreakerStatus(false, &stdout, &stderr); rc != 1 {
			t.Fatalf("cmdBreakerStatus with no controller = %d, want 1", rc)
		}
		if !strings.Contains(stderr.String(), "gc breaker status:") {
			t.Fatalf("stderr = %q, want a gc breaker status error", stderr.String())
		}
	})

	t.Run("disabled", func(t *testing.T) {
		dir := fleetControlCity(t, nil)
		serveFleetBreakerSocket(t, dir)
		var stdout, stderr bytes.Buffer
		if rc := cmdBreakerStatus(false, &stdout, &stderr); rc != 0 {
			t.Fatalf("cmdBreakerStatus = %d (stderr %q), want 0", rc, stderr.String())
		}
		if !strings.Contains(stdout.String(), "disabled ([daemon] fleet_start_breaker = false)") {
			t.Fatalf("stdout = %q, want the kill-switch line", stdout.String())
		}
	})

	t.Run("closed", func(t *testing.T) {
		g, _ := newFleetTestGuard(t)
		dir := fleetControlCity(t, g)
		serveFleetBreakerSocket(t, dir)
		var stdout, stderr bytes.Buffer
		if rc := cmdBreakerStatus(false, &stdout, &stderr); rc != 0 {
			t.Fatalf("cmdBreakerStatus = %d (stderr %q), want 0", rc, stderr.String())
		}
		if got := stdout.String(); got != "Fleet start breaker: closed\n" {
			t.Fatalf("stdout = %q, want only the closed line", got)
		}
	})

	t.Run("open", func(t *testing.T) {
		dir := fleetControlCity(t, openFleetGuard(t))
		serveFleetBreakerSocket(t, dir)
		var stdout, stderr bytes.Buffer
		if rc := cmdBreakerStatus(false, &stdout, &stderr); rc != 0 {
			t.Fatalf("cmdBreakerStatus = %d (stderr %q), want 0", rc, stderr.String())
		}
		for _, want := range []string{"Fleet start breaker: open", "Reason: failures", "Open since: ", "Trips: 1 (backoff cap ", "Next probe: ", "probe in flight: false", "Clear with: gc breaker reset"} {
			if !strings.Contains(stdout.String(), want) {
				t.Fatalf("stdout = %q, want it to contain %q", stdout.String(), want)
			}
		}
	})

	t.Run("json", func(t *testing.T) {
		dir := fleetControlCity(t, openFleetGuard(t))
		serveFleetBreakerSocket(t, dir)
		var stdout, stderr bytes.Buffer
		if rc := cmdBreakerStatus(true, &stdout, &stderr); rc != 0 {
			t.Fatalf("cmdBreakerStatus(--json) = %d (stderr %q), want 0", rc, stderr.String())
		}
		var reply fleetBreakerReply
		if err := json.Unmarshal(stdout.Bytes(), &reply); err != nil {
			t.Fatalf("stdout %q is not the JSON reply: %v", stdout.String(), err)
		}
		if !reply.Enabled || reply.State != "open" {
			t.Fatalf("json reply = %+v, want enabled and open", reply)
		}
	})
}

func TestCmdBreakerReset(t *testing.T) {
	t.Run("controller not running", func(t *testing.T) {
		fleetControlCity(t, nil)
		var stdout, stderr bytes.Buffer
		if rc := cmdBreakerReset(&stdout, &stderr); rc != 1 {
			t.Fatalf("cmdBreakerReset with no controller = %d, want 1", rc)
		}
		if !strings.Contains(stderr.String(), "gc breaker reset:") {
			t.Fatalf("stderr = %q, want a gc breaker reset error", stderr.String())
		}
	})

	t.Run("disabled", func(t *testing.T) {
		dir := fleetControlCity(t, nil)
		serveFleetBreakerSocket(t, dir)
		var stdout, stderr bytes.Buffer
		if rc := cmdBreakerReset(&stdout, &stderr); rc != 0 {
			t.Fatalf("cmdBreakerReset = %d (stderr %q), want 0", rc, stderr.String())
		}
		if !strings.Contains(stdout.String(), "disabled; nothing to reset") {
			t.Fatalf("stdout = %q, want the disabled line", stdout.String())
		}
	})

	t.Run("already closed", func(t *testing.T) {
		g, _ := newFleetTestGuard(t)
		dir := fleetControlCity(t, g)
		serveFleetBreakerSocket(t, dir)
		var stdout, stderr bytes.Buffer
		if rc := cmdBreakerReset(&stdout, &stderr); rc != 0 {
			t.Fatalf("cmdBreakerReset = %d (stderr %q), want 0", rc, stderr.String())
		}
		if !strings.Contains(stdout.String(), "already closed") {
			t.Fatalf("stdout = %q, want the already-closed line", stdout.String())
		}
	})

	t.Run("open", func(t *testing.T) {
		g := openFleetGuard(t)
		dir := fleetControlCity(t, g)
		serveFleetBreakerSocket(t, dir)
		var stdout, stderr bytes.Buffer
		if rc := cmdBreakerReset(&stdout, &stderr); rc != 0 {
			t.Fatalf("cmdBreakerReset = %d (stderr %q), want 0", rc, stderr.String())
		}
		if !strings.Contains(stdout.String(), "reset: automatic session starts resumed") {
			t.Fatalf("stdout = %q, want the reset line", stdout.String())
		}
		if got := fleetState(g); got != resilience.StateClosed {
			t.Fatalf("breaker after gc breaker reset = %v, want closed", got)
		}
	})
}

func TestFleetBreakerLineAndNote(t *testing.T) {
	t.Run("line is empty while closed", func(t *testing.T) {
		g, _ := newFleetTestGuard(t)
		dir := fleetControlCity(t, g)
		serveFleetBreakerSocket(t, dir)
		line, err := fleetBreakerLine(dir)
		if err != nil || line != "" {
			t.Fatalf("fleetBreakerLine of a closed breaker = (%q, %v), want empty", line, err)
		}
	})

	t.Run("line describes an open breaker", func(t *testing.T) {
		dir := fleetControlCity(t, openFleetGuard(t))
		serveFleetBreakerSocket(t, dir)
		line, err := fleetBreakerLine(dir)
		if err != nil || !strings.Contains(line, "fleet start breaker open (failures)") {
			t.Fatalf("fleetBreakerLine of an open breaker = (%q, %v), want its description", line, err)
		}
		if note := fleetBreakerNote(ControllerJSON{Running: true}, dir); note != line {
			t.Fatalf("fleetBreakerNote = %q, want %q", note, line)
		}
	})

	t.Run("line reports an unreachable controller", func(t *testing.T) {
		dir := fleetControlCity(t, nil)
		if line, err := fleetBreakerLine(dir); err == nil || line != "" {
			t.Fatalf("fleetBreakerLine with no controller = (%q, %v), want an error", line, err)
		}
	})

	t.Run("note adds nothing without a running controller", func(t *testing.T) {
		dir := fleetControlCity(t, openFleetGuard(t))
		serveFleetBreakerSocket(t, dir)
		if note := fleetBreakerNote(ControllerJSON{Running: false}, dir); note != "" {
			t.Fatalf("fleetBreakerNote of a stopped controller = %q, want empty", note)
		}
		if note := fleetBreakerNote(ControllerJSON{Running: true}, ""); note != "" {
			t.Fatalf("fleetBreakerNote with no city path = %q, want empty", note)
		}
	})

	t.Run("note adds nothing when the controller cannot answer", func(t *testing.T) {
		dir := fleetControlCity(t, nil)
		if note := fleetBreakerNote(ControllerJSON{Running: true}, dir); note != "" {
			t.Fatalf("fleetBreakerNote with no listener = %q, want empty", note)
		}
	})
}

// gc status polls the breaker as a courtesy line; a controller that accepts
// the connection and never answers must cost it seconds, not the 95 s a
// controller command may legitimately take.
func TestFleetBreakerNote_WedgedControllerDoesNotHangStatus(t *testing.T) {
	dir := fleetControlCity(t, nil)
	silentController(t, dir)
	done := make(chan string, 1)
	go func() { done <- fleetBreakerNote(ControllerJSON{Running: true}, dir) }()
	select {
	case note := <-done:
		if note != "" {
			t.Fatalf("fleetBreakerNote from a silent controller = %q, want empty", note)
		}
	case <-time.After(8 * time.Second):
		t.Fatal("fleetBreakerNote still waiting on a silent controller after 8s: gc status would hang on it")
	}
}

func TestFleetBreakerDoctorCheck_Run(t *testing.T) {
	off := false
	reply := func(state, reason string) func(string) (fleetBreakerReply, error) {
		return func(string) (fleetBreakerReply, error) {
			return fleetBreakerReply{Outcome: "ok", Enabled: true, State: state, Reason: reason}, nil
		}
	}
	ctx := &doctor.CheckContext{CityPath: "/city"}

	t.Run("kill switch off", func(t *testing.T) {
		c := &fleetBreakerDoctorCheck{cfg: &config.City{Daemon: config.DaemonConfig{FleetStartBreaker: &off}}, query: func(string) (fleetBreakerReply, error) {
			t.Fatal("the check queried the controller although the breaker is disabled")
			return fleetBreakerReply{}, nil
		}}
		r := c.Run(ctx)
		if r.Status != doctor.StatusOK || !strings.Contains(r.Message, "disabled") {
			t.Fatalf("Run = %+v, want OK and disabled", r)
		}
	})

	t.Run("no city path", func(t *testing.T) {
		c := &fleetBreakerDoctorCheck{query: func(string) (fleetBreakerReply, error) {
			t.Fatal("the check queried the controller without a city")
			return fleetBreakerReply{}, nil
		}}
		for _, cc := range []*doctor.CheckContext{nil, {}} {
			if r := c.Run(cc); r.Status != doctor.StatusOK || !strings.Contains(r.Message, "not checked") {
				t.Fatalf("Run(%+v) = %+v, want OK and not checked", cc, r)
			}
		}
	})

	t.Run("controller unreachable", func(t *testing.T) {
		c := &fleetBreakerDoctorCheck{query: func(string) (fleetBreakerReply, error) {
			return fleetBreakerReply{}, os.ErrNotExist
		}}
		r := c.Run(ctx)
		if r.Status != doctor.StatusOK || !strings.Contains(r.Message, "controller not reachable") || len(r.Details) != 1 {
			t.Fatalf("Run = %+v, want OK, not reachable, with the error as a detail (other checks own the controller)", r)
		}
	})

	t.Run("open breaker warns", func(t *testing.T) {
		c := &fleetBreakerDoctorCheck{query: reply("open", "failures")}
		r := c.Run(ctx)
		if r.Status != doctor.StatusWarning || !strings.Contains(r.Message, "fleet start breaker open (failures)") || !strings.Contains(r.FixHint, "gc breaker reset") {
			t.Fatalf("Run = %+v, want a warning with a reset hint", r)
		}
	})

	t.Run("closed breaker is fine", func(t *testing.T) {
		c := &fleetBreakerDoctorCheck{query: reply("closed", "")}
		r := c.Run(ctx)
		if r.Status != doctor.StatusOK || r.Message != "fleet start breaker: closed" {
			t.Fatalf("Run = %+v, want OK and closed", r)
		}
	})

	t.Run("static surface", func(t *testing.T) {
		c := newFleetBreakerDoctorCheck(nil)
		if c.Name() != "fleet-start-breaker" || c.CanFix() || c.WarmupEligible() || c.Fix(nil) != nil {
			t.Fatalf("check surface = (%q, canfix %v, warmup %v), want fleet-start-breaker, no fix, not warmup eligible", c.Name(), c.CanFix(), c.WarmupEligible())
		}
	})
}
