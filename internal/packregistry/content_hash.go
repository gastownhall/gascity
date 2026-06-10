package packregistry

import (
	"bytes"
	"crypto/sha256"
	"fmt"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"sort"
	"strings"

	"github.com/BurntSushi/toml"
)

// PackContentHash returns the canonical sha256 content hash for one pack
// directory at a git commit. The hash is over a sorted manifest of tracked
// files containing relative path, executable mode, and file-content hash.
func PackContentHash(repoDir, commit, packPath string) (string, error) {
	repoDir = strings.TrimSpace(repoDir)
	if repoDir == "" {
		return "", fmt.Errorf("repository path is required")
	}
	commit = strings.TrimSpace(commit)
	if commit == "" {
		return "", fmt.Errorf("commit is required")
	}
	packPath, err := cleanPackContentPath(packPath)
	if err != nil {
		return "", err
	}
	resolvedCommit, err := ResolveGitCommit(repoDir, commit)
	if err != nil {
		return "", err
	}
	if err := requirePackToml(repoDir, resolvedCommit, packPath); err != nil {
		return "", err
	}

	args := []string{"ls-tree", "-r", "-z", "--full-tree", resolvedCommit}
	if packPath != "" {
		args = append(args, "--", packPath)
	}
	out, err := runRegistryGitBytes(repoDir, args...)
	if err != nil {
		return "", fmt.Errorf("listing pack tree: %w", err)
	}
	records := bytes.Split(bytes.TrimSuffix(out, []byte{0}), []byte{0})
	entries := make([]string, 0, len(records))
	for _, record := range records {
		if len(record) == 0 {
			continue
		}
		fields, filePath, ok := bytes.Cut(record, []byte{'\t'})
		if !ok {
			return "", fmt.Errorf("unexpected git ls-tree record %q", string(record))
		}
		parts := strings.Fields(string(fields))
		if len(parts) != 3 {
			return "", fmt.Errorf("unexpected git ls-tree metadata %q", string(fields))
		}
		mode, objectType, objectID := parts[0], parts[1], parts[2]
		if objectType != "blob" {
			return "", fmt.Errorf("unsupported git object type %q for %s", objectType, filePath)
		}
		rel, err := relativePackContentPath(string(filePath), packPath)
		if err != nil {
			return "", err
		}
		perm, err := gitManifestPerm(mode)
		if err != nil {
			return "", fmt.Errorf("%s: %w", rel, err)
		}
		data, err := runRegistryGitBytes(repoDir, "cat-file", "blob", objectID)
		if err != nil {
			return "", fmt.Errorf("reading git blob %s for %s: %w", objectID, rel, err)
		}
		sum := sha256.Sum256(data)
		entries = append(entries, fmt.Sprintf("%s %s %x", rel, perm, sum[:]))
	}
	if len(entries) == 0 {
		return "", fmt.Errorf("pack path %q has no tracked files at %s", displayPackPath(packPath), resolvedCommit)
	}
	sort.Strings(entries)
	sum := sha256.Sum256([]byte(strings.Join(entries, "\n")))
	return fmt.Sprintf("sha256:%x", sum[:]), nil
}

// VerifyPackContentHash checks one pack-content hash against the canonical
// hash for repoDir/commit/packPath.
func VerifyPackContentHash(repoDir, commit, packPath, expected string) error {
	expected = strings.TrimSpace(expected)
	if err := ValidateReleaseHash(expected); err != nil {
		return err
	}
	actual, err := PackContentHash(repoDir, commit, packPath)
	if err != nil {
		return err
	}
	if actual != expected {
		return fmt.Errorf("hash mismatch: got %s, want %s", actual, expected)
	}
	return nil
}

// PackNameAtCommit reads [pack].name from pack.toml at repoDir/commit/packPath.
func PackNameAtCommit(repoDir, commit, packPath string) (string, error) {
	packPath, err := cleanPackContentPath(packPath)
	if err != nil {
		return "", err
	}
	resolvedCommit, err := ResolveGitCommit(repoDir, commit)
	if err != nil {
		return "", err
	}
	packToml := path.Join(packPath, "pack.toml")
	if packPath == "" {
		packToml = "pack.toml"
	}
	data, err := runRegistryGitBytes(repoDir, "cat-file", "blob", resolvedCommit+":"+packToml)
	if err != nil {
		return "", fmt.Errorf("reading %s at %s: %w", packToml, resolvedCommit, err)
	}
	var meta struct {
		Pack struct {
			Name string `toml:"name"`
		} `toml:"pack"`
	}
	if _, err := toml.Decode(string(data), &meta); err != nil {
		return "", fmt.Errorf("parsing %s at %s: %w", packToml, resolvedCommit, err)
	}
	if strings.TrimSpace(meta.Pack.Name) == "" {
		return "", fmt.Errorf("%s at %s is missing [pack].name", packToml, resolvedCommit)
	}
	return meta.Pack.Name, nil
}

