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

// TestContribDockerfileBaseImagesArePinnedByDigest keeps the alert 418 fix
// and the other contrib base-image digests in place: every FROM in a
// contrib file whose name starts with "Dockerfile" must contain "@sha256:".
// A FROM naming an earlier build stage is exempt, and so is any image that
// starts with "$", such as ${BASE}; the ARG default is not resolved, so a
// build-arg that defaults to a registry image also passes unpinned.
//
// The test pins the current sites; it does not reimplement Scorecard's
// Pinned-Dependencies rule, so passing it does not rule out a new alert.
// Scorecard resolves ARG defaults (it flags the two k8s build-arg images
// exempted here, alerts 27 and 63), checks files whose name contains
// "dockerfile" in any case, accepts only a full 64-hex or ${ARG} digest,
// and exempts FROM scratch, which this test rejects.
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

// TestContribNPMProjectInstallsUseLockfile keeps the npm ci fixes for
// alerts 420, 184 and 181 in place. For each listed file it requires the
// project's package-lock.json to exist, the text "npm ci " to appear in the
// file, and no line other than a #-comment line to contain `npm install`
// unless the same line contains `npm install -g ` (a global tool install).
// `npm ci` installs exactly the locked, integrity-checked tree; `npm
// install` may re-resolve it.
//
// The file list is fixed and the match is textual, so the test pins these
// three sites rather than enforcing Scorecard's Pinned-Dependencies rule
// across contrib. It does not check a new install site or an `npm i`
// spelling, and Scorecard flags the global install this test exempts
// (alert 419).
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
