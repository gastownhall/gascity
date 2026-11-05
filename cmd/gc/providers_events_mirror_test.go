package main

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/supervisor"
)

func TestEventsMirrorOptionsUnsetAddsNothing(t *testing.T) {
	var stderr strings.Builder
	if _, ok := eventsMirrorOptions(config.EventsMirrorConfig{}, &stderr); ok {
		t.Fatal("eventsMirrorOptions enabled a mirror with no path")
	}
	if got := len(eventsFileRecorderOptions(config.EventsConfig{}, &stderr)); got != 4 {
		t.Fatalf("eventsFileRecorderOptions returned %d options without a mirror, want the 4 rotation options", got)
	}
	if stderr.String() != "" {
		t.Fatalf("stderr = %q, want empty", stderr.String())
	}
}

func TestEventsMirrorOptionsFromConfig(t *testing.T) {
	maxSize := int64(4096)
	cases := []struct {
		name string
		cfg  config.EventsMirrorConfig
		want events.MirrorOptions
	}{
		{
			name: "defaults",
			cfg:  config.EventsMirrorConfig{Path: "/var/log/gascity/hq/events.jsonl"},
			want: events.MirrorOptions{Path: "/var/log/gascity/hq/events.jsonl", Mode: 0o640, MaxSize: config.DefaultEventsMirrorMaxSizeBytes},
		},
		{
			name: "configured",
			cfg:  config.EventsMirrorConfig{Path: "/var/log/gascity/hq/events.jsonl", Mode: "0600", MaxSizeBytes: &maxSize},
			want: events.MirrorOptions{Path: "/var/log/gascity/hq/events.jsonl", Mode: 0o600, MaxSize: 4096},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := eventsMirrorOptions(tc.cfg, os.Stderr)
			if !ok {
				t.Fatal("eventsMirrorOptions disabled a valid mirror")
			}
			if got.Path != tc.want.Path || got.Mode != tc.want.Mode || got.MaxSize != tc.want.MaxSize {
				t.Fatalf("eventsMirrorOptions = %+v, want %+v", got, tc.want)
			}
		})
	}
}

func TestEventsMirrorOptionsWarnAndDisableOnBadConfig(t *testing.T) {
	negative := int64(-1)
	for name, cfg := range map[string]config.EventsMirrorConfig{
		"relative path": {Path: "log/events.jsonl"},
		"bad mode":      {Path: "/var/log/gascity/hq/events.jsonl", Mode: "rw-r-----"},
		"mode too wide": {Path: "/var/log/gascity/hq/events.jsonl", Mode: "04750"},
		"negative size": {Path: "/var/log/gascity/hq/events.jsonl", MaxSizeBytes: &negative},
	} {
		t.Run(name, func(t *testing.T) {
			var stderr strings.Builder
			if _, ok := eventsMirrorOptions(cfg, &stderr); ok {
				t.Fatal("eventsMirrorOptions enabled an unusable mirror")
			}
			if !strings.Contains(stderr.String(), "events.mirror: warning:") {
				t.Fatalf("stderr = %q, want an events.mirror warning", stderr.String())
			}
		})
	}
}

