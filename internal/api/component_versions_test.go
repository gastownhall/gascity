package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/gastownhall/gascity/internal/config"
)

func TestProbeComponentVersionsParsesBothVersions(t *testing.T) {
	got := probeComponentVersions(
		func() (string, error) { return "2.0.7", nil },
		func() (string, error) { return "1.0.4", nil },
	)
	if got.Dolt != "2.0.7" {
		t.Errorf("Dolt = %q, want 2.0.7", got.Dolt)
	}
	if got.Beads != "1.0.4" {
		t.Errorf("Beads = %q, want 1.0.4", got.Beads)
	}
}

func TestProbeComponentVersionsOmitsAndLogsOnFailure(t *testing.T) {
	var buf bytes.Buffer
	defer captureLog(t, &buf)()

	got := probeComponentVersions(
		func() (string, error) { return "", fmt.Errorf("dolt: executable file not found") },
		func() (string, error) { return "1.0.4", nil },
	)
	if got.Dolt != "" {
		t.Errorf("Dolt = %q, want empty on probe failure", got.Dolt)
	}
	if got.Beads != "1.0.4" {
		t.Errorf("Beads = %q, want 1.0.4", got.Beads)
	}
	if !strings.Contains(buf.String(), "dolt version probe failed") {
		t.Errorf("expected dolt probe failure to be logged, got %q", buf.String())
	}
}

func TestResolveComponentVersionsProbesOnce(t *testing.T) {
	var calls int32
	s := &Server{componentVersionsProbe: func() componentVersions {
		atomic.AddInt32(&calls, 1)
		return componentVersions{Dolt: "2.0.7", Beads: "1.0.4"}
	}}
	for i := 0; i < 3; i++ {
		got := s.resolveComponentVersions()
		if got.Dolt != "2.0.7" || got.Beads != "1.0.4" {
			t.Fatalf("resolveComponentVersions() = %+v", got)
		}
	}
	if n := atomic.LoadInt32(&calls); n != 1 {
		t.Errorf("probe called %d times, want 1 (cached for process lifetime)", n)
	}
}

func TestBuildStatusBodyIncludesComponentVersions(t *testing.T) {
	state := newFakeState(t)
	s := &Server{state: state, componentVersionsProbe: func() componentVersions {
		return componentVersions{Dolt: "2.0.7", Beads: "1.0.4"}
	}}

	body := s.buildStatusBody(context.Background(), false)
	if body.DoltVersion != "2.0.7" {
		t.Errorf("DoltVersion = %q, want 2.0.7", body.DoltVersion)
	}
	if body.BeadsVersion != "1.0.4" {
		t.Errorf("BeadsVersion = %q, want 1.0.4", body.BeadsVersion)
	}
}

func TestStatusBodyOmitsEmptyComponentVersions(t *testing.T) {
	out, err := json.Marshal(StatusBody{Name: "c"})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	s := string(out)
	if strings.Contains(s, "dolt_version") {
		t.Errorf("dolt_version present for empty value; want omitted: %s", s)
	}
	if strings.Contains(s, "beads_version") {
		t.Errorf("beads_version present for empty value; want omitted: %s", s)
	}
}

