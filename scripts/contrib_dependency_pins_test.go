package scripts_test

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// fromLineRe captures the image reference and optional stage alias of a
// Dockerfile FROM instruction (flags such as --platform are skipped).
var fromLineRe = regexp.MustCompile(`(?i)^\s*FROM\s+(?:--\S+\s+)*(\S+)(?:\s+AS\s+(\S+))?`)

// TestContribDockerfileBaseImagesArePinnedByDigest guards Scorecard's
// Pinned-Dependencies check (alert 418): every registry base image in a
// contrib Dockerfile is pinned by digest. A FROM naming an earlier build
// stage or a build-arg (${BASE}, a locally built image) is not a registry
// pull and is exempt.
func TestContribDockerfileBaseImagesArePinnedByDigest(t *testing.T) {
	root := repoRoot(t)
	var dockerfiles []string
	err := filepath.WalkDir(filepath.Join(root, "contrib"), func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() && d.Name() == "node_modules" {
			return filepath.SkipDir
		}
		if !d.IsDir() && strings.HasPrefix(d.Name(), "Dockerfile") {
			dockerfiles = append(dockerfiles, path)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk contrib: %v", err)
	}
	if len(dockerfiles) == 0 {
		t.Fatal("found no Dockerfile under contrib; the walk is broken")
	}
	for _, path := range dockerfiles {
		rel, _ := filepath.Rel(root, path)
		stages := map[string]bool{}
		for i, line := range strings.Split(readFile(t, root, rel), "\n") {
			m := fromLineRe.FindStringSubmatch(line)
			if m == nil {
				continue
			}
			image, alias := m[1], m[2]
			exempt := stages[strings.ToLower(image)] || strings.HasPrefix(image, "$")
			if !exempt && !strings.Contains(image, "@sha256:") {
				t.Errorf("%s:%d: base image %q must be pinned by digest (image:tag@sha256:...)", rel, i+1, image)
			}
			if alias != "" {
				stages[strings.ToLower(alias)] = true
			}
		}
	}
}

// TestContribNPMProjectInstallsUseLockfile guards Scorecard's
// Pinned-Dependencies check (alerts 420, 184, 181): a contrib project that
// commits a package-lock.json installs with `npm ci`, which installs exactly
// the locked, integrity-checked tree, never `npm install`, which may
// re-resolve it.
func TestContribNPMProjectInstallsUseLockfile(t *testing.T) {
	root := repoRoot(t)
	for _, tc := range []struct {
		file     string
		lockfile string
	}{
		{file: "contrib/mayor-chat/Dockerfile", lockfile: "contrib/mayor-chat/package-lock.json"},
		{file: "contrib/openclaw-bridge/demo.sh", lockfile: "contrib/openclaw-bridge/package-lock.json"},
		{file: "contrib/openclaw-bridge/demo-telegram.sh", lockfile: "contrib/openclaw-bridge/package-lock.json"},
	} {
		if _, err := os.Stat(filepath.Join(root, tc.lockfile)); err != nil {
			t.Fatalf("%s: lockfile %s: %v", tc.file, tc.lockfile, err)
		}
		content := readFile(t, root, tc.file)
		if !strings.Contains(content, "npm ci ") {
			t.Errorf("%s must install its locked dependencies with `npm ci`", tc.file)
		}
		for i, line := range strings.Split(content, "\n") {
			trimmed := strings.TrimSpace(line)
			if strings.HasPrefix(trimmed, "#") {
				continue
			}
			// `npm install -g <pkg>@<version>` installs a global tool, not
			// the project's locked tree, and is out of scope here.
			if strings.Contains(line, "npm install") && !strings.Contains(line, "npm install -g ") {
				t.Errorf("%s:%d: project dependencies must use `npm ci`, not `npm install`: %s", tc.file, i+1, trimmed)
			}
		}
	}
}
