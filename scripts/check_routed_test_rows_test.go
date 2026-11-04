package scripts_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestRoutedRowsGuard(t *testing.T) {
	const allRows = `name: "api-happy-path", name: "api-cache-not-live", name: "api-500-fallback", name: "api-404-error", name: "controller-down", name: "escape-hatch"`
	for _, tc := range []struct {
		name, listed, extra, manifest, want string
	}{
		{"routing reasons are not matrix rows", allRows, `route(nil, "escape-hatch"); reason: "controller-down"`, "cmd/gc/cmd_listed_test.go\n", ""},
		{"partial matrix discovered", allRows, `name: "controller-down"`, "cmd/gc/cmd_listed_test.go\n", "INCOMPLETE: cmd/gc/cmd_extra_test.go has 1/6"},
		{"complete matrix discovered", allRows, allRows, "cmd/gc/cmd_listed_test.go\n", "ADD TO MANIFEST: cmd/gc/cmd_extra_test.go"},
		{"listed row removed", strings.Replace(allRows, `name: "escape-hatch"`, `reason: "escape-hatch"`, 1), "", "cmd/gc/cmd_listed_test.go\n", "MANIFEST FILE UNDER-COVERED: cmd/gc/cmd_listed_test.go has 5/6"},
		{"listed file missing", allRows, "", "cmd/gc/cmd_missing_test.go\n", "MANIFEST FILE MISSING: cmd/gc/cmd_missing_test.go"},
		{"empty manifest", allRows, "", "# empty\n", "lists no files"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			for _, dir := range []string{"scripts", "cmd/gc"} {
				if err := os.MkdirAll(filepath.Join(root, dir), 0o755); err != nil {
					t.Fatal(err)
				}
			}
			script, err := os.ReadFile(filepath.Join(repoRoot(t), "scripts/check-routed-test-rows.sh"))
			if err != nil {
				t.Fatal(err)
			}
			for path, content := range map[string]string{
				"scripts/check-routed-test-rows.sh": string(script),
				"scripts/routed-test-rows.manifest": tc.manifest,
				"cmd/gc/cmd_listed_test.go":         tc.listed,
				"cmd/gc/cmd_extra_test.go":          tc.extra,
			} {
				if err := os.WriteFile(filepath.Join(root, path), []byte(content), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			out, err := exec.Command("bash", filepath.Join(root, "scripts/check-routed-test-rows.sh")).CombinedOutput()
			if tc.want == "" {
				if err != nil {
					t.Fatalf("guard unexpectedly failed: %v\n%s", err, out)
				}
			} else if err == nil || !strings.Contains(string(out), tc.want) {
				t.Fatalf("guard output = %q, error = %v; want failure containing %q", out, err, tc.want)
			}
		})
	}
}