// TestCityPinnedBDBin is the G4 (native-program) table test for resolving a
// city's workspace.env BD_BIN pin for the status probe — the fix for
// ProbeBDVersion reporting whichever "bd" happens to be first on the
// supervisor process's PATH instead of the binary the city is actually pinned
// to (the same class of blind spot G4 closed in the native-store preflight's
// version_compat check).
func TestCityPinnedBDBin(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("fake binary script uses a POSIX shebang")
	}
	dir := t.TempDir()
	fakeBD := filepath.Join(dir, "fake-bd")
	if err := os.WriteFile(fakeBD, []byte("#!/bin/sh\necho ok\n"), 0o755); err != nil { //nolint:gosec // test fixture, deliberately executable
		t.Fatalf("write fake bd: %v", err)
	}
	relative := "fake-bd"

	tests := []struct {
		name string
		cfg  *config.City
		want string
	}{
		{"nil config — no pin", nil, ""},
		{"no BD_BIN configured — no pin", &config.City{Workspace: config.Workspace{}}, ""},
		{"empty BD_BIN — no pin", &config.City{Workspace: config.Workspace{Env: map[string]string{"BD_BIN": "  "}}}, ""},
		{"relative BD_BIN — not an absolute pin, refused", &config.City{Workspace: config.Workspace{Env: map[string]string{"BD_BIN": relative}}}, ""},
		{"nonexistent absolute BD_BIN — not resolvable, refused", &config.City{Workspace: config.Workspace{Env: map[string]string{"BD_BIN": filepath.Join(dir, "does-not-exist")}}}, ""},
		{"valid absolute executable BD_BIN — pinned", &config.City{Workspace: config.Workspace{Env: map[string]string{"BD_BIN": fakeBD}}}, fakeBD},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Isolate from any ambient BD_BIN the test runner's own shell
			// carries: cityPinnedBDBin now falls back to it (mirroring
			// workspacePinnedBdBinaryOptional), and these cases are about
			// the workspace.env sources only.
			t.Setenv("BD_BIN", "")
			if got := cityPinnedBDBin(tt.cfg); got != tt.want {
				t.Errorf("cityPinnedBDBin() = %q, want %q", got, tt.want)
			}
		})
	}
}

// TestCityPinnedBDBinWarnsOnInvalidButSetPin is the MED4 fix: a workspace.env
// BD_BIN that is set but cannot be resolved (relative, or absolute-but-not-an-
// executable) must log a warning naming the bad value, not merely return ""
// as if no pin were configured at all — an operator debugging "why is status
// reporting the wrong bd version" needs the line.
func TestCityPinnedBDBinWarnsOnInvalidButSetPin(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("fake binary script uses a POSIX shebang")
	}
	t.Setenv("BD_BIN", "")
	dir := t.TempDir()

	t.Run("relative pin warns", func(t *testing.T) {
		var buf bytes.Buffer
		defer captureLog(t, &buf)()
		cfg := &config.City{Workspace: config.Workspace{Env: map[string]string{"BD_BIN": "fake-bd"}}}
		if got := cityPinnedBDBin(cfg); got != "" {
			t.Errorf("cityPinnedBDBin() = %q, want empty", got)
		}
		if !strings.Contains(buf.String(), "fake-bd") || !strings.Contains(buf.String(), "BD_BIN") {
			t.Errorf("expected a warning naming the bad workspace.env BD_BIN pin, got %q", buf.String())
		}
	})

	t.Run("nonexistent absolute pin warns", func(t *testing.T) {
		var buf bytes.Buffer
		defer captureLog(t, &buf)()
		missing := filepath.Join(dir, "does-not-exist")
		cfg := &config.City{Workspace: config.Workspace{Env: map[string]string{"BD_BIN": missing}}}
		if got := cityPinnedBDBin(cfg); got != "" {
			t.Errorf("cityPinnedBDBin() = %q, want empty", got)
		}
		if !strings.Contains(buf.String(), missing) {
			t.Errorf("expected a warning naming the unresolvable workspace.env BD_BIN pin, got %q", buf.String())
		}
	})
}

// TestCityPinnedBDBinWorkspacePATHSearch covers the second resolution source
// workspacePinnedBdBinaryOptional defines: an explicit workspace.env PATH
// (not BD_BIN) that names a directory containing "bd". A workspace.env PATH
// that does NOT resolve bd is authoritative — it must not fall through to an
// ambient BD_BIN, same as cmd/gc's resolver.
func TestCityPinnedBDBinWorkspacePATHSearch(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("fake binary script uses a POSIX shebang")
	}
	t.Setenv("BD_BIN", "/should-not-be-used/bd")
	dir := t.TempDir()
	fakeBD := filepath.Join(dir, "bd")
	if err := os.WriteFile(fakeBD, []byte("#!/bin/sh\necho ok\n"), 0o755); err != nil { //nolint:gosec // test fixture, deliberately executable
		t.Fatalf("write fake bd: %v", err)
	}

	t.Run("workspace PATH resolves bd", func(t *testing.T) {
		cfg := &config.City{Workspace: config.Workspace{Env: map[string]string{"PATH": dir}}}
		if got := cityPinnedBDBin(cfg); got != fakeBD {
			t.Errorf("cityPinnedBDBin() = %q, want %q", got, fakeBD)
		}
	})

	t.Run("workspace PATH without bd is authoritative, no ambient fallback", func(t *testing.T) {
		empty := t.TempDir()
		cfg := &config.City{Workspace: config.Workspace{Env: map[string]string{"PATH": empty}}}
		if got := cityPinnedBDBin(cfg); got != "" {
			t.Errorf("cityPinnedBDBin() = %q, want empty (workspace PATH is authoritative)", got)
		}
	})
}

