//go:build integration

package integration

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads/contract"
	"github.com/gastownhall/gascity/internal/fsys"
)

// TestGasCityBeadsModeSelectorFrontDoor runs the production gc binary against
// a recording bd-contract provider. The provider derives the scope's dolt_mode
// from the BEADS_DOLT_PROXIED_SERVER request gc sends it, so the assertions
// read back gc's selection rather than fixture-authored metadata: a fresh
// managed-local scope must request Beads' proxied init, and [dolt] mode =
// "server" must seed direct-server markers without a proxied request even
// when the ambient shell asks for the proxy.
func TestGasCityBeadsModeSelectorFrontDoor(t *testing.T) {
	for _, tc := range []struct {
		name, mode, wantMode string
	}{
		{name: "fresh default selects proxied", wantMode: "proxied-server"},
		{name: "explicit server selects direct", mode: "server", wantMode: "server"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cityDir := t.TempDir()
			if err := os.WriteFile(filepath.Join(cityDir, "pack.toml"), []byte("[pack]\nname=\"mode-frontdoor\"\nschema=2\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			// The gc-beads-bd basename routes the fake through gc's bd-contract
			// init path, which is where gc chooses proxied or direct Dolt.
			providerDir := t.TempDir()
			logPath := filepath.Join(providerDir, "provider.log")
			provider := filepath.Join(providerDir, "gc-beads-bd.sh")
			script := "#!/bin/sh\n" +
				"printf '%s|proxied=%s\\n' \"$*\" \"${BEADS_DOLT_PROXIED_SERVER:-}\" >> \"" + logPath + "\"\n" +
				"[ \"$1\" = init ] || exit 2\n" +
				"mode=server\n" +
				"[ \"${BEADS_DOLT_PROXIED_SERVER:-}\" = 1 ] && mode=proxied-server\n" +
				"mkdir -p \"$2/.beads\"\n" +
				"printf '{\"backend\":\"dolt\",\"database\":\"dolt\",\"dolt_mode\":\"%s\"}' \"$mode\" > \"$2/.beads/metadata.json\"\n"
			if err := os.WriteFile(provider, []byte(script), 0o755); err != nil {
				t.Fatal(err)
			}
			// Keep provider selection in the city config so this exercises gc's
			// front door and lifecycle resolver, not a helper function.
			data := []byte("[workspace]\nname = \"mode-frontdoor-" + strings.ReplaceAll(tc.name, " ", "-") + "\"\n")
			if tc.mode != "" {
				data = append(data, []byte("[dolt]\nmode = \""+tc.mode+"\"\n")...)
			}
			data = append(data, []byte("[beads]\nprovider = \"exec:"+provider+"\"\n")...)
			env := commandEnvForDir(cityDir, false)
			if tc.mode == "" {
				// Proxied init hands the UOW to the provider, so let gc invoke
				// it, and keep the shell's proxy flag out of the evidence.
				env = filterEnv(env, "GC_DOLT")
				env = filterEnv(env, "BEADS_DOLT_PROXIED_SERVER")
			} else {
				// A direct scope would start and wait for managed Dolt here, so
				// defer it; gc still seeds the scope's canonical mode markers.
				env = replaceEnv(env, "GC_DOLT", "skip")
				env = replaceEnv(env, "BEADS_DOLT_PROXIED_SERVER", "1")
			}
			sourcePath := filepath.Join(t.TempDir(), "city.toml")
			if err := os.WriteFile(sourcePath, data, 0o644); err != nil {
				t.Fatal(err)
			}
			if out, err := runGCDoltWithEnv(env, "", "init", "--no-start", "--skip-provider-readiness", "--file", sourcePath, cityDir); err != nil {
				t.Fatalf("gc init: %v\n%s", err, out)
			}
			log, err := os.ReadFile(logPath)
			if err != nil && !os.IsNotExist(err) {
				t.Fatal(err)
			}
			var proxiedInits int
			for _, line := range strings.Split(strings.TrimSpace(string(log)), "\n") {
				if strings.HasPrefix(line, "init ") && strings.HasSuffix(line, "|proxied=1") {
					proxiedInits++
				}
			}
			wantInits := 0
			if tc.wantMode == "proxied-server" {
				wantInits = 1
			}
			if proxiedInits != wantInits {
				t.Fatalf("provider received %d proxied init request(s), want %d; log:\n%s", proxiedInits, wantInits, log)
			}
			beadsDir := filepath.Join(cityDir, ".beads")
			mode, ok, err := contract.ReadDoltMode(fsys.OSFS{}, filepath.Join(beadsDir, "metadata.json"))
			if err != nil || !ok || mode != tc.wantMode {
				t.Fatalf("metadata dolt_mode = %q (present %v, err %v), want %s", mode, ok, err, tc.wantMode)
			}
			cfg, ok, err := contract.ReadConfigState(fsys.OSFS{}, filepath.Join(beadsDir, "config.yaml"))
			if err != nil || !ok || cfg.DoltMode != tc.wantMode {
				t.Fatalf("config.yaml dolt.mode = %q (present %v, err %v), want %s", cfg.DoltMode, ok, err, tc.wantMode)
			}
			if tc.wantMode == "proxied-server" {
				for _, path := range []string{filepath.Join(beadsDir, "dolt-server.port"), filepath.Join(cityDir, ".gc", "runtime", "packs", "dolt")} {
					if _, err := os.Stat(path); err == nil {
						t.Fatalf("proxied init created GC-owned Dolt artifact %s", path)
					}
				}
			}
		})
	}
}
