package main

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// providerOwnedIdentityFixture is a pending (journaled provider_initializing)
// provider-owned city whose provider script always succeeds and whose
// workspace-pinned bd answers `config get --json issue_prefix` with prefixJSON
// (or fails when prefixExit is non-zero). It is the interrupted-init shape: the
// provider's init op exits 0, so only bd's own answer can say whether the
// identity bootstrap ran.
func providerOwnedIdentityFixture(t *testing.T, prefixJSON string, prefixExit int) (city, marker string) {
	t.Helper()
	city = t.TempDir()
	marker = filepath.Join(city, "bd-invoked")
	prefixFile := filepath.Join(city, "stub-prefix.json")
	if err := os.WriteFile(prefixFile, []byte(prefixJSON+"\n"), 0o644); err != nil { //nolint:gosec // fixture
		t.Fatal(err)
	}
	pinnedBd := filepath.Join(city, "pinned-bd")
	bdScript := "#!/bin/sh\nprintf '%s\\n' \"$*\" >> " + marker + "\n" +
		"case \"$*\" in\n" +
		"  'config get --json issue_prefix') cat " + prefixFile + "; exit " + strconv.Itoa(prefixExit) + " ;;\n" +
		"  'config get --json types.custom') printf '{\"value\":\"\"}\\n' ;;\n" +
		"  'types --json') printf '{\"core_types\":[]}\\n' ;;\n" +
		"esac\nexit 0\n"
	if err := os.WriteFile(pinnedBd, []byte(bdScript), 0o755); err != nil { //nolint:gosec // fixture must be executable
		t.Fatal(err)
	}
	provider := filepath.Join(city, "provider.sh")
	if err := os.WriteFile(provider, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil { //nolint:gosec // fixture must be executable
		t.Fatal(err)
	}
	cityToml := "[workspace]\nname = \"identity\"\n\n[workspace.env]\nBD_BIN = \"" + pinnedBd + "\"\n" +
		"[beads]\nprovider = \"exec:" + provider + "\"\n"
	if err := os.WriteFile(filepath.Join(city, "city.toml"), []byte(cityToml), 0o644); err != nil { //nolint:gosec // fixture
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(city, ".beads"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := persistProviderScopeOwnership(city, city, providerScopeIntent{Transport: "proxied", Target: "local"}); err != nil {
		t.Fatal(err)
	}
	return city, marker
}

// TestInitAndHookDirProviderOwnedJournalsReadyOnlyWhenBdBootstrapped pins the
// post-init identity check (ga-a7f4frg): a provider-owned scope is journaled
// ready only once bd reports an issue_prefix. An interrupted proxied init
// leaves a bare root that bd (--init-if-missing) reports as already
// initialized, so the provider's init op exits 0 with the bootstrap never run;
// the journal must stay provider_initializing so the next run retries instead
// of handing the operator a city whose `bd create` fails.
func TestInitAndHookDirProviderOwnedJournalsReadyOnlyWhenBdBootstrapped(t *testing.T) {
	for _, tc := range []struct {
		name       string
		prefixJSON string
		prefixExit int
		wantErr    string // substring; "" means success
		wantReady  bool
	}{
		{
			name:       "unset prefix keeps the journal initializing",
			prefixJSON: `{"key":"issue_prefix","value":""}`,
			wantErr:    "bootstrap never ran",
		},
		{
			name:       "whitespace-only prefix counts as unset",
			prefixJSON: `{"key":"issue_prefix","value":"  "}`,
			wantErr:    "bootstrap never ran",
		},
		{
			name:       "bootstrapped prefix journals ready",
			prefixJSON: `{"key":"issue_prefix","value":"af"}`,
			wantReady:  true,
		},
		{
			name:       "unreadable prefix is returned, not swallowed",
			prefixJSON: `boom`,
			prefixExit: 1,
			wantErr:    "verifying provider-owned scope",
		},
		{
			name:       "unparseable prefix is returned, not swallowed",
			prefixJSON: `not json`,
			wantErr:    "verifying provider-owned scope",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			city, marker := providerOwnedIdentityFixture(t, tc.prefixJSON, tc.prefixExit)

			err := initAndHookDir(city, city, "af")

			switch {
			case tc.wantErr == "" && err != nil:
				t.Fatalf("initAndHookDir: %v", err)
			case tc.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tc.wantErr)):
				t.Fatalf("initAndHookDir = %v, want an error containing %q", err, tc.wantErr)
			}
			if tc.wantErr != "" && !strings.Contains(err.Error(), city) {
				t.Errorf("error %q does not name the scope %q", err, city)
			}
			entry, owned, ownErr := providerOwnedScopeState(city, city)
			if ownErr != nil || !owned {
				t.Fatalf("providerOwnedScopeState = (%+v, %t, %v), want an owned scope", entry, owned, ownErr)
			}
			if got := entry.State == providerScopeReady; got != tc.wantReady {
				t.Fatalf("journal state = %q, ready = %t, want ready = %t", entry.State, got, tc.wantReady)
			}
			data, readErr := os.ReadFile(marker)
			if readErr != nil || !strings.Contains(string(data), "config get --json issue_prefix") {
				t.Fatalf("bd invocations = %q (%v), want one `config get --json issue_prefix`", data, readErr)
			}
		})
	}
}

// TestInitAndHookDirReadyProviderOwnedScopeSkipsTheIdentityCheck pins the cost
// bound: a scope the journal already marks ready is reopened by the provider
// `start` op, and the identity check adds no bd fork to that steady-state path.
func TestInitAndHookDirReadyProviderOwnedScopeSkipsTheIdentityCheck(t *testing.T) {
	city, marker := providerOwnedIdentityFixture(t, `{"key":"issue_prefix","value":""}`, 0)
	if err := markProviderScopeOwnershipReady(city, city); err != nil {
		t.Fatal(err)
	}

	if err := initAndHookDir(city, city, "af"); err != nil {
		t.Fatalf("initAndHookDir on a ready scope: %v", err)
	}

	data, _ := os.ReadFile(marker)
	if strings.Contains(string(data), "issue_prefix") {
		t.Fatalf("a ready scope read issue_prefix: bd invocations = %q", data)
	}
}
