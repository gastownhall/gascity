package session

import (
	"errors"
	"testing"

	"github.com/gastownhall/gascity/internal/runtime"
)

type failingMetaProvider struct {
	runtime.Provider
	failKey string
}

func (p failingMetaProvider) SetMeta(name, key, value string) error {
	if key == p.failKey {
		return errors.New("set denied")
	}
	return p.Provider.SetMeta(name, key, value)
}

func TestRuntimeEnvWithSessionContextAlignsAgentAndBeadsActor(t *testing.T) {
	for _, tc := range []struct {
		name                 string
		alias                string
		configuredIdentity   string
		sessionName          string
		persistedSessionName string
		want                 string
	}{
		{name: "canonical alias", alias: "rig/worker", sessionName: "rig--worker", persistedSessionName: "rig--worker", want: "rig/worker"},
		{name: "configured named identity fallback", configuredIdentity: "rig/worker", sessionName: "rig--worker", persistedSessionName: "rig--worker", want: "rig/worker"},
		{name: "session name fallback", sessionName: "rig--worker", persistedSessionName: "rig--worker", want: "rig--worker"},
		{name: "bead id fallback", sessionName: "s-session-id", want: "session-id"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			info := Info{
				ID:                      "session-id",
				SessionName:             tc.sessionName,
				SessionNameMetadata:     tc.persistedSessionName,
				Alias:                   tc.alias,
				ConfiguredNamedIdentity: tc.configuredIdentity,
				Template:                "rig/template",
				SessionOrigin:           "ephemeral",
			}
			env := RuntimeEnvWithSessionContext(
				info,
				DefaultGeneration,
				DefaultContinuationEpoch,
				"instance-token",
			)

			if got := env["GC_SESSION_NAME"]; got != tc.sessionName {
				t.Fatalf("GC_SESSION_NAME = %q, want %q", got, tc.sessionName)
			}
			for _, key := range []string{"GC_AGENT", "BEADS_ACTOR"} {
				if got := env[key]; got != tc.want {
					t.Fatalf("%s = %q, want %q", key, got, tc.want)
				}
			}
			if got := AssigneeIdentifier(info); got != env["BEADS_ACTOR"] {
				t.Fatalf("AssigneeIdentifier = %q, BEADS_ACTOR = %q", got, env["BEADS_ACTOR"])
			}
		})
	}
}

func TestSyncRuntimeAliasAlignsOwnershipMetadata(t *testing.T) {
	sp := runtime.NewFake()
	assertMeta := func(sessionName, key, want string) {
		t.Helper()
		got, err := sp.GetMeta(sessionName, key)
		if err != nil {
			t.Fatalf("GetMeta(%s): %v", key, err)
		}
		if got != want {
			t.Fatalf("%s = %q, want %q", key, got, want)
		}
	}

	info := Info{ID: "session-id", SessionName: "rig--worker", SessionNameMetadata: "rig--worker", Alias: "rig/worker"}
	if err := SyncRuntimeAlias(sp, info); err != nil {
		t.Fatalf("SyncRuntimeAlias(set): %v", err)
	}
	for _, key := range []string{"GC_ALIAS", "GC_AGENT", "BEADS_ACTOR"} {
		assertMeta(info.SessionName, key, "rig/worker")
	}

	info.Alias = ""
	if err := SyncRuntimeAlias(sp, info); err != nil {
		t.Fatalf("SyncRuntimeAlias(clear): %v", err)
	}
	assertMeta(info.SessionName, "GC_ALIAS", "")
	for _, key := range []string{"GC_AGENT", "BEADS_ACTOR"} {
		assertMeta(info.SessionName, key, "rig--worker")
	}

	repairInfo := Info{ID: "repair-id", SessionName: "s-repair-id"}
	if err := SyncRuntimeAlias(sp, repairInfo); err != nil {
		t.Fatalf("SyncRuntimeAlias(repair): %v", err)
	}
	assertMeta(repairInfo.SessionName, "GC_ALIAS", "")
	for _, key := range []string{"GC_AGENT", "BEADS_ACTOR"} {
		assertMeta(repairInfo.SessionName, key, "repair-id")
	}
}

func TestSyncRuntimeAliasRollsBackOnPartialFailure(t *testing.T) {
	base := runtime.NewFake()
	for _, key := range []string{"GC_ALIAS", "GC_AGENT", "BEADS_ACTOR"} {
		if err := base.SetMeta("rig--worker", key, "old-alias"); err != nil {
			t.Fatalf("seed %s: %v", key, err)
		}
	}
	sp := failingMetaProvider{Provider: base, failKey: "BEADS_ACTOR"}
	info := Info{ID: "session-id", SessionName: "rig--worker", SessionNameMetadata: "rig--worker", Alias: "new-alias"}

	if err := SyncRuntimeAlias(sp, info); err == nil {
		t.Fatal("SyncRuntimeAlias() error = nil, want BEADS_ACTOR failure")
	}
	for _, key := range []string{"GC_ALIAS", "GC_AGENT", "BEADS_ACTOR"} {
		got, err := base.GetMeta("rig--worker", key)
		if err != nil {
			t.Fatalf("GetMeta(%s): %v", key, err)
		}
		if got != "old-alias" {
			t.Fatalf("%s after rollback = %q, want old-alias", key, got)
		}
	}
}