// mirrorCity makes a city directory with a .gc and an operator-provisioned
// mirror directory beside it, and returns the city path and mirror file path.
func mirrorCity(t *testing.T) (cityDir, mirrorPath string) {
	t.Helper()
	root := t.TempDir()
	cityDir = filepath.Join(root, "city")
	if err := os.MkdirAll(filepath.Join(cityDir, ".gc"), 0o755); err != nil {
		t.Fatal(err)
	}
	mirrorDir := filepath.Join(root, "var-log-gascity", "city")
	if err := os.MkdirAll(mirrorDir, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(mirrorDir, 0o750|os.ModeSetgid); err != nil {
		t.Fatal(err)
	}
	return cityDir, filepath.Join(mirrorDir, "events.jsonl")
}

// observeEventsMirror routes the mirror copier's progress to the returned
// channel for the rest of the test.
func observeEventsMirror(t *testing.T) <-chan events.MirrorProgress {
	t.Helper()
	ch := make(chan events.MirrorProgress, 1024)
	prev := eventsMirrorObserve
	eventsMirrorObserve = func(step events.MirrorProgress) {
		select {
		case ch <- step:
		default: // never block the copier
		}
	}
	t.Cleanup(func() { eventsMirrorObserve = prev })
	return ch
}

// waitMirrored waits for the copier to append the next event, then returns
// the mirror file, which must contain want.
func waitMirrored(t *testing.T, steps <-chan events.MirrorProgress, path, want string) string {
	t.Helper()
	deadline := time.NewTimer(15 * time.Second)
	defer deadline.Stop()
	for {
		select {
		case step := <-steps:
			if step.Kind != events.MirrorWrote {
				continue
			}
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(string(data), want) {
				t.Fatalf("mirror %s lacks %q after seq %d:\n%s", path, want, step.Seq, data)
			}
			return string(data)
		case <-deadline.C:
			t.Fatalf("the mirror copier never wrote %q", want)
		}
	}
}

func TestEventsMirrorWrittenOnlyByRotationOwner(t *testing.T) {
	cityDir, mirrorPath := mirrorCity(t)
	cfg := config.EventsConfig{Mirror: config.EventsMirrorConfig{Path: mirrorPath}}

	// A one-shot gc start is a secondary writer: it never mirrors, even with
	// the mirror configured.
	secondary, err := openStandaloneCityEventsRecorder(cityDir, cfg, false, os.Stderr)
	if err != nil {
		t.Fatal(err)
	}
	secondary.Record(events.Event{Type: "test.secondary", Actor: "tester"})
	_ = secondary.Close()
	if _, err := os.Stat(mirrorPath); !os.IsNotExist(err) {
		t.Fatalf("a secondary writer created the mirror (stat err %v)", err)
	}

	steps := observeEventsMirror(t)
	owner, err := openSupervisorCityEventsRecorder(cityDir, cfg, os.Stderr)
	if err != nil {
		t.Fatal(err)
	}
	defer owner.Close() //nolint:errcheck
	owner.Record(events.Event{Type: "test.owner", Actor: "tester"})
	got := waitMirrored(t, steps, mirrorPath, `"type":"test.owner"`)
	if strings.Contains(got, "test.secondary") {
		t.Fatalf("mirror replayed history from before it was enabled:\n%s", got)
	}
}

func TestEnforceGCPermissionsLeavesEventsMirrorAlone(t *testing.T) {
	cityDir, mirrorPath := mirrorCity(t)
	mirrorDir := filepath.Dir(mirrorPath)
	cfg := config.EventsConfig{Mirror: config.EventsMirrorConfig{Path: mirrorPath}}

	steps := observeEventsMirror(t)
	owner, err := openStandaloneCityEventsRecorder(cityDir, cfg, true, os.Stderr)
	if err != nil {
		t.Fatal(err)
	}
	defer owner.Close() //nolint:errcheck
	owner.Record(events.Event{Type: "test.perm", Actor: "tester"})
	waitMirrored(t, steps, mirrorPath, `"type":"test.perm"`)

	enforceGCPermissions(cityDir, os.Stderr)

	info, err := os.Stat(filepath.Join(cityDir, ".gc"))
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != gcDirPerm {
		t.Fatalf(".gc mode = %#o, want %#o", got, gcDirPerm)
	}
	info, err = os.Stat(mirrorDir)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := info.Mode()&(os.ModePerm|os.ModeSetgid), os.FileMode(0o750)|os.ModeSetgid; got != want {
		t.Fatalf("mirror dir mode = %v, want %v (untouched)", got, want)
	}
	info, err = os.Stat(mirrorPath)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != config.DefaultEventsMirrorMode {
		t.Fatalf("mirror file mode = %#o, want %#o", got, config.DefaultEventsMirrorMode)
	}

	// The owner keeps mirroring after .gc is tightened.
	owner.Record(events.Event{Type: "test.after-enforce", Actor: "tester"})
	waitMirrored(t, steps, mirrorPath, `"type":"test.after-enforce"`)
}

// TestEventsMirrorInheritsSetgidGroup checks the documented operator setup: a
// setgid mirror directory owned by the shipper's group hands that group to the
// mirror file, without gc chowning anything.
func TestEventsMirrorInheritsSetgidGroup(t *testing.T) {
	gid := otherGroupForTest(t)
	cityDir, mirrorPath := mirrorCity(t)
	if err := os.Chown(filepath.Dir(mirrorPath), -1, gid); err != nil {
		t.Skipf("cannot give the mirror dir group %d: %v", gid, err)
	}
	// chown can clear the setgid bit; restore the provisioned mode.
	if err := os.Chmod(filepath.Dir(mirrorPath), 0o750|os.ModeSetgid); err != nil {
		t.Fatal(err)
	}
	cfg := config.EventsConfig{Mirror: config.EventsMirrorConfig{Path: mirrorPath}}
	steps := observeEventsMirror(t)
	owner, err := openSupervisorCityEventsRecorder(cityDir, cfg, os.Stderr)
	if err != nil {
		t.Fatal(err)
	}
	defer owner.Close() //nolint:errcheck
	owner.Record(events.Event{Type: "test.group", Actor: "tester"})
	waitMirrored(t, steps, mirrorPath, `"type":"test.group"`)
	info, err := os.Stat(mirrorPath)
	if err != nil {
		t.Fatal(err)
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		t.Skip("no Stat_t on this platform")
	}
	if int(st.Gid) != gid {
		t.Fatalf("mirror file group = %d, want the directory's group %d", st.Gid, gid)
	}
}

// otherGroupForTest returns a group other than the process's effective group
// that this process may chown a directory it owns to, or skips.
func otherGroupForTest(t *testing.T) int {
	t.Helper()
	egid := os.Getegid()
	if os.Geteuid() == 0 {
		return egid + 1
	}
	groups, err := os.Getgroups()
	if err != nil {
		t.Skipf("getgroups: %v", err)
	}
	for _, g := range groups {
		if g != egid {
			return g
		}
	}
	t.Skip("process has no supplementary group to test setgid inheritance with")
	return 0
}

// TestSupervisorClosesTheCityRecorderWhenTheRuntimeFails pins that a city
// whose runtime cannot be built (city_runtime_failed) closes the recorder it
// opened. Left open, that rotation owner's [events.mirror] copier would hold
// the mirror lock for the life of the supervisor, and every retry's copier,
// including the one that would apply a corrected [events.mirror], would wait
// on it.
func TestSupervisorClosesTheCityRecorderWhenTheRuntimeFails(t *testing.T) {
	reg, cityPath := newWedgedInitCity(t)
	mirrorPath := filepath.Join(t.TempDir(), "events.jsonl")
	cityTOML, err := os.OpenFile(filepath.Join(cityPath, "city.toml"), os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fmt.Fprintf(cityTOML, "\n[events.mirror]\npath = %q\n", mirrorPath); err != nil {
		t.Fatal(err)
	}
	if err := cityTOML.Close(); err != nil {
		t.Fatal(err)
	}
	steps := observeEventsMirror(t)

	cr := newCityRegistry()
	var stdout, stderr bytes.Buffer
	reconcileCities(context.Background(), reg, cr, supervisor.PublicationConfig{}, &stdout, &stderr)
	if count := initFailureCount(cr, canonicalTestPath(cityPath)); count != 1 {
		t.Fatalf("init failure count = %d, want 1: the city must fail inside newCityRuntime (stderr: %s)", count, stderr.String())
	}

	// Close waits for the copier to exit, so a closed recorder's copier has
	// already reported its last step.
	requireMirrorStopped(t, steps, "the failed city's", &stderr)
}

// TestSupervisorClosesTheRecorderOfACityCanceledInPlace pins that a city
// canceled in place, as its controller socket's stop and stop-force cancel it,
// closes its recorder when it exits. No stop removed such a city from the
// registry, so none closes the recorder through mc.closer; left open, its
// [events.mirror] copier would hold the mirror lock for the life of the
// supervisor, and the restarted city's copier would wait on it.
func TestSupervisorClosesTheRecorderOfACityCanceledInPlace(t *testing.T) {
	t.Setenv("GC_HOME", t.TempDir())
	cityPath := shortSocketTempDir(t, "gc-mirror-city-")
	cleanupManagedDoltTestCity(t, cityPath)
	if err := os.MkdirAll(filepath.Join(cityPath, ".gc"), 0o755); err != nil {
		t.Fatal(err)
	}
	mirrorPath := filepath.Join(t.TempDir(), "events.jsonl")
	cityToml := fmt.Sprintf(`[workspace]
name = "mirror-city"

[orders]
skip = ["beads-health", "cross-rig-deps", "gate-sweep", "jsonl-export", "reaper", "order-tracking-sweep", "orphan-sweep", "prune-branches", "spawn-storm-detect", "wisp-compact"]

[session]
provider = "fake"

[daemon]
shutdown_timeout = "100ms"

[events.mirror]
path = %q
`, mirrorPath)
	if err := os.WriteFile(filepath.Join(cityPath, "city.toml"), []byte(cityToml), 0o644); err != nil {
		t.Fatal(err)
	}
	script := writeSpyScript(t, filepath.Join(t.TempDir(), "ops.log"))
	t.Setenv("GC_BEADS", "exec:"+script)
	t.Setenv("GC_BEADS_SCOPE_ROOT", cityPath)
	reg := supervisor.NewRegistry(supervisor.RegistryPath())
	if err := reg.Register(cityPath, "mirror-city"); err != nil {
		t.Fatal(err)
	}
	steps := observeEventsMirror(t)

	cr := newCityRegistry()
	path := canonicalTestPath(cityPath)
	// The started city keeps logging while this test reads.
	var stdout, stderr lockedBuffer
	reconcileCities(context.Background(), reg, cr, supervisor.PublicationConfig{}, &stdout, &stderr)
	t.Cleanup(func() {
		if done := cr.CancelCity(path); done != nil {
			select {
			case <-done:
			case <-time.After(15 * time.Second):
				t.Error("city goroutine did not exit in time")
			}
		}
	})
	waitMirrorWatching(t, steps, &stderr)

	done := cr.CancelCity(path)
	if done == nil {
		t.Fatalf("the city is not running (stderr: %s)", stderr.String())
	}
	select {
	case <-done:
	case <-time.After(15 * time.Second):
		t.Fatalf("the city canceled in place did not exit (stderr: %s)", stderr.String())
	}
	// The city closes its recorder before it reports done.
	requireMirrorStopped(t, steps, "the canceled city's", &stderr)

	// The city is still registered, so the next reconcile restarts it, and
	// its copier takes the mirror lock instead of waiting on it.
	reconcileCities(context.Background(), reg, cr, supervisor.PublicationConfig{}, &stdout, &stderr)
	waitMirrorWatching(t, steps, &stderr)
}

// requireMirrorStopped fails unless a mirror copier has already reported its
// last step, MirrorStopped. whose names the copier's city in the failure.
func requireMirrorStopped(t *testing.T, steps <-chan events.MirrorProgress, whose string, stderr fmt.Stringer) {
	t.Helper()
	var seen []events.MirrorProgressKind
	for {
		select {
		case step := <-steps:
			if step.Kind == events.MirrorStopped {
				return
			}
			seen = append(seen, step.Kind)
		default:
			t.Fatalf("%s mirror copier is still running (steps so far %v): its recorder was never closed\nstderr: %s", whose, seen, stderr)
		}
	}
}

// waitMirrorWatching waits for a mirror copier to start tailing the canonical
// log, failing if one reports a failure first.
func waitMirrorWatching(t *testing.T, steps <-chan events.MirrorProgress, stderr fmt.Stringer) {
	t.Helper()
	deadline := time.NewTimer(15 * time.Second)
	defer deadline.Stop()
	for {
		select {
		case step := <-steps:
			switch step.Kind {
			case events.MirrorWatching:
				return
			case events.MirrorFailed:
				t.Fatalf("the mirror copier failed: %v\nstderr: %s", step.Err, stderr)
			}
		case <-deadline.C:
			t.Fatalf("no mirror copier started tailing the event log\nstderr: %s", stderr)
		}
	}
}
