package proxyendpoint

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// writeSidecar writes body as a scope's proxied-server sidecar.
func writeSidecar(t *testing.T, beadsDir, body string) {
	t.Helper()
	if err := os.MkdirAll(beadsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(SidecarPath(beadsDir), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

// TestSidecarIdleSemantics pins bd's three-way meaning for one field whose zero
// value is unwritable.
//
// bd tags idle_timeout omitempty, so a 0 never reaches the file and reads back
// as absent, and its provider substitutes 30s for both. A reader that treated
// absent as "no timeout" would hold a resident pool against a proxy bd retires
// after 30s of quiet — the exact failure the distinction exists to prevent.
func TestSidecarIdleSemantics(t *testing.T) {
	never := IdleTimeoutNever
	zero := time.Duration(0)
	five := 5 * time.Second

	cases := []struct {
		name          string
		sidecar       Sidecar
		want          IdlePolicy
		explicitNever bool
	}{
		{
			name:    "no sidecar at all",
			sidecar: Sidecar{},
			want:    IdlePolicy{Kind: IdleFinite, Timeout: BdDefaultIdleTimeout, Source: IdleSourceBdDefault},
		},
		{
			name:    "sidecar with no idle_timeout key",
			sidecar: Sidecar{Present: true},
			want:    IdlePolicy{Kind: IdleFinite, Timeout: BdDefaultIdleTimeout, Source: IdleSourceBdDefault},
		},
		{
			name:    "an explicit zero is bd's default, not never",
			sidecar: Sidecar{Present: true, IdleTimeout: &zero},
			want:    IdlePolicy{Kind: IdleFinite, Timeout: BdDefaultIdleTimeout, Source: IdleSourceBdDefault},
		},
		{
			name:          "negative is never",
			sidecar:       Sidecar{Present: true, IdleTimeout: &never},
			want:          IdlePolicy{Kind: IdleNever, Source: IdleSourceSidecar},
			explicitNever: true,
		},
		{
			name:    "positive is that window",
			sidecar: Sidecar{Present: true, IdleTimeout: &five},
			want:    IdlePolicy{Kind: IdleFinite, Timeout: five, Source: IdleSourceSidecar},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.sidecar.IdlePolicy(); got != tc.want {
				t.Fatalf("IdlePolicy() = %v, want %v", got, tc.want)
			}
			if got := tc.sidecar.ExplicitIdleNever(); got != tc.explicitNever {
				t.Fatalf("ExplicitIdleNever() = %v, want %v", got, tc.explicitNever)
			}
		})
	}
}

// TestResolveIdlePolicyArgvWins crosses both sources: the live supervisor's argv
// is the truth for a running proxy, and the sidecar answers only when no live
// argv did.
func TestResolveIdlePolicyArgvWins(t *testing.T) {
	never := IdleTimeoutNever
	ten := 10 * time.Second

	cases := []struct {
		name    string
		sidecar Sidecar
		argv    IdlePolicy
		want    IdlePolicy
	}{
		{
			name:    "argv never over a sidecar default",
			sidecar: Sidecar{Present: true},
			argv:    IdlePolicy{Kind: IdleNever, Source: IdleSourceArgv},
			want:    IdlePolicy{Kind: IdleNever, Source: IdleSourceArgv},
		},
		{
			name:    "argv finite over a sidecar never — the operator edited the file under a live proxy",
			sidecar: Sidecar{Present: true, IdleTimeout: &never},
			argv:    IdlePolicy{Kind: IdleFinite, Timeout: ten, Source: IdleSourceArgv},
			want:    IdlePolicy{Kind: IdleFinite, Timeout: ten, Source: IdleSourceArgv},
		},
		{
			name:    "no argv falls back to the sidecar",
			sidecar: Sidecar{Present: true, IdleTimeout: &never},
			argv:    IdlePolicy{},
			want:    IdlePolicy{Kind: IdleNever, Source: IdleSourceSidecar},
		},
		{
			name:    "no argv and no sidecar is bd's default",
			sidecar: Sidecar{},
			argv:    IdlePolicy{},
			want:    IdlePolicy{Kind: IdleFinite, Timeout: BdDefaultIdleTimeout, Source: IdleSourceBdDefault},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := ResolveIdlePolicy(tc.sidecar, tc.argv); got != tc.want {
				t.Fatalf("ResolveIdlePolicy() = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestReadSidecar(t *testing.T) {
	t.Run("absent is not an error", func(t *testing.T) {
		got, err := ReadSidecar(filepath.Join(t.TempDir(), ".beads"))
		if err != nil {
			t.Fatalf("ReadSidecar on an absent file = %v, want nil", err)
		}
		if got.Present {
			t.Fatal("an absent sidecar reported Present")
		}
	})

	t.Run("bd's gc-owned shape reads back as an explicit never", func(t *testing.T) {
		beadsDir := filepath.Join(t.TempDir(), ".beads")
		// The literal bd writes for a scope initialized with
		// --proxied-server-idle-timeout 0, which it maps to IdleTimeoutNever
		// (-1ns) before persisting.
		writeSidecar(t, beadsDir, `{"root_path":"dolt","idle_timeout":-1}`)
		got, err := ReadSidecar(beadsDir)
		if err != nil {
			t.Fatalf("ReadSidecar: %v", err)
		}
		if !got.Present || got.IdleTimeout == nil || *got.IdleTimeout != IdleTimeoutNever {
			t.Fatalf("ReadSidecar = %+v, want an explicit -1ns idle timeout", got)
		}
		if !got.ExplicitIdleNever() {
			t.Fatal("a sidecar carrying idle_timeout -1 did not read as an explicit never")
		}
		if want := (IdlePolicy{Kind: IdleNever, Source: IdleSourceSidecar}); got.IdlePolicy() != want {
			t.Fatalf("IdlePolicy() = %v, want %v", got.IdlePolicy(), want)
		}
	})

	t.Run("an operator-initialized scope carries no key", func(t *testing.T) {
		beadsDir := filepath.Join(t.TempDir(), ".beads")
		writeSidecar(t, beadsDir, `{"root_path":"dolt"}`)
		got, err := ReadSidecar(beadsDir)
		if err != nil {
			t.Fatalf("ReadSidecar: %v", err)
		}
		if got.IdleTimeout != nil {
			t.Fatalf("idle_timeout = %v, want absent", *got.IdleTimeout)
		}
		if got.ExplicitIdleNever() {
			t.Fatal("an absent idle_timeout read as an explicit never; bd's provider would substitute 30s")
		}
	})

	t.Run("malformed is an error, not a default", func(t *testing.T) {
		beadsDir := filepath.Join(t.TempDir(), ".beads")
		writeSidecar(t, beadsDir, `{"root_path":`)
		if _, err := ReadSidecar(beadsDir); err == nil {
			t.Fatal("ReadSidecar accepted a truncated sidecar")
		}
	})
}

func TestIdlePolicyRendering(t *testing.T) {
	cases := []struct {
		policy IdlePolicy
		want   string
	}{
		{IdlePolicy{}, "unknown"},
		{IdlePolicy{Kind: IdleNever, Source: IdleSourceArgv}, "never(argv)"},
		{IdlePolicy{Kind: IdleNever, Source: IdleSourceSidecar}, "never(sidecar)"},
		{IdlePolicy{Kind: IdleFinite, Timeout: BdDefaultIdleTimeout, Source: IdleSourceBdDefault}, "finite(30s, bd-default)"},
		{IdlePolicy{Kind: IdleFinite, Timeout: 5 * time.Second, Source: IdleSourceArgv}, "finite(5s, argv)"},
	}
	for _, tc := range cases {
		if got := tc.policy.String(); got != tc.want {
			t.Errorf("IdlePolicy.String() = %q, want %q", got, tc.want)
		}
	}
}
