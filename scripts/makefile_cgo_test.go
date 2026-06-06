package scripts_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestMakefileLinuxCGOPathsIncludeExistingSystemLibLayouts(t *testing.T) {
	t.Setenv("CGO_CPPFLAGS", "-Iambient")
	t.Setenv("CGO_LDFLAGS", "-Lambient")

	repoRoot := repoRoot(t)
	tmp := t.TempDir()
	binDir := filepath.Join(tmp, "bin")
	if err := os.Mkdir(binDir, 0o755); err != nil {
		t.Fatalf("mkdir bin: %v", err)
	}
	sysInclude := filepath.Join(tmp, "sysinclude")
	if err := os.MkdirAll(filepath.Join(sysInclude, "unicode"), 0o755); err != nil {
		t.Fatalf("mkdir unicode include: %v", err)
	}
	if err := os.WriteFile(filepath.Join(sysInclude, "unicode", "uregex.h"), []byte(""), 0o644); err != nil {
		t.Fatalf("write ICU header: %v", err)
	}
	sysLib := filepath.Join(tmp, "syslib")
	sysLib64 := filepath.Join(tmp, "syslib64")
	for _, dir := range []string{filepath.Join(sysLib, "riscv64-linux-gnu"), sysLib, sysLib64} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", dir, err)
		}
	}

	writeExecutable(t, filepath.Join(binDir, "uname"), "#!/usr/bin/env sh\necho Linux\n")
	writeExecutable(t, filepath.Join(binDir, "dpkg-architecture"), "#!/usr/bin/env sh\nexit 1\n")
	writeExecutable(t, filepath.Join(binDir, "cc"), `#!/usr/bin/env sh
case " $* " in
  *" -print-multiarch "*)
    echo riscv64-linux-gnu
    exit 0
    ;;
esac
printf '%s\n' '#include <...> search starts here:' ' /nix/store/include' 'End of search list.' >&2
`)

	out := runMakefileCGOPrintTarget(t, repoRoot, tmp, binDir,
		"SYS_USR_INCLUDE="+sysInclude,
		"SYS_USR_LIB_ROOT="+sysLib,
		"SYS_USR_LIB64_ROOT="+sysLib64,
		"CC=cc",
	)
	assertContains(t, out, "CGO_CPPFLAGS=-I"+sysInclude)
	assertContains(t, out, "-L"+filepath.Join(sysLib, "riscv64-linux-gnu"))
	assertContains(t, out, "-L"+sysLib64)
	assertContains(t, out, "-L"+sysLib)
	if strings.Contains(out, "x86_64-linux-gnu") {
		t.Fatalf("CGO paths should not hardcode the x86_64 Debian fallback:\n%s", out)
	}
	if strings.Contains(out, "ambient") {
		t.Fatalf("Makefile CGO tests should not inherit ambient CGO flags:\n%s", out)
	}
}

func TestMakefileLinuxCGOPathsTreatNestedIncludeAsMissingSystemRoot(t *testing.T) {
	repoRoot := repoRoot(t)
	tmp := t.TempDir()
	binDir := filepath.Join(tmp, "bin")
	if err := os.Mkdir(binDir, 0o755); err != nil {
		t.Fatalf("mkdir bin: %v", err)
	}
	sysInclude := filepath.Join(tmp, "sysinclude")
	if err := os.MkdirAll(filepath.Join(sysInclude, "unicode"), 0o755); err != nil {
		t.Fatalf("mkdir unicode include: %v", err)
	}
	if err := os.WriteFile(filepath.Join(sysInclude, "unicode", "uregex.h"), []byte(""), 0o644); err != nil {
		t.Fatalf("write ICU header: %v", err)
	}
	sysLib := filepath.Join(tmp, "syslib")
	if err := os.MkdirAll(sysLib, 0o755); err != nil {
		t.Fatalf("mkdir syslib: %v", err)
	}

	writeExecutable(t, filepath.Join(binDir, "uname"), "#!/usr/bin/env sh\necho Linux\n")
	writeExecutable(t, filepath.Join(binDir, "dpkg-architecture"), "#!/usr/bin/env sh\nexit 1\n")
	writeExecutable(t, filepath.Join(binDir, "cc"), `#!/usr/bin/env sh
case " $* " in
  *" -print-multiarch "*)
    exit 0
    ;;
esac
printf '%s\n' '#include <...> search starts here:' ' `+sysInclude+`/x86_64-linux-gnu' 'End of search list.' >&2
`)

	out := runMakefileCGOPrintTarget(t, repoRoot, tmp, binDir,
		"SYS_USR_INCLUDE="+sysInclude,
		"SYS_USR_LIB_ROOT="+sysLib,
		"SYS_USR_LIB64_ROOT="+filepath.Join(tmp, "syslib64"),
		"CC=cc",
	)
	assertContains(t, out, "CGO_CPPFLAGS=-I"+sysInclude)
	assertContains(t, out, "-L"+sysLib)
}

