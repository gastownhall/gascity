package scripts_test

import (
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// A qualification runner installs a real service. Bad inputs must be refused
// before it invokes any candidate or touches an existing user's directory.
func TestBeginnerQualifierRefusesUnsafeCandidates(t *testing.T) {
	for _, fault := range []string{"existing directory", "symlink directory", "missing companion", "occupied port", "changed published commands"} {
		t.Run(fault, func(t *testing.T) {
			base := t.TempDir()
			candidate := filepath.Join(base, "candidate")
			if err := os.Mkdir(candidate, 0o755); err != nil {
				t.Fatal(err)
			}
			marker := filepath.Join(base, "candidate-executed")
			gc := filepath.Join(candidate, "gc")
			writeExecutable(t, gc, "#!/bin/sh\nprintf called > '"+marker+"'\nexit 37\n")
			if fault != "missing companion" {
				writeExecutable(t, filepath.Join(candidate, "gc-osc7501"), "#!/bin/sh\nexit 37\n")
			}
			work := filepath.Join(base, "qualification")
			want := "work-dir must not exist"
			port := "18503"
			docs := filepath.Join(repoRoot(t), "docs")
			switch fault {
			case "existing directory", "symlink directory":
				retained := work
				if fault == "symlink directory" {
					retained = filepath.Join(base, "retained")
				}
				if err := os.Mkdir(retained, 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(retained, "user-data"), []byte("preserve"), 0o600); err != nil {
					t.Fatal(err)
				}
				if fault == "symlink directory" {
					if err := os.Symlink(retained, work); err != nil {
						t.Fatal(err)
					}
				}
			case "missing companion":
				want = "candidate companion"
			case "changed published commands":
				docs = filepath.Join(base, "docs")
				if err := os.MkdirAll(filepath.Join(docs, "getting-started"), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(docs, "getting-started", "quickstart.md"), []byte("```bash\ngc init --default-provider claude ~/bright-lights\n```\n"), 0o644); err != nil {
					t.Fatal(err)
				}
				listener, err := net.Listen("tcp", "127.0.0.1:0")
				if err != nil {
					t.Fatal(err)
				}
				port = strconv.Itoa(listener.Addr().(*net.TCPAddr).Port)
				if err := listener.Close(); err != nil {
					t.Fatal(err)
				}
				want = "published commands changed"
			case "occupied port":
				listener, err := net.Listen("tcp", "127.0.0.1:0")
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() {
					if err := listener.Close(); err != nil {
						t.Error(err)
					}
				})
				port = strconv.Itoa(listener.Addr().(*net.TCPAddr).Port)
				want = "supervisor port is unavailable"
			}
			cmd := exec.Command("python3", filepath.Join(repoRoot(t), "scripts", "qualify-beginner"), "--gc", gc, "--work-dir", work, "--port", port, "--docs-root", docs, "--expected-commit", strings.Repeat("a", 40))
			out, err := cmd.CombinedOutput()
			if err == nil || !strings.Contains(string(out), want) {
				t.Fatalf("unsafe setup was not refused clearly: %v\n%s", err, out)
			}
			if _, err := os.Stat(marker); !os.IsNotExist(err) {
				t.Fatalf("candidate ran before validation: %v", err)
			}
			if fault == "existing directory" || fault == "symlink directory" {
				data, err := os.ReadFile(filepath.Join(work, "user-data"))
				if err != nil || string(data) != "preserve" {
					t.Fatalf("existing data changed: %q %v", data, err)
				}
			} else if _, err := os.Lstat(work); !os.IsNotExist(err) {
				t.Fatalf("unsafe setup created work directory: %v", err)
			}
		})
	}
}

func TestBeginnerQualificationIsReleasePrerequisite(t *testing.T) {
	root := repoRoot(t)
	for _, name := range []string{"nightly.yml", "rc-gate.yml", "release.yml", "rc-release.yml"} {
		t.Run(name, func(t *testing.T) {
			var wf struct {
				Jobs map[string]struct {
					Uses  string            `yaml:"uses"`
					Needs any               `yaml:"needs"`
					With  map[string]string `yaml:"with"`
				} `yaml:"jobs"`
			}
			if err := yaml.Unmarshal([]byte(readFile(t, root, ".github/workflows/"+name)), &wf); err != nil {
				t.Fatal(err)
			}
			gate, ok := wf.Jobs["beginner_qualification"]
			if !ok || gate.Uses != "./.github/workflows/beginner-qualification.yml" {
				t.Fatal("installed beginner journey is not a required native workflow")
			}
			if name == "release.yml" || name == "rc-release.yml" {
				publish := "release"
				if name == "rc-release.yml" {
					publish = "rc-release"
				}
				needs := fmt.Sprint(wf.Jobs[publish].Needs)
				if !strings.Contains(needs, "beginner_qualification") {
					t.Fatal("publication can bypass beginner qualification")
				}
			}
			if name == "rc-gate.yml" && !strings.Contains(fmt.Sprint(wf.Jobs["rc_summary"].Needs), "beginner_qualification") {
				t.Fatal("RC summary can bypass beginner qualification")
			}
		})
	}
	body := readFile(t, root, ".github/workflows/beginner-qualification.yml")
	for _, required := range []string{"persist-credentials: false", "scripts/qualify-beginner", "--expected-commit", "gc-osc7501", "if: ${{ always() }}", "/evidence/", "contents: read", "deps.env"} {
		if !strings.Contains(body, required) {
			t.Errorf("native qualification is missing %q", required)
		}
	}
	if strings.Contains(body, "continue-on-error: true") {
		t.Fatal("failed beginner qualification may not pass")
	}
}