// TestCityPinnedBDBinAmbientFallback covers the third resolution source: an
// inherited ambient BD_BIN, used only when workspace.env configures neither
// BD_BIN nor PATH — the same precedence cmd/gc's workspacePinnedBdBinaryOptional
// applies for a freshly-initialized city with no generated pin yet.
func TestCityPinnedBDBinAmbientFallback(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("fake binary script uses a POSIX shebang")
	}
	dir := t.TempDir()
	fakeBD := filepath.Join(dir, "fake-bd")
	if err := os.WriteFile(fakeBD, []byte("#!/bin/sh\necho ok\n"), 0o755); err != nil { //nolint:gosec // test fixture, deliberately executable
		t.Fatalf("write fake bd: %v", err)
	}

	t.Run("valid ambient BD_BIN is used", func(t *testing.T) {
		t.Setenv("BD_BIN", fakeBD)
		cfg := &config.City{Workspace: config.Workspace{}}
		if got := cityPinnedBDBin(cfg); got != fakeBD {
			t.Errorf("cityPinnedBDBin() = %q, want %q", got, fakeBD)
		}
	})

	t.Run("invalid ambient BD_BIN warns and falls back to empty", func(t *testing.T) {
		t.Setenv("BD_BIN", "not-absolute")
		var buf bytes.Buffer
		defer captureLog(t, &buf)()
		cfg := &config.City{Workspace: config.Workspace{}}
		if got := cityPinnedBDBin(cfg); got != "" {
			t.Errorf("cityPinnedBDBin() = %q, want empty", got)
		}
		if !strings.Contains(buf.String(), "not-absolute") {
			t.Errorf("expected a warning naming the bad ambient BD_BIN, got %q", buf.String())
		}
	})
}

// TestResolveComponentVersionsUsesCityPinnedBDBin verifies the probe wiring
// end to end: resolveComponentVersions passes the city's pinned BD_BIN
// through to beads.ProbeBDVersion rather than letting it resolve "bd" on
// PATH, by pinning BD_BIN to a fake script and checking its fake version
// comes back.
func TestResolveComponentVersionsUsesCityPinnedBDBin(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("fake binary script uses a POSIX shebang")
	}
	dir := t.TempDir()
	fakeBD := filepath.Join(dir, "fake-bd")
	script := "#!/bin/sh\necho 'bd version 7.7.7 (pinned)'\n"
	if err := os.WriteFile(fakeBD, []byte(script), 0o755); err != nil { //nolint:gosec // test fixture, deliberately executable
		t.Fatalf("write fake bd: %v", err)
	}

	state := newFakeState(t)
	state.cfg.Workspace.Env = map[string]string{"BD_BIN": fakeBD}
	s := &Server{state: state}

	got := s.resolveComponentVersions()
	if got.Beads != "7.7.7" {
		t.Errorf("resolveComponentVersions().Beads = %q, want %q (the city-pinned bd's version)", got.Beads, "7.7.7")
	}
}

// captureLog redirects the standard logger to buf for the duration of a test,
// returning a restore func.
func captureLog(t *testing.T, buf *bytes.Buffer) func() {
	t.Helper()
	prevOut := log.Writer()
	prevFlags := log.Flags()
	log.SetOutput(buf)
	log.SetFlags(0)
	return func() {
		log.SetOutput(prevOut)
		log.SetFlags(prevFlags)
	}
}
