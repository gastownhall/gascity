//go:build !windows

package session

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"strconv"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/pidutil"
	"github.com/gastownhall/gascity/internal/runtime"
)

// runtimeOwnerStarts are the two Manager verbs that resume a stopped session
// through the pre-start gate.
var runtimeOwnerStarts = []struct {
	name  string
	start func(t *testing.T, mgr *Manager, info Info) error
}{
	{"Start", func(_ *testing.T, mgr *Manager, info Info) error {
		return mgr.Start(context.Background(), testActor(mgr, ActorOperator), info.ID, BuildResumeCommand(info), runtime.Config{WorkDir: info.WorkDir})
	}},
	{"StartRuntimeOnly", func(t *testing.T, mgr *Manager, info Info) error {
		return startRuntimeOnly(t, mgr, info.ID, BuildResumeCommand(info), runtime.Config{WorkDir: info.WorkDir})
	}},
}

// seedRuntimeOwner writes a runtime_owner record as raw JSON, the way the
// row stores it.
func seedRuntimeOwner(t *testing.T, mgr *Manager, id string, uid int, boot string, roots ...map[string]any) {
	t.Helper()
	if roots == nil {
		roots = []map[string]any{}
	}
	raw, err := json.Marshal(map[string]any{"uid": uid, "boot": boot, "roots": roots})
	if err != nil {
		t.Fatal(err)
	}
	if err := mgr.store.SetMetadata(id, "runtime_owner", string(raw)); err != nil {
		t.Fatalf("seeding runtime_owner: %v", err)
	}
}

// liveRoot is the test process itself standing in for a runtime root another
// uid launched: it is alive and its identity can be verified unprivileged.
func liveRoot(t *testing.T) map[string]any {
	t.Helper()
	start, err := pidutil.StartTime(os.Getpid())
	if err != nil {
		t.Skipf("start time of the test process is unreadable here: %v", err)
	}
	return map[string]any{"pid": os.Getpid(), "start": start}
}

// AC-1: a start under euid B over a session whose runtime euid A launched in
// this boot, and whose recorded root is still alive at its recorded start
// time, refuses with ErrRuntimeUnavailable naming the session, uid A, the live
// pid and the remedy, and never reaches the provider's Start, even though B's
// process-table scan (which skips other uids' processes) finds no root.
func TestStartUnderAnotherUIDRefusesWhileRecordedRuntimeLives(t *testing.T) {
	for _, tc := range runtimeOwnerStarts {
		t.Run(tc.name, func(t *testing.T) {
			mgr, sp, info := seedSuspendedResumeTarget(t)
			otherUID := os.Geteuid() + 1
			seedRuntimeOwner(t, mgr, info.ID, otherUID, runtimeLeaseBootID(), liveRoot(t))

			err := tc.start(t, mgr, info)
			if !errors.Is(err, runtime.ErrRuntimeUnavailable) {
				t.Fatalf("%s error = %v, want it to wrap runtime.ErrRuntimeUnavailable", tc.name, err)
			}
			for _, want := range []string{info.ID, "uid " + strconv.Itoa(otherUID), strconv.Itoa(os.Getpid()), "--unset-metadata runtime_owner"} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("%s error = %v, want it to contain %q", tc.name, err, want)
				}
			}
			if hasEventPrefix(sp.events, "start:") || hasEventPrefix(sp.events, "terminate:") {
				t.Fatalf("events = %v, want no start and no terminate", sp.events)
			}
		})
	}
}
