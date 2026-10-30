package beads

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	beadslib "github.com/steveyegge/beads"
)

// TestDirectOpenWithholdsBDAllowRemoteMigrateByDefault and
// TestDirectOpenWithoutAmbientEnvWithholdsBDAllowRemoteMigrateByDefault pin
// that a direct native open never hands the linked library an inherited
// BD_ALLOW_REMOTE_MIGRATE: the env-carrying open (OpenNativeDoltStoreAt, whose
// caller did not set the key) and the hosted open that withholds the ambient
// namespace both open with the key unset, and both restore the ambient value
// afterwards.
func TestDirectOpenWithholdsBDAllowRemoteMigrateByDefault(t *testing.T) {
	t.Setenv(BDAllowRemoteMigrateEnvKey, "ambient-poison")
	oldOpen := nativeDoltOpenBestAvailable
	t.Cleanup(func() { nativeDoltOpenBestAvailable = oldOpen })

	var seen string
	seenSet := false
	nativeDoltOpenBestAvailable = func(context.Context, string) (beadslib.Storage, error) {
		seen = os.Getenv(BDAllowRemoteMigrateEnvKey)
		seenSet = true
		return &nativeDoltStorageSpy{
			getConfig: func(context.Context, string) (string, error) { return "gc", nil },
		}, nil
	}

	store, err := OpenNativeDoltStoreAt(context.Background(), filepath.Join(t.TempDir(), "scope"), nil)
	if err != nil {
		t.Fatalf("OpenNativeDoltStoreAt: %v", err)
	}
	t.Cleanup(func() { _ = store.CloseStore() })

	if !seenSet {
		t.Fatal("the open seam never ran")
	}
	if seen != "" {
		t.Errorf("BD_ALLOW_REMOTE_MIGRATE during the open = %q, want withheld (unset): an ambient unlock must not reach a database gc opens directly without the city's own opt-in", seen)
	}
	if got := os.Getenv(BDAllowRemoteMigrateEnvKey); got != "ambient-poison" {
		t.Errorf("BD_ALLOW_REMOTE_MIGRATE after the open = %q, want the ambient value restored", got)
	}
}

func TestDirectOpenWithoutAmbientEnvWithholdsBDAllowRemoteMigrateByDefault(t *testing.T) {
	t.Setenv(BDAllowRemoteMigrateEnvKey, "ambient-poison")
	oldOpen := nativeDoltOpenBestAvailable
	t.Cleanup(func() { nativeDoltOpenBestAvailable = oldOpen })

	var seen string
	seenSet := false
	nativeDoltOpenBestAvailable = func(context.Context, string) (beadslib.Storage, error) {
		seen = os.Getenv(BDAllowRemoteMigrateEnvKey)
		seenSet = true
		return &nativeDoltStorageSpy{
			getConfig: func(context.Context, string) (string, error) { return "gc", nil },
		}, nil
	}

	store, err := OpenNativeDoltStoreAtWithoutAmbientEnv(context.Background(), filepath.Join(t.TempDir(), "scope"))
	if err != nil {
		t.Fatalf("OpenNativeDoltStoreAtWithoutAmbientEnv: %v", err)
	}
	t.Cleanup(func() { _ = store.CloseStore() })

	if !seenSet {
		t.Fatal("the open seam never ran")
	}
	if seen != "" {
		t.Errorf("BD_ALLOW_REMOTE_MIGRATE during the hosted open = %q, want withheld (unset): an ambient unlock must not reach a database gc opens for a hosted workspace without the city's own opt-in", seen)
	}
	if got := os.Getenv(BDAllowRemoteMigrateEnvKey); got != "ambient-poison" {
		t.Errorf("BD_ALLOW_REMOTE_MIGRATE after the open = %q, want the ambient value restored", got)
	}
}

// TestDirectOpenProjectsBDAllowRemoteMigrateFromTheCallerEnv pins the other
// half: the open env is the only route by which gc's decision for an opted-in
// city reaches the linked library, so the key the caller set must be in the
// process environment for the initial open and for every reopen, and a blank
// value must withhold it like an absent one. The ambient value is restored
// after each open.
func TestDirectOpenProjectsBDAllowRemoteMigrateFromTheCallerEnv(t *testing.T) {
	openers := []struct {
		name string
		open func(t *testing.T, scopeRoot string, env map[string]string)
	}{
		{name: "initial open", open: func(t *testing.T, scopeRoot string, env map[string]string) {
			t.Helper()
			store, err := OpenNativeDoltStoreAt(context.Background(), scopeRoot, env)
			if err != nil {
				t.Fatalf("OpenNativeDoltStoreAt: %v", err)
			}
			t.Cleanup(func() { _ = store.CloseStore() })
		}},
		{name: "reopen", open: func(t *testing.T, scopeRoot string, env map[string]string) {
			t.Helper()
			storage, err := OpenNativeStorage(context.Background(), scopeRoot, env)
			if err != nil {
				t.Fatalf("OpenNativeStorage: %v", err)
			}
			t.Cleanup(func() { _ = storage.Close() })
		}},
	}
	for _, opener := range openers {
		for _, tc := range []struct {
			name  string
			value string
			want  string
		}{
			{name: "set", value: "1", want: "1"},
			{name: "blank withholds", value: "  ", want: ""},
		} {
			t.Run(opener.name+"/"+tc.name, func(t *testing.T) {
				t.Setenv(BDAllowRemoteMigrateEnvKey, "ambient-poison")
				oldOpen := nativeDoltOpenBestAvailable
				t.Cleanup(func() { nativeDoltOpenBestAvailable = oldOpen })

				var seen string
				seenSet := false
				nativeDoltOpenBestAvailable = func(context.Context, string) (beadslib.Storage, error) {
					seen = os.Getenv(BDAllowRemoteMigrateEnvKey)
					seenSet = true
					return &nativeDoltStorageSpy{
						getConfig: func(context.Context, string) (string, error) { return "gc", nil },
					}, nil
				}

				opener.open(t, filepath.Join(t.TempDir(), "scope"), map[string]string{BDAllowRemoteMigrateEnvKey: tc.value})

				if !seenSet {
					t.Fatal("the open seam never ran")
				}
				if seen != tc.want {
					t.Errorf("BD_ALLOW_REMOTE_MIGRATE during the open = %q, want %q: the open must carry exactly the caller's decision", seen, tc.want)
				}
				if got := os.Getenv(BDAllowRemoteMigrateEnvKey); got != "ambient-poison" {
					t.Errorf("BD_ALLOW_REMOTE_MIGRATE after the open = %q, want the ambient value restored", got)
				}
			})
		}
	}
}
