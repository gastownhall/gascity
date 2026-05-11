package examples_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestShippedExamplesDoNotHardcodeShortRoutedToPools(t *testing.T) {
	_, filename, _, _ := runtime.Caller(0)
	root := filepath.Dir(filename)
	badRoutes := []string{
		"gc.routed_to=dog",
		"gc.routed_to=worker",
		"gc.routed_to=<rig>/polecat",
		"gc.routed_to=<rig>/refinery",
		"gc.routed_to={{ .RigName }}/refinery",
		"pool:dog",
	}

	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		body := string(data)
		for _, bad := range badRoutes {
			if strings.Contains(body, bad) {
				t.Errorf("%s contains short-form routed_to target %q", path, bad)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestExamplePoolScriptsUseCanonicalGCTemplateRoutes(t *testing.T) {
	root := examplesRoot(t)

	tests := []struct {
		name     string
		rel      string
		template string
		want     string
	}{
		{
			name:     "hyperscale worker",
			rel:      "hyperscale/packs/hyperscale/assets/scripts/mock-worker.sh",
			template: "demo/hyperscale.worker",
			want:     "demo/hyperscale.worker",
		},
		{
			name:     "lifecycle polecat",
			rel:      "lifecycle/packs/lifecycle/assets/scripts/mock-polecat.sh",
			template: "demo/lifecycle.polecat",
			want:     "demo/lifecycle.polecat",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := filepath.Join(root, tt.rel)
			assignment := shellLineContaining(t, path, "POOL_LABEL=")
			got := runShell(t, []string{"GC_TEMPLATE=" + tt.template}, assignment+`
printf '%s' "$POOL_LABEL"
`)
			if got != tt.want {
				t.Fatalf("POOL_LABEL = %q, want %q", got, tt.want)
			}

			cmd := shellCommand(t, nil, assignment)
			if err := cmd.Run(); err == nil {
				t.Fatalf("POOL_LABEL assignment succeeded without GC_TEMPLATE")
			}
		})
	}
}

func TestLifecyclePolecatDerivesRefineryTargetFromCanonicalTemplate(t *testing.T) {
	root := examplesRoot(t)
	path := filepath.Join(root, "lifecycle/packs/lifecycle/assets/scripts/mock-polecat.sh")
	function := shellFunction(t, path, "derive_refinery_target")

	tests := []struct {
		name     string
		template string
		want     string
	}{
		{
			name:     "v1 template",
			template: "demo/polecat",
			want:     "demo/refinery",
		},
		{
			name:     "binding qualified template",
			template: "demo/lifecycle.polecat",
			want:     "demo/lifecycle.refinery",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := runShell(t, []string{"GC_TEMPLATE=" + tt.template}, function+`
derive_refinery_target
`)
			if got != tt.want {
				t.Fatalf("derive_refinery_target() = %q, want %q", got, tt.want)
			}
		})
	}

	cmd := shellCommand(t, []string{"GC_TEMPLATE=demo/lifecycle.worker"}, function+`
derive_refinery_target
`)
	if err := cmd.Run(); err == nil {
		t.Fatalf("derive_refinery_target succeeded for a non-polecat template")
	}
}

func TestLifecycleRefineryConsumesPolecatHandoffAlias(t *testing.T) {
	root := examplesRoot(t)
	polecatPath := filepath.Join(root, "lifecycle/packs/lifecycle/assets/scripts/mock-polecat.sh")
	refineryPath := filepath.Join(root, "lifecycle/packs/lifecycle/assets/scripts/mock-refinery.sh")

	polecatTarget := runShell(t, []string{"GC_TEMPLATE=demo/lifecycle.polecat"}, shellFunction(t, polecatPath, "derive_refinery_target")+`
derive_refinery_target
`)
	refineryAssignment := shellLineContaining(t, refineryPath, "MERGE_ASSIGNEE=")
	refineryAssignee := runShell(t, []string{
		"GC_ALIAS=" + polecatTarget,
		"GC_AGENT=demo--lifecycle__refinery",
	}, refineryAssignment+`
printf '%s' "$MERGE_ASSIGNEE"
`)

	if refineryAssignee != polecatTarget {
		t.Fatalf("refinery consumes assignee %q, want polecat handoff target %q", refineryAssignee, polecatTarget)
	}
	if refineryAssignee == "demo--lifecycle__refinery" {
		t.Fatalf("refinery still consumes sanitized GC_AGENT instead of canonical GC_ALIAS")
	}
}

func examplesRoot(t *testing.T) string {
	t.Helper()
	_, filename, _, _ := runtime.Caller(0)
	return filepath.Dir(filename)
}

func shellLineContaining(t *testing.T, path, needle string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}
	for _, line := range strings.Split(string(data), "\n") {
		if strings.Contains(line, needle) {
			return line
		}
	}
	t.Fatalf("%s missing shell line containing %q", path, needle)
	return ""
}

func shellFunction(t *testing.T, path, name string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}
	lines := strings.Split(string(data), "\n")
	for i, line := range lines {
		if line == name+"() {" {
			for j := i + 1; j < len(lines); j++ {
				if lines[j] == "}" {
					return strings.Join(lines[i:j+1], "\n")
				}
			}
			t.Fatalf("%s shell function %q has no closing brace", path, name)
		}
	}
	t.Fatalf("%s missing shell function %q", path, name)
	return ""
}

func runShell(t *testing.T, env []string, body string) string {
	t.Helper()
	cmd := shellCommand(t, env, body)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("shell command failed: %v\n%s", err, out)
	}
	return strings.TrimSpace(string(out))
}

func shellCommand(t *testing.T, env []string, body string) *exec.Cmd {
	t.Helper()
	cmd := exec.Command("bash", "-e", "-u", "-o", "pipefail", "-c", body)
	cmd.Env = append(scrubEnv(os.Environ(), "GC_TEMPLATE", "GC_ALIAS", "GC_AGENT"), env...)
	return cmd
}

func scrubEnv(env []string, names ...string) []string {
	blocked := make(map[string]struct{}, len(names))
	for _, name := range names {
		blocked[name] = struct{}{}
	}
	kept := env[:0]
	for _, entry := range env {
		name, _, _ := strings.Cut(entry, "=")
		if _, ok := blocked[name]; !ok {
			kept = append(kept, entry)
		}
	}
	return kept
}
