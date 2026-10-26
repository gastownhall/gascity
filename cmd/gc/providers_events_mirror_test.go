package main

import (
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/events"
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
