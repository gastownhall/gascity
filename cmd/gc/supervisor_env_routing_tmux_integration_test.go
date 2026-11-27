//go:build integration

package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/gastownhall/gascity/internal/runtime"
)

// TestSupervisorSessionsWithholdStaleServerRoutingAfterCleanReinstallOnRealTmux
// is the two-install regression for #9257 on a real tmux server. The city's
// tmux server outlives every supervisor install, and one started while the
// shell exported a provider's routing holds that routing in its global
// environment. The first install, from that shell, drops the routing because
// the provider's credentials come only from secrets.env. The second, from a
// shell without the routing, has nothing to drop. A session the second
// install's supervisor starts on the surviving server still gets none of the
// server's endpoint or model names, at create and at relaunch, while routing
// the session env sets, here re-supplied through GC_SUPERVISOR_ENV, reaches it.
func TestSupervisorSessionsWithholdStaleServerRoutingAfterCleanReinstallOnRealTmux(t *testing.T) {
	path := os.Getenv("PATH")
	isolateSupervisorRoutingEnv(t)
	// Keep the tmux binary reachable: no service file is written here.
	t.Setenv("PATH", path)
	setIncidentEnv(t)

	first, err := buildSupervisorServiceData()
	if err != nil {
		t.Fatalf("first buildSupervisorServiceData: %v", err)
	}
	if len(first.WithheldEnv) != 1 || !slices.Equal(first.WithheldEnv[0].Dropped, incidentRoutingNames) {
		t.Fatalf("first install withheld %+v, want the incident shell's %v dropped", first.WithheldEnv, incidentRoutingNames)
	}

	// The surviving city tmux server, started from the incident shell's
	// environment.
	sp, tm := realTmux(t, "rt")
	tm("new-session", "-d", "-s", "keeper", "sleep 600")
	if got, want := tm("show-environment", "-g", "ANTHROPIC_BASE_URL"), "ANTHROPIC_BASE_URL=shell-anthropic_base_url"; got != want {
		t.Fatalf("server global env holds %q, want %q", got, want)
	}
	relauncher, ok := sp.(interface {
		Relaunch(context.Context, string, runtime.Config) error
	})
	if !ok {
		t.Fatalf("tmux provider %T has no Relaunch", sp)
	}

	for i, tc := range []struct {
		name     string
		endpoint string // re-supplied by the second install's shell through GC_SUPERVISOR_ENV
		want     string // the session's ANTHROPIC_BASE_URL
	}{
		{"clean shell", "", "ABSENT"},
		{"endpoint re-supplied", "https://allowed.example.test", "https://allowed.example.test"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, name := range incidentRoutingNames {
				t.Setenv(name, "")
			}
			if tc.endpoint != "" {
				t.Setenv("ANTHROPIC_BASE_URL", tc.endpoint)
				t.Setenv("GC_SUPERVISOR_ENV", "ANTHROPIC_BASE_URL")
			}
			second, err := buildSupervisorServiceData()
			if err != nil {
				t.Fatalf("second buildSupervisorServiceData: %v", err)
			}
			if len(second.WithheldEnv) != 0 {
				t.Fatalf("second install withheld %+v, want nothing: its shell has no routing to drop", second.WithheldEnv)
			}
			// Run as the supervisor the second unit starts: its env is the
			// unit's.
			for _, v := range second.ExtraEnv {
				t.Setenv(v.Name, v.Value)
			}
			env := passthroughEnv()

			name := fmt.Sprintf("s-routing-%d", i)
			want := tc.want + "|ABSENT|tok-file-1"
			dir := t.TempDir()
			for _, step := range []struct {
				what   string
				launch func(context.Context, string, runtime.Config) error
			}{
				{"start", sp.Start},
				{"relaunch", relauncher.Relaunch},
			} {
				report := filepath.Join(dir, step.what)
				probe := fmt.Sprintf(`sh -c 'printf "%%s|%%s|%%s" "${ANTHROPIC_BASE_URL-ABSENT}" "${ANTHROPIC_MODEL-ABSENT}" "${ANTHROPIC_AUTH_TOKEN-ABSENT}" > %s; exec sleep 600'`, report)
				if err := step.launch(context.Background(), name, runtime.Config{Command: probe, Env: env}); err != nil {
					t.Fatalf("%s: %v", step.what, err)
				}
				read := func() string {
					b, _ := os.ReadFile(report)
					return string(b)
				}
				waitTmux(t, step.what+" to report its routing", func() bool { return read() != "" })
				if got := read(); got != want {
					t.Errorf("after %s the session saw ANTHROPIC_BASE_URL|ANTHROPIC_MODEL|ANTHROPIC_AUTH_TOKEN = %q, want %q", step.what, got, want)
				}
			}
		})
	}
}
