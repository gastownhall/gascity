//go:build windows

package builtinpacks

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// tamperMaterializedFileMode sets the read-only attribute, which is the only
// mode change Windows records. A narrowing chmod such as 0600 is not a tamper
// here: os.Chmod reads one bit of its argument and 0600 is writable, so it
// leaves the file exactly as it found it and the cache is genuinely unchanged.
func tamperMaterializedFileMode(t *testing.T, path string) {
	t.Helper()
	if err := os.Chmod(path, 0o444); err != nil {
		t.Fatalf("Chmod(%q): %v", path, err)
	}
	t.Cleanup(func() {
		// A read-only file left behind would survive the TempDir teardown on
		// filesystems where Remove does not clear the attribute first.
		_ = os.Chmod(path, 0o644)
	})
}

// TestCheckMaterializedFileModeAcceptsWindowsSynthesizedModes pins the reason
// this comparison is platform-split. Windows synthesizes 0666 for every
// writable file regardless of the mode it was created with, so a validator
// demanding equality with the manifest's 0644 or 0755 can never pass.
func TestCheckMaterializedFileModeAcceptsWindowsSynthesizedModes(t *testing.T) {
	for _, want := range []os.FileMode{0o644, 0o755} {
		if err := checkMaterializedFileMode(0o666, want); err != nil {
			t.Fatalf("checkMaterializedFileMode(0666, %04o) = %v, want nil", want, err)
		}
	}
}

// TestCheckMaterializedFileModeRejectsReadOnly pins that the check was narrowed
// to what Windows models rather than switched off: the one mode difference the
// platform records still fails, and the message says which side is wrong.
func TestCheckMaterializedFileModeRejectsReadOnly(t *testing.T) {
	err := checkMaterializedFileMode(0o444, 0o644)
	if err == nil {
		t.Fatal("checkMaterializedFileMode(0444, 0644) = nil, want a mode rejection")
	}
	if !strings.Contains(err.Error(), "has mode") || !strings.Contains(err.Error(), "writable") {
		t.Fatalf("error = %v, want it to name the mode and the expected writability", err)
	}
	if err := checkMaterializedFileMode(0o666, 0o444); err == nil {
		t.Fatal("checkMaterializedFileMode(0666, 0444) = nil, want a mode rejection")
	}
}

// TestValidateSyntheticRepoAcceptsFreshlyMaterializedCache is the unit-level
// form of the gc init failure this split fixes: a cache this binary just wrote
// must validate on the platform that wrote it.
func TestValidateSyntheticRepoAcceptsFreshlyMaterializedCache(t *testing.T) {
	dst := materializeTestRepo(t)
	if err := ValidateSyntheticRepo(dst, Repository, testCommit); err != nil {
		t.Fatalf("freshly materialized cache is invalid: %v", err)
	}
}

// TestValidateSyntheticRepoRejectsReadOnlyCacheFile carries the integrity
// property end to end on Windows: flipping a cached file to read-only is
// detected by the full validator, not just by the comparison in isolation.
func TestValidateSyntheticRepoRejectsReadOnlyCacheFile(t *testing.T) {
	dst := materializeTestRepo(t)
	target := filepath.Join(dst, filepath.FromSlash("internal/bootstrap/packs/core/pack.toml"))
	tamperMaterializedFileMode(t, target)

	err := ValidateSyntheticRepo(dst, Repository, testCommit)
	if err == nil {
		t.Fatal("ValidateSyntheticRepo accepted a read-only cache file")
	}
	if !strings.Contains(err.Error(), "has mode") {
		t.Fatalf("error = %v, want a mode rejection", err)
	}
}

// grantEveryone widens a file's DACL so a principal that is neither the owner
// nor a system account holds rights, in icacls notation, over it. No elevation
// is needed: the owner of a file may rewrite its own DACL. The principal is
// named by SID so the test does not depend on the machine's display language.
func grantEveryone(t *testing.T, path, rights string) {
	t.Helper()
	out, err := exec.Command("icacls.exe", path, "/grant", "*S-1-1-0:"+rights).CombinedOutput()
	if err != nil {
		t.Fatalf("widening the DACL of %q: %v: %s", path, err, out)
	}
}

// TestValidateSyntheticRepoRejectsAWidenedDACL is the Windows form of the Unix
// case where a cached file turns group- or other-writable. The synthesized mode
// carries no trace of the widening — the file still reports 0666 — so the
// refusal has to come from the DACL.
func TestValidateSyntheticRepoRejectsAWidenedDACL(t *testing.T) {
	for _, rights := range []string{"(W)", "(M)", "(D)", "(WDAC)"} {
		t.Run(rights, func(t *testing.T) {
			dst := materializeTestRepo(t)
			target := filepath.Join(dst, filepath.FromSlash("internal/bootstrap/packs/core/pack.toml"))
			grantEveryone(t, target, rights)

			info, err := os.Lstat(target)
			if err != nil {
				t.Fatalf("Lstat(%q): %v", target, err)
			}
			if info.Mode().Perm()&0o200 == 0 {
				t.Fatalf("widening the DACL made the file read-only (%s); the mode check would refuse it and this case would prove nothing", info.Mode().Perm())
			}
			err = ValidateSyntheticRepo(dst, Repository, testCommit)
			if err == nil {
				t.Fatalf("ValidateSyntheticRepo accepted a cache file that grants Everyone %s", rights)
			}
			if !strings.Contains(err.Error(), "S-1-1-0") || !strings.Contains(err.Error(), "pack.toml") {
				t.Fatalf("error = %v, want it to name the file and the principal", err)
			}
		})
	}
}

// TestValidateSyntheticRepoAcceptsAReadGrant keeps the DACL check to what the
// Unix mode forbids: another account reading a cached file is what its 0644
// already allows.
func TestValidateSyntheticRepoAcceptsAReadGrant(t *testing.T) {
	dst := materializeTestRepo(t)
	target := filepath.Join(dst, filepath.FromSlash("internal/bootstrap/packs/core/pack.toml"))
	grantEveryone(t, target, "(RX)")

	if err := ValidateSyntheticRepo(dst, Repository, testCommit); err != nil {
		t.Fatalf("a cache file others may only read was refused: %v", err)
	}
}
