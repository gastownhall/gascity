package rollout

import (
	"testing"

	"github.com/gastownhall/gascity/internal/config"
)

func TestResolveDaemonSessionReconciler(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		raw        string
		want       Mode
		wantOrigin Origin
		wantErr    bool
	}{
		{name: "omitted", want: Off, wantOrigin: OriginBuiltin},
		{name: "off", raw: "off", want: Off, wantOrigin: OriginConfig},
		{name: "auto", raw: "auto", want: Auto, wantOrigin: OriginConfig},
		{name: "require", raw: "require", want: Require, wantOrigin: OriginConfig},
		{name: "invalid", raw: "enabled", wantErr: true},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			flags, err := Resolve(
				&config.City{Daemon: config.DaemonConfig{SessionReconciler: test.raw}},
				ResolveOptions{LookupEnv: func(string) (string, bool) { return "", false }},
			)
			if (err != nil) != test.wantErr {
				t.Fatalf("Resolve error = %v, wantErr=%v", err, test.wantErr)
			}
			if test.wantErr {
				return
			}
			if got := flags.SessionReconciler(); got != test.want {
				t.Fatalf("SessionReconciler = %q, want %q", got, test.want)
			}
			if got := flags.OriginOf(KeyDaemonSessionReconciler); got != test.wantOrigin {
				t.Fatalf("OriginOf = %q, want %q", got, test.wantOrigin)
			}
			if got := flags.ValueOf(KeyDaemonSessionReconciler); got != string(test.want) {
				t.Fatalf("ValueOf = %q, want %q", got, test.want)
			}
		})
	}
}

func TestForTestSessionReconcilerIsInstanceLocal(t *testing.T) {
	t.Parallel()

	if got := ForTest().SessionReconciler(); got != Off {
		t.Fatalf("ForTest default = %q, want off", got)
	}
	auto := ForTest(WithSessionReconciler(Auto))
	require := ForTest(WithSessionReconciler(Require))
	off := ForTest(WithSessionReconciler(Off))
	if auto.SessionReconciler() != Auto ||
		require.SessionReconciler() != Require ||
		off.SessionReconciler() != Off {
		t.Fatalf(
			"ForTest modes = %q/%q/%q, want auto/require/off",
			auto.SessionReconciler(),
			require.SessionReconciler(),
			off.SessionReconciler(),
		)
	}
}

func TestZeroFlagsKeepsSessionReconcilerLegacy(t *testing.T) {
	t.Parallel()

	var flags Flags
	if got := flags.SessionReconciler(); got != ModeUnset {
		t.Fatalf("zero Flags session-start reconciler = %q, want ModeUnset", got)
	}
	if got := flags.OriginOf(KeyDaemonSessionReconciler); got != "" {
		t.Fatalf("zero Flags origin = %q, want empty", got)
	}
}

func TestSessionReconcilerRegistryBinding(t *testing.T) {
	t.Parallel()

	for _, spec := range Specs() {
		if spec.Key != KeyDaemonSessionReconciler {
			continue
		}
		if spec.ConfigPath != "daemon.session_reconciler" {
			t.Fatalf("ConfigPath = %q", spec.ConfigPath)
		}
		if spec.Default.Mode == nil || *spec.Default.Mode != Off {
			t.Fatalf("Default = %#v, want mode off", spec.Default)
		}
		if spec.EnvOverride != "" {
			t.Fatalf("EnvOverride = %q, want none for boot-latched ownership", spec.EnvOverride)
		}
		if spec.Owner.Bead != "ga-f7v2ft" {
			t.Fatalf("Owner.Bead = %q, want ga-f7v2ft", spec.Owner.Bead)
		}
		return
	}
	t.Fatalf("registry missing %s", KeyDaemonSessionReconciler)
}

func TestSessionReconcilerDefaultsDoNotDrift(t *testing.T) {
	t.Parallel()

	if got := defaultFlags().SessionReconciler(); got != Off {
		t.Fatalf("defaultFlags session-start reconciler = %q, want off", got)
	}
	if got := (config.DaemonConfig{}).SessionReconcilerMode(); got != string(Off) {
		t.Fatalf("config accessor default = %q, want %q", got, Off)
	}
}
