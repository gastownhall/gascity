package scripts_test

import (
	"encoding/json"
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
	for _, fault := range []string{"existing directory", "symlink directory", "missing companion", "occupied port", "changed published commands", "changed parallel commands"} {
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
			case "changed published commands", "changed parallel commands":
				docs = filepath.Join(base, "docs")
				if err := os.MkdirAll(filepath.Join(docs, "getting-started"), 0o755); err != nil {
					t.Fatal(err)
				}
				if fault == "changed parallel commands" {
					for _, name := range []string{"getting-started/quickstart.md", "tutorials/02-agents.md", "tutorials/05-formulas.md", "tutorials/07-orders.md"} {
						data, err := os.ReadFile(filepath.Join(repoRoot(t), "docs", name))
						if err != nil {
							t.Fatal(err)
						}
						if name == "tutorials/07-orders.md" {
							data = []byte(strings.ReplaceAll(string(data), "cat > orders/practice-client-check.toml", "cat > orders/renamed-client-check.toml"))
						}
						target := filepath.Join(docs, name)
						if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
							t.Fatal(err)
						}
						if err := os.WriteFile(target, data, 0o644); err != nil {
							t.Fatal(err)
						}
					}
				} else if err := os.WriteFile(filepath.Join(docs, "getting-started", "quickstart.md"), []byte("```bash\ngc init --default-provider claude ~/bright-lights\n```\n"), 0o644); err != nil {
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
				if fault == "changed parallel commands" {
					want = "published parallel-check exercise is missing or ambiguous"
				}
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
	t.Run("distinct runtime namespace", func(t *testing.T) {
		base := t.TempDir()
		baseline := filepath.Join(base, "bright-lights")
		if err := os.Mkdir(baseline, 0o755); err != nil {
			t.Fatal(err)
		}
		marker := filepath.Join(baseline, "user-data")
		if err := os.WriteFile(marker, []byte("preserve"), 0o600); err != nil {
			t.Fatal(err)
		}
		candidate := filepath.Join(base, "candidate")
		if err := os.Mkdir(candidate, 0o755); err != nil {
			t.Fatal(err)
		}
		gc := filepath.Join(candidate, "gc")
		writeExecutable(t, filepath.Join(candidate, "gc-osc7501"), "#!/bin/sh\nexit 37\n")
		cities := map[string]bool{}
		for _, parent := range []string{"first", "second"} {
			// Matching work-directory basenames must still receive different
			// tmux namespaces, even when profiles and supervisor ports differ.
			work := filepath.Join(base, parent, "qualification")
			capture := filepath.Join(base, parent+"-invocation.json")
			writeExecutable(t, gc, fmt.Sprintf("#!/usr/bin/env python3\nimport json, os, sys\nwith open(%q, 'w') as f: json.dump({'args': sys.argv[1:], 'profile': os.environ['GC_HOME']}, f)\nsys.exit(37)\n", capture))
			listener, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			port := strconv.Itoa(listener.Addr().(*net.TCPAddr).Port)
			if err := listener.Close(); err != nil {
				t.Fatal(err)
			}
			cmd := exec.Command("python3", filepath.Join(repoRoot(t), "scripts", "qualify-beginner"), "--gc", gc, "--work-dir", work, "--port", port, "--docs-root", filepath.Join(repoRoot(t), "docs"), "--expected-commit", strings.Repeat("a", 40))
			out, err := cmd.CombinedOutput()
			if err == nil || !strings.Contains(string(out), "init exit=37") {
				t.Fatalf("candidate must stop at initial argument capture: %v\n%s", err, out)
			}
			data, err := os.ReadFile(capture)
			if err != nil {
				t.Fatal(err)
			}
			var invocation struct {
				Args    []string
				Profile string
			}
			if err := json.Unmarshal(data, &invocation); err != nil {
				t.Fatal(err)
			}
			if len(invocation.Args) == 0 || invocation.Args[0] != "init" {
				t.Fatalf("unexpected initial candidate command: %#v", invocation)
			}
			city := invocation.Args[len(invocation.Args)-1]
			name := filepath.Base(city)
			suffix := strings.TrimPrefix(name, "bright-lights-")
			if filepath.Dir(city) != work || len(suffix) != 32 || strings.Trim(suffix, "0123456789abcdef") != "" {
				t.Errorf("candidate city must have its own UUID runtime namespace: %q", city)
			}
			if cities[name] {
				t.Errorf("two profiles reused runtime namespace %q", name)
			}
			cities[name] = true
			if invocation.Profile != filepath.Join(work, "gc-home") {
				t.Errorf("candidate profile escaped own work directory: %q", invocation.Profile)
			}
		}
		data, err := os.ReadFile(marker)
		if err != nil || string(data) != "preserve" {
			t.Fatalf("existing bright-lights data changed: %q %v", data, err)
		}
	})
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
