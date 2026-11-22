//go:build !windows

package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/fsys"
)

const quarantineWarningFragment = `rig "bad" quarantined: its packs are not loaded`

// newQuarantineBdCity is the parity fixture (fake bd pinned through
// [workspace.env] PATH, canonical builtin imports) reshaped into the
// rig-quarantine city: rig "good" imports a healthy local pack, rig "bad" a
// locked remote pack whose cache has local worktree changes.
func newQuarantineBdCity(t *testing.T) (c bdParityCity, goodDir, badDir string) {
	t.Helper()
	c = newBdParityCity(t, bdParityOpts{})
	goodDir = filepath.Join(c.root, "rigs", "good")
	badDir = filepath.Join(c.root, "rigs", "bad")
	for _, d := range []string{filepath.Join(goodDir, ".beads"), filepath.Join(badDir, ".beads")} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	commit := seedDirtyBrokenCache(t, c.scratch, os.Getenv("GC_HOME"))
	writeQuarantineFile(t, c.root, "goodpack/pack.toml", "[pack]\nname = \"goodpack\"\nschema = 1\n\n[[agent]]\nname = \"worker\"\nscope = \"rig\"\n")
	cityTOML := "[workspace.env]\nPATH = " + strconv.Quote(c.bin+string(os.PathListSeparator)+"$PATH") + "\n" +
		"\n[[rigs]]\nname = \"good\"\nprefix = \"gd\"\n" +
		"\n[rigs.imports.tools]\nsource = \"../goodpack\"\n" +
		"\n[[rigs]]\nname = \"bad\"\nprefix = \"bd\"\n" +
		"\n[rigs.imports.broken]\nsource = " + strconv.Quote(quarantineBrokenSource) + "\nversion = \"^1.0\"\n"
	writeQuarantineFile(t, c.city, "city.toml", cityTOML)
	writeQuarantineFile(t, c.city, ".gc/site.toml", "workspace_name = \"demo\"\nworkspace_prefix = \"demo\"\n"+quarantineSiteBindings(goodDir, badDir))
	lock, err := os.ReadFile(filepath.Join(c.city, "packs.lock"))
	if err != nil {
		t.Fatal(err)
	}
	writeQuarantineFile(t, c.city, "packs.lock", string(lock)+brokenLockEntry(commit))
	return c, goodDir, badDir
}

func TestRigListJSONListsBothRigsWhenOneRigIsQuarantined(t *testing.T) {
	c, _, _ := newQuarantineBdCity(t)

	var stdout, stderr bytes.Buffer
	code := doRigList(fsys.OSFS{}, c.city, true, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("gc rig list --json = %d, want 0; stderr=%q", code, stderr.String())
	}
	if strings.Contains(stdout.String(), "quarantined") {
		t.Errorf("quarantine warning leaked to stdout: %q", stdout.String())
	}
	var out struct {
		Rigs []struct {
			Name string `json:"name"`
		} `json:"rigs"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &out); err != nil {
		t.Fatalf("stdout is not JSON: %v\n%s", err, stdout.String())
	}
	names := map[string]bool{}
	for _, r := range out.Rigs {
		names[r.Name] = true
	}
	if !names["good"] || !names["bad"] {
		t.Errorf("rig list = %v, want both good and bad", names)
	}
}

func TestGcBdRunsAgainstEachRigsStoreWhenOneRigIsQuarantined(t *testing.T) {
	for _, tc := range []struct {
		rig, prefix string
	}{{"good", "gd"}, {"bad", "bd"}} {
		t.Run(tc.rig, func(t *testing.T) {
			c, goodDir, badDir := newQuarantineBdCity(t)
			wantRoot := map[string]string{"good": goodDir, "bad": badDir}[tc.rig]

			var stdout, stderr bytes.Buffer
			code := doBd([]string{"--rig", tc.rig, "list"}, &stdout, &stderr)
			if code != 0 {
				t.Fatalf("gc bd --rig %s list = %d, want 0; stderr=%q", tc.rig, code, stderr.String())
			}
			log := readBdParityLog(t, c.scratch)
			for _, want := range []string{"argv=list", "GC_STORE_ROOT=" + wantRoot + "\n", "GC_BEADS_PREFIX=" + tc.prefix + "\n", "GC_RIG=" + tc.rig + "\n"} {
				if !strings.Contains(log, want) {
					t.Errorf("bd invocation missing %q:\n%s", want, log)
				}
			}
			if !strings.Contains(stderr.String(), quarantineWarningFragment) {
				t.Errorf("stderr = %q, want the quarantine warning", stderr.String())
			}
			if strings.Contains(stdout.String(), "quarantined") {
				t.Errorf("quarantine warning leaked to stdout: %q", stdout.String())
			}
		})
	}
}
