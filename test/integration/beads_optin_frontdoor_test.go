//go:build integration

package integration

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestGasCityBeadsModeSelectorFrontDoor runs the production gc binary with a
// recording provider. It proves that fresh scopes select direct/server by
// default and only select Beads' proxied init when city.toml opts in.
func TestGasCityBeadsModeSelectorFrontDoor(t *testing.T) {
	for _, tc := range []struct {
		name, mode string
	}{
		{name: "direct default"},
		{name: "explicit proxied", mode: "proxied-server"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cityDir := t.TempDir()
			if err := os.WriteFile(filepath.Join(cityDir, "pack.toml"), []byte("[pack]\nname=\"mode-frontdoor\"\nschema=2\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			logPath := filepath.Join(cityDir, "provider.log")
			provider := filepath.Join(cityDir, "provider.sh")
			mode := tc.mode
			if mode == "" {
				mode = "server"
			}
			script := "#!/bin/sh\nprintf '%s\\n' \"$*\" >> \"" + logPath + "\"\ndir=\"$2\"\nmkdir -p \"$dir/.beads\"\nprintf '{\"backend\":\"dolt\",\"database\":\"dolt\",\"dolt_mode\":\"" + mode + "\"}' > \"$dir/.beads/metadata.json\"\nexit 0\n"
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
			env = append(env, "GC_DOLT=skip")
			if tc.mode == "" {
				env = append(env, "BEADS_DOLT_PROXIED_SERVER=1")
			}
			sourcePath := filepath.Join(t.TempDir(), "city.toml")
			if err := os.WriteFile(sourcePath, data, 0o644); err != nil {
				t.Fatal(err)
			}
			if out, err := runGCDoltWithEnv(env, "", "init", "--no-start", "--skip-provider-readiness", "--file", sourcePath, cityDir); err != nil {
				t.Fatalf("gc init: %v\n%s", err, out)
			}
			log, err := os.ReadFile(logPath)
			if err != nil {
				t.Fatal(err)
			}
			if len(log) == 0 {
				t.Fatal("provider log is empty")
			}
			meta, err := os.ReadFile(filepath.Join(cityDir, ".beads", "metadata.json"))
			if err != nil {
				t.Fatal(err)
			}
			var got map[string]any
			if err := json.Unmarshal(meta, &got); err != nil {
				t.Fatal(err)
			}
			wantMode := "server"
			if tc.mode != "" {
				wantMode = tc.mode
			}
			if got["dolt_mode"] != wantMode {
				t.Fatalf("metadata dolt_mode = %v, want %s", got["dolt_mode"], wantMode)
			}
			if tc.mode == "proxied-server" {
				for _, path := range []string{filepath.Join(cityDir, ".beads", "dolt-server.port"), filepath.Join(cityDir, ".gc", "runtime", "packs", "dolt")} {
					if _, err := os.Stat(path); err == nil {
						t.Fatalf("proxied init created GC-owned Dolt artifact %s", path)
					}
				}
			}
		})
	}
}
