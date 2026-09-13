package main

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestParseDetachedProbeSpec(t *testing.T) {
	tests := []struct {
		name    string
		spec    string
		want    detachedProbeSpec
		wantErr bool
	}{
		{
			name: "tmux spec",
			spec: "tmux:gascity:soak-loop",
			want: detachedProbeSpec{
				Kind:    "tmux",
				Socket:  "gascity",
				Session: "soak-loop",
			},
		},
		{
			name: "session keeps extra colons",
			spec: "tmux:gascity:scope:soak-loop",
			want: detachedProbeSpec{
				Kind:    "tmux",
				Socket:  "gascity",
				Session: "scope:soak-loop",
			},
		},
		{
			name:    "unsupported kind",
			spec:    "k8s:pod:agent",
			wantErr: true,
		},
		{
			name:    "missing session",
			spec:    "tmux:gascity",
			wantErr: true,
		},
		{
			name:    "empty socket",
			spec:    "tmux::soak-loop",
			wantErr: true,
		},
		{
			name:    "empty session",
			spec:    "tmux:gascity:",
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseDetachedProbeSpec(tt.spec)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("parseDetachedProbeSpec(%q) error = nil, want error", tt.spec)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseDetachedProbeSpec(%q) error = %v", tt.spec, err)
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("parseDetachedProbeSpec(%q) = %+v, want %+v", tt.spec, got, tt.want)
			}
		})
	}
}

func TestProbeDetachedWork_TmuxExitStatus(t *testing.T) {
	tests := []struct {
		name       string
		exitCode   string
		wantStatus detachedProbeStatus
	}{
		{name: "alive on exit zero", exitCode: "0", wantStatus: detachedProbeAlive},
		{name: "dead on exit one", exitCode: "1", wantStatus: detachedProbeDead},
		{name: "error on other exit", exitCode: "2", wantStatus: detachedProbeError},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			argsFile := filepath.Join(t.TempDir(), "args")
			installFakeTmux(t, `printf '%s\n' "$@" > "$FAKE_TMUX_ARGS"; exit `+tt.exitCode)
			t.Setenv("FAKE_TMUX_ARGS", argsFile)

			got := probeDetachedWork(context.Background(), "tmux:gascity:soak-loop")
			if got.Status != tt.wantStatus {
				t.Fatalf("Status = %q, want %q (err=%v)", got.Status, tt.wantStatus, got.Err)
			}
			args, err := os.ReadFile(argsFile)
			if err != nil {
				t.Fatalf("read fake tmux args: %v", err)
			}
			wantArgs := "-L\ngascity\nhas-session\n-t\nsoak-loop\n"
			if string(args) != wantArgs {
				t.Fatalf("tmux args = %q, want %q", string(args), wantArgs)
			}
		})
	}
}

func TestProbeDetachedWork_MalformedSpecIsError(t *testing.T) {
	got := probeDetachedWorkWithTimeout(context.Background(), "tmux:gascity", time.Second)
	if got.Status != detachedProbeError {
		t.Fatalf("Status = %q, want %q", got.Status, detachedProbeError)
	}
	if got.Err == nil {
		t.Fatal("Err = nil, want parse error")
	}
}

func TestProbeDetachedWork_Timeout(t *testing.T) {
	installFakeTmux(t, "sleep 1")

	got := probeDetachedWorkWithTimeout(context.Background(), "tmux:gascity:soak-loop", 10*time.Millisecond)
	if got.Status != detachedProbeTimeout {
		t.Fatalf("Status = %q, want %q (err=%v)", got.Status, detachedProbeTimeout, got.Err)
	}
	if got.Err == nil {
		t.Fatal("Err = nil, want timeout error")
	}
}

func installFakeTmux(t *testing.T, body string) {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "tmux")
	script := "#!/bin/sh\n" + body + "\n"
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatalf("write fake tmux: %v", err)
	}
	pathEnv := dir
	if existing := os.Getenv("PATH"); strings.TrimSpace(existing) != "" {
		pathEnv += string(os.PathListSeparator) + existing
	}
	t.Setenv("PATH", pathEnv)
}
