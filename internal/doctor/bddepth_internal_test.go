//go:build !windows

package doctor

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
)

// The recursion fuse (ga-kihcmff, incident ga-2t6u9dy) at the three doctor
// seams that launch a bd binary themselves: the custom-types command builder,
// the proxied-backup status reader and the Dolt remote remover. Each stamps
// GC_BD_DEPTH on the child and refuses at the bound, before launching.

func setDoctorBdDepth(t *testing.T, value string) {
	t.Helper()
	t.Setenv(beads.BdDepthEnvKey, value)
}

// writeDoctorDepthStub writes a bd that appends the GC_BD_DEPTH it saw to a
// log; it uses only shell builtins, so it runs under an empty PATH.
func writeDoctorDepthStub(t *testing.T, dir string) (stub, log string) {
	t.Helper()
	stub = filepath.Join(dir, "bd-depth-stub")
	log = filepath.Join(dir, "bd-depth-stub.log")
	script := "#!/bin/sh\nprintf 'depth=%s\\n' \"$GC_BD_DEPTH\" >> '" + log + "'\nprintf '{}'\n"
	if err := os.WriteFile(stub, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	return stub, log
}

func doctorStubRuns(t *testing.T, log string) []string {
	t.Helper()
	data, err := os.ReadFile(log)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		t.Fatal(err)
	}
	return strings.Split(strings.TrimSpace(string(data)), "\n")
}

func TestCustomTypesBDCommandStampsAndRefuses(t *testing.T) {
	dir := guardedTempDir(t)
	writeProxiedLocalScope(t, dir, "hq")
	ctx := &CheckContext{CityPath: dir}

	t.Run("stamps the child env last", func(t *testing.T) {
		setDoctorBdDepth(t, "3")
		cmd, err := customTypesBDCommand(ctx, "/opt/pinned/bd", dir, "config", "get", "--json", "types.custom")
		if err != nil {
			t.Fatalf("customTypesBDCommand at depth 3: %v", err)
		}
		if len(cmd.Env) == 0 || cmd.Env[len(cmd.Env)-1] != beads.BdDepthEnvKey+"=4" {
			t.Fatalf("last child env entry = %v, want %s=4", cmd.Env[max(0, len(cmd.Env)-1):], beads.BdDepthEnvKey)
		}
	})
	t.Run("refuses at the bound and names the pinned bd", func(t *testing.T) {
		setDoctorBdDepth(t, "16")
		cmd, err := customTypesBDCommand(ctx, "/opt/pinned/bd", dir, "config", "get", "--json", "types.custom")
		if err == nil {
			t.Fatalf("customTypesBDCommand at depth 16 built %v, want a refusal", cmd)
		}
		var depthErr *beads.BdDepthError
		if !errors.As(err, &depthErr) {
			t.Fatalf("error %T %q is not a *beads.BdDepthError", err, err)
		}
		for _, want := range []string{"/opt/pinned/bd", beads.BdDepthEnvKey, "16"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("error %q does not name %q", err, want)
			}
		}
	})
	t.Run("an unpinned bd is named as bd", func(t *testing.T) {
		setDoctorBdDepth(t, "16")
		if _, err := customTypesBDCommand(ctx, "", dir, "config", "get"); err == nil {
			t.Fatal("unpinned customTypesBDCommand at depth 16 built a command, want a refusal")
		}
	})
}

func TestReadBdBackupStatusStampsAndRefuses(t *testing.T) {
	t.Run("stamps", func(t *testing.T) {
		dir := guardedTempDir(t)
		writeProxiedLocalScope(t, dir, "hq")
		t.Setenv("PATH", filepath.Join(dir, "empty-path"))
		stub, log := writeDoctorDepthStub(t, dir)
		setDoctorBdDepth(t, "3")
		if _, err := readBdBackupStatus(context.Background(), &CheckContext{CityPath: dir}, stub, dir); err != nil {
			t.Fatalf("readBdBackupStatus at depth 3: %v", err)
		}
		if got := doctorStubRuns(t, log); len(got) != 1 || got[0] != "depth=4" {
			t.Fatalf("stub saw %v, want one run with depth=4", got)
		}
	})
	t.Run("refuses before launching", func(t *testing.T) {
		dir := guardedTempDir(t)
		writeProxiedLocalScope(t, dir, "hq")
		t.Setenv("PATH", filepath.Join(dir, "empty-path"))
		stub, log := writeDoctorDepthStub(t, dir)
		setDoctorBdDepth(t, "16")
		_, err := readBdBackupStatus(context.Background(), &CheckContext{CityPath: dir}, stub, dir)
		if err == nil {
			t.Fatal("readBdBackupStatus at depth 16 succeeded, want a refusal")
		}
		for _, want := range []string{stub, beads.BdDepthEnvKey} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("error %q does not name %q", err, want)
			}
		}
		if runs := doctorStubRuns(t, log); len(runs) != 0 {
			t.Fatalf("stub ran at the bound: %v", runs)
		}
	})
}

func TestRemoveDoltRemoteStampsAndRefuses(t *testing.T) {
	t.Run("stamps", func(t *testing.T) {
		bin := t.TempDir()
		_, log := writeDoctorDepthStub(t, bin)
		if err := os.Rename(filepath.Join(bin, "bd-depth-stub"), filepath.Join(bin, "bd")); err != nil {
			t.Fatal(err)
		}
		t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
		setDoctorBdDepth(t, "3")
		if err := removeDoltRemote(t.TempDir(), "origin"); err != nil {
			t.Fatalf("removeDoltRemote at depth 3: %v", err)
		}
		if got := doctorStubRuns(t, log); len(got) != 1 || got[0] != "depth=4" {
			t.Fatalf("stub saw %v, want one run with depth=4", got)
		}
	})
	t.Run("refuses before launching", func(t *testing.T) {
		bin := t.TempDir()
		_, log := writeDoctorDepthStub(t, bin)
		if err := os.Rename(filepath.Join(bin, "bd-depth-stub"), filepath.Join(bin, "bd")); err != nil {
			t.Fatal(err)
		}
		t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
		setDoctorBdDepth(t, "16")
		err := removeDoltRemote(t.TempDir(), "origin")
		if err == nil || !strings.Contains(err.Error(), beads.BdDepthEnvKey) {
			t.Fatalf("removeDoltRemote at depth 16 error = %v, want a refusal naming %s", err, beads.BdDepthEnvKey)
		}
		if runs := doctorStubRuns(t, log); len(runs) != 0 {
			t.Fatalf("stub ran at the bound: %v", runs)
		}
	})
}
