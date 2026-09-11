package scripts_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestShardScriptsApplyLinuxCGOFallback(t *testing.T) {
	for _, tt := range []struct {
		name   string
		script string
		args   []string
	}{
		{
			name:   "go-test-shard",
			script: "test-go-test-shard",
			args:   []string{"./cmd/gc", "1", "1"},
		},
		{
			name:   "integration-shard",
			script: "test-integration-shard",
			args:   []string{"bdstore"},
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			repoRoot := repoRoot(t)
			fixture := newShardCGOFixture(t)

			cmd := exec.Command(filepath.Join(repoRoot, "scripts", tt.script), tt.args...)
			cmd.Dir = repoRoot
			cmd.Env = fixture.env(t)

			out, err := cmd.CombinedOutput()
			if err != nil {
				t.Fatalf("%s failed: %v\n%s", tt.script, err, out)
			}

			got := readCapturedGoEnv(t, fixture.capturePath)
			if got["CGO_CPPFLAGS"] != "-I"+fixture.sysInclude {
				t.Fatalf("CGO_CPPFLAGS=%q, want %q", got["CGO_CPPFLAGS"], "-I"+fixture.sysInclude)
			}
			assertFieldsInOrder(t, got["CGO_LDFLAGS"],
				"-L"+fixture.multiarchLib,
				"-L"+fixture.sysLib64,
				"-L"+fixture.sysLib,
			)
			for _, flag := range []string{"-L" + fixture.multiarchLib, "-L" + fixture.sysLib64, "-L" + fixture.sysLib} {
				if count := countExactField(got["CGO_LDFLAGS"], flag); count != 1 {
					t.Fatalf("%s appears %d times, want 1 in CGO_LDFLAGS=%q", flag, count, got["CGO_LDFLAGS"])
				}
			}
		})
	}
}

func TestShardScriptsDisableLinuxCGOFallback(t *testing.T) {
	for _, tt := range []struct {
		name   string
		script string
		args   []string
	}{
		{
			name:   "go-test-shard",
			script: "test-go-test-shard",
			args:   []string{"./cmd/gc", "1", "1"},
		},
		{
			name:   "integration-shard",
			script: "test-integration-shard",
			args:   []string{"bdstore"},
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			repoRoot := repoRoot(t)
			fixture := newShardCGOFixture(t)

			cmd := exec.Command(filepath.Join(repoRoot, "scripts", tt.script), tt.args...)
			cmd.Dir = repoRoot
			cmd.Env = append(fixture.env(t), "SYS_USR_CGO_FALLBACK=0")

			out, err := cmd.CombinedOutput()
			if err != nil {
				t.Fatalf("%s failed: %v\n%s", tt.script, err, out)
			}

			got := readCapturedGoEnv(t, fixture.capturePath)
			if got["CGO_CPPFLAGS"] != "" {
				t.Fatalf("CGO_CPPFLAGS=%q, want empty", got["CGO_CPPFLAGS"])
			}
			if got["CGO_LDFLAGS"] != "" {
				t.Fatalf("CGO_LDFLAGS=%q, want empty", got["CGO_LDFLAGS"])
			}
		})
	}
}

type shardCGOFixture struct {
	tmp          string
	binDir       string
	capturePath  string
	sysInclude   string
	sysLib       string
	sysLib64     string
	multiarchLib string
}

func newShardCGOFixture(t *testing.T) shardCGOFixture {
	t.Helper()

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
	multiarchLib := filepath.Join(sysLib, "riscv64-linux-gnu")
	for _, dir := range []string{multiarchLib, sysLib64, sysLib} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", dir, err)
		}
	}

	writeExecutable(t, filepath.Join(binDir, "uname"), "#!/usr/bin/env sh\necho Linux\n")
	writeExecutable(t, filepath.Join(binDir, "dpkg-architecture"), "#!/usr/bin/env sh\necho riscv64-linux-gnu\n")
	writeExecutable(t, filepath.Join(binDir, "cc"), `#!/usr/bin/env sh
case " $* " in
  *" -print-multiarch "*)
    echo riscv64-linux-gnu
    exit 0
    ;;
esac
printf '%s\n' '#include <...> search starts here:' ' /nix/store/include' 'End of search list.' >&2
`)
	capturePath := filepath.Join(tmp, "go-env.capture")
	writeExecutable(t, filepath.Join(binDir, "go"), `#!/usr/bin/env bash
set -euo pipefail

fake_go_root=`+shellQuote(tmp)+`
fake_go_capture=`+shellQuote(capturePath)+`

case "$1" in
  env)
    case "$2" in
      GOPATH) echo "$fake_go_root/gopath" ;;
      GOCACHE) echo "$fake_go_root/gocache" ;;
      GOMODCACHE) echo "$fake_go_root/gomodcache" ;;
      GOTMPDIR) echo "" ;;
      GOROOT) echo "$fake_go_root/goroot" ;;
      *) echo "unexpected go env key: $2" >&2; exit 1 ;;
    esac
    ;;
  test)
    for arg in "$@"; do
      if [[ "$arg" == "-list" ]]; then
        echo TestSynthetic
        exit 0
      fi
    done
    {
      printf 'CGO_CPPFLAGS=%s\n' "${CGO_CPPFLAGS-}"
      printf 'CGO_LDFLAGS=%s\n' "${CGO_LDFLAGS-}"
    } >> "$fake_go_capture"
    ;;
  *)
    echo "unexpected go command: $*" >&2
    exit 1
    ;;
esac
`)

	return shardCGOFixture{
		tmp:          tmp,
		binDir:       binDir,
		capturePath:  capturePath,
		sysInclude:   sysInclude,
		sysLib:       sysLib,
		sysLib64:     sysLib64,
		multiarchLib: multiarchLib,
	}
}

func (f shardCGOFixture) env(t *testing.T) []string {
	t.Helper()
	return []string{
		"PATH=" + f.binDir + string(os.PathListSeparator) + os.Getenv("PATH"),
		"HOME=" + filepath.Join(f.tmp, "home"),
		"SYS_USR_INCLUDE=" + f.sysInclude,
		"SYS_USR_LIB_ROOT=" + f.sysLib,
		"SYS_USR_LIB64_ROOT=" + f.sysLib64,
		"CC=cc",
	}
}

func readCapturedGoEnv(t *testing.T, path string) map[string]string {
	t.Helper()
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read captured go env: %v", err)
	}
	got := make(map[string]string)
	for _, line := range strings.Split(string(content), "\n") {
		if line == "" {
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			t.Fatalf("captured env line missing '=': %q", line)
		}
		got[key] = value
	}
	return got
}