// ResolveGitCommit resolves commit-ish to a full lowercase git commit SHA.
func ResolveGitCommit(repoDir, commitish string) (string, error) {
	out, err := runRegistryGitBytes(repoDir, "rev-parse", "--verify", strings.TrimSpace(commitish)+"^{commit}")
	if err != nil {
		return "", fmt.Errorf("resolving commit %q: %w", commitish, err)
	}
	commit := strings.ToLower(strings.TrimSpace(string(out)))
	if !commitRE.MatchString(commit) {
		return "", fmt.Errorf("resolved commit %q is not a full SHA", commit)
	}
	return commit, nil
}

// ValidateReleaseHash validates the registry release hash syntax.
func ValidateReleaseHash(hash string) error {
	if !hashRE.MatchString(strings.TrimSpace(hash)) {
		return fmt.Errorf("hash must be sha256:<64 lowercase hex>")
	}
	return nil
}

// ValidateReleaseCommit validates the registry release commit syntax.
func ValidateReleaseCommit(commit string) error {
	if !commitRE.MatchString(strings.TrimSpace(commit)) {
		return fmt.Errorf("commit must be a full lowercase SHA")
	}
	return nil
}

func requirePackToml(repoDir, commit, packPath string) error {
	packToml := path.Join(packPath, "pack.toml")
	if packPath == "" {
		packToml = "pack.toml"
	}
	if _, err := runRegistryGitBytes(repoDir, "cat-file", "-e", commit+":"+packToml); err != nil {
		return fmt.Errorf("pack path %q does not contain tracked pack.toml at %s", displayPackPath(packPath), commit)
	}
	return nil
}

func cleanPackContentPath(raw string) (string, error) {
	raw = strings.TrimSpace(filepath.ToSlash(raw))
	if raw == "" || raw == "." {
		return "", nil
	}
	if strings.HasPrefix(raw, "/") {
		return "", fmt.Errorf("pack path %q must be relative", raw)
	}
	clean := path.Clean(strings.Trim(raw, "/"))
	if clean == "." {
		return "", nil
	}
	if clean == ".." || strings.HasPrefix(clean, "../") || strings.Contains(clean, "/../") {
		return "", fmt.Errorf("pack path %q must stay within the repository", raw)
	}
	return clean, nil
}

func relativePackContentPath(gitPath, packPath string) (string, error) {
	gitPath = path.Clean(filepath.ToSlash(gitPath))
	if packPath == "" {
		return gitPath, nil
	}
	prefix := packPath + "/"
	if !strings.HasPrefix(gitPath, prefix) {
		return "", fmt.Errorf("git path %q is outside pack path %q", gitPath, packPath)
	}
	rel := strings.TrimPrefix(gitPath, prefix)
	if rel == "" || rel == "." {
		return "", fmt.Errorf("empty relative path for %q", gitPath)
	}
	return rel, nil
}

func gitManifestPerm(mode string) (string, error) {
	switch mode {
	case "100644":
		return "0644", nil
	case "100755":
		return "0755", nil
	case "120000":
		return "0777", nil
	default:
		return "", fmt.Errorf("unsupported git file mode %q", mode)
	}
}

func displayPackPath(packPath string) string {
	if packPath == "" {
		return "."
	}
	return packPath
}

func runRegistryGitBytes(dir string, args ...string) ([]byte, error) {
	cmdArgs := append([]string{
		"-c", "core.fsmonitor=false",
		"-c", "core.hooksPath=/dev/null",
		"-c", "core.untrackedCache=false",
	}, args...)
	cmd := exec.Command("git", cmdArgs...)
	cmd.Dir = dir
	for _, env := range os.Environ() {
		key, _, ok := strings.Cut(env, "=")
		if ok && registryGitEnvBlacklist[key] {
			continue
		}
		cmd.Env = append(cmd.Env, env)
	}
	cmd.Env = append(cmd.Env, "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null")
	out, err := cmd.CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("git %s: %s: %w", strings.Join(args, " "), strings.TrimSpace(string(out)), err)
	}
	return out, nil
}

var registryGitEnvBlacklist = map[string]bool{
	"GIT_DIR":                          true,
	"GIT_WORK_TREE":                    true,
	"GIT_INDEX_FILE":                   true,
	"GIT_OBJECT_DIRECTORY":             true,
	"GIT_ALTERNATE_OBJECT_DIRECTORIES": true,
	"GIT_COMMON_DIR":                   true,
	"GIT_CEILING_DIRECTORIES":          true,
	"GIT_DISCOVERY_ACROSS_FILESYSTEM":  true,
	"GIT_NAMESPACE":                    true,
	"GIT_CONFIG":                       true,
	"GIT_CONFIG_GLOBAL":                true,
	"GIT_CONFIG_SYSTEM":                true,
	"GIT_CONFIG_NOSYSTEM":              true,
	"GIT_CONFIG_COUNT":                 true,
	"GIT_EXEC_PATH":                    true,
	"GIT_PAGER":                        true,
}
