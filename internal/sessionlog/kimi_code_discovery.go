package sessionlog

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

var kimiCodeSlugInvalid = regexp.MustCompile(`[^a-z0-9._-]+`)

// kimiCodeWorkDirKey follows Kimi Code's encodeWorkDirKey, including slug
// truncation and the SHA-256 suffix. Kimi Code resolves the workspace root
// before persisting it; the legacy CLI's MD5 key remains lexical.
func kimiCodeWorkDirKey(workDir string) string {
	if resolved, err := filepath.EvalSymlinks(workDir); err == nil {
		workDir = resolved
	}
	normalized := strings.TrimRight(strings.ReplaceAll(filepath.Clean(workDir), `\`, "/"), "/")
	parts := strings.Split(normalized, "/")
	slug := strings.Trim(kimiCodeSlugInvalid.ReplaceAllString(strings.ToLower(parts[len(parts)-1]), "-"), "-")
	if len(slug) > 40 {
		slug = slug[:40]
	}
	slug = strings.Trim(slug, "-")
	if slug == "" || slug == "." || slug == ".." {
		slug = "workspace"
	}
	sum := sha256.Sum256([]byte(normalized))
	return "wd_" + slug + "_" + hex.EncodeToString(sum[:6])
}

func kimiTranscriptPath(workRoot, sessionID string) string {
	if strings.HasPrefix(filepath.Base(workRoot), "wd_") {
		return filepath.Join(workRoot, sessionID, "agents", "main", "wire.jsonl")
	}
	return filepath.Join(workRoot, sessionID, "context.jsonl")
}

func kimiSessionCandidates(searchPaths []string, workDir string) []kimiContextCandidate {
	var candidates []kimiContextCandidate
	for _, root := range mergeKimiSearchPaths(searchPaths) {
		for _, key := range []string{kimiWorkDirHash(workDir), kimiCodeWorkDirKey(workDir)} {
			candidates = append(candidates, findKimiSessionFilesIn(root, key)...)
		}
	}
	return candidates
}

// A missing bucket in one account is normal when another account or CLI
// layout owns the session. Diagnose the workdir only after discovery fails.
func logKimiMissingWorkDir(searchPaths []string, workDir string) {
	legacyKey, codeKey := kimiWorkDirHash(workDir), kimiCodeWorkDirKey(workDir)
	for _, root := range mergeKimiSearchPaths(searchPaths) {
		if kimiDirectoryExists(filepath.Join(root, legacyKey)) || kimiDirectoryExists(filepath.Join(root, codeKey)) {
			return
		}
	}
	// Report the key belonging to the layout the populated root actually uses:
	// naming the other CLI's key sends triage after a bucket that CLI never
	// mints, which is the opposite of the message's own hashing advice.
	for _, root := range mergeKimiSearchPaths(searchPaths) {
		entries, err := os.ReadDir(root)
		if err != nil {
			continue
		}
		for _, key := range []string{legacyKey, codeKey} {
			if hasKimiSessionRootEntries(entries, key) {
				logKimiMissingWorkHash(root, key)
				return
			}
		}
	}
}