func TestMakefileLinuxCGOPathsSkipDetectionWhenICUHeaderMissing(t *testing.T) {
	repoRoot := repoRoot(t)
	tmp := t.TempDir()
	binDir := filepath.Join(tmp, "bin")
	if err := os.Mkdir(binDir, 0o755); err != nil {
		t.Fatalf("mkdir bin: %v", err)
	}
	sysInclude := filepath.Join(tmp, "sysinclude")
	if err := os.MkdirAll(sysInclude, 0o755); err != nil {
		t.Fatalf("mkdir sysinclude: %v", err)
	}
	writeExecutable(t, filepath.Join(binDir, "uname"), "#!/usr/bin/env sh\necho Linux\n")
	writeExecutable(t, filepath.Join(binDir, "dpkg-architecture"), `#!/usr/bin/env sh
touch "$(dirname "$0")/dpkg.ran"
echo x86_64-linux-gnu
`)
	writeExecutable(t, filepath.Join(binDir, "cc"), `#!/usr/bin/env sh
touch "$(dirname "$0")/cc.ran"
exit 0
`)

	out := runMakefileCGOPrintTarget(t, repoRoot, tmp, binDir,
		"SYS_USR_INCLUDE="+sysInclude,
		"SYS_USR_LIB_ROOT="+filepath.Join(tmp, "syslib"),
		"SYS_USR_LIB64_ROOT="+filepath.Join(tmp, "syslib64"),
		"CC=cc",
	)
	if out != "CGO_CPPFLAGS=\nCGO_LDFLAGS=\n" {
		t.Fatalf("CGO flags should stay empty when the ICU header is absent:\n%s", out)
	}
	for _, name := range []string{"dpkg.ran", "cc.ran"} {
		if _, err := os.Stat(filepath.Join(binDir, name)); err == nil {
			t.Fatalf("Makefile should not run %s when the ICU header is absent", strings.TrimSuffix(name, ".ran"))
		} else if !os.IsNotExist(err) {
			t.Fatalf("stat %s: %v", name, err)
		}
	}
}

func runMakefileCGOPrintTarget(t *testing.T, repoRoot, tmp, binDir string, args ...string) string {
	t.Helper()
	makefile, err := os.ReadFile(filepath.Join(repoRoot, "Makefile"))
	if err != nil {
		t.Fatalf("read Makefile: %v", err)
	}
	testMakefile := filepath.Join(tmp, "Makefile")
	makefileContent := string(makefile) + `
.PHONY: print-cgo-flags
print-cgo-flags:
	@printf 'CGO_CPPFLAGS=%s\n' '$(CGO_CPPFLAGS)'
	@printf 'CGO_LDFLAGS=%s\n' '$(CGO_LDFLAGS)'
`
	if err := os.WriteFile(testMakefile, []byte(makefileContent), 0o644); err != nil {
		t.Fatalf("write test Makefile: %v", err)
	}

	cmdArgs := append([]string{"--no-print-directory", "-f", testMakefile, "print-cgo-flags"}, args...)
	cmd := exec.Command("make", cmdArgs...)
	cmd.Dir = repoRoot
	cmd.Env = append(filteredMakefileCGOTestEnv(),
		"PATH="+binDir+string(os.PathListSeparator)+os.Getenv("PATH"),
		"HOME="+filepath.Join(tmp, "home"),
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("make print-cgo-flags failed: %v\n%s", err, out)
	}
	return string(out)
}

func filteredMakefileCGOTestEnv() []string {
	env := os.Environ()
	filtered := make([]string, 0, len(env))
	for _, entry := range env {
		if strings.HasPrefix(entry, "CGO_") {
			continue
		}
		filtered = append(filtered, entry)
	}
	return filtered
}

func assertContains(t *testing.T, haystack, needle string) {
	t.Helper()
	if !strings.Contains(haystack, needle) {
		t.Fatalf("output missing %q:\n%s", needle, haystack)
	}
}
