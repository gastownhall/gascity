package runtime

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

type overlayTrackedStatus int

const (
	overlayTrackedUnknown overlayTrackedStatus = iota
	overlayTrackedNo
	overlayTrackedYes
)

var (
	probeOverlayTrackedStatuses = gitOverlayTrackedStatuses
	probeOverlayTrackedStatus   = gitOverlayTrackedStatus
	trackedOverlayWarningsSeen  sync.Map
)

func trackedOverlaySkip(dstDir string, relPaths []string, warnings io.Writer) func(string, bool) bool {
	statuses, enabled := probeOverlayTrackedStatuses(dstDir, relPaths)
	if !enabled {
		return nil
	}
	return func(relPath string, isDir bool) bool {
		if isDir {
			return false
		}
		clean := cleanOverlayRelPath(relPath)
		status, ok := statuses[clean]
		if !ok {
			status = overlayTrackedUnknown
		}
		if status == overlayTrackedNo {
			return false
		}
		warnTrackedOverlaySkip(dstDir, clean, status, warnings)
		return true
	}
}

func gitOverlayTrackedStatus(dstDir, relPath string) overlayTrackedStatus {
	statuses, enabled := gitOverlayTrackedStatuses(dstDir, []string{relPath})
	if !enabled {
		return overlayTrackedNo
	}
	status, ok := statuses[cleanOverlayRelPath(relPath)]
	if !ok {
		return overlayTrackedUnknown
	}
	return status
}

func gitOverlayTrackedStatuses(dstDir string, relPaths []string) (map[string]overlayTrackedStatus, bool) {
	relPaths = uniqueCleanOverlayRelPaths(relPaths)
	statuses := make(map[string]overlayTrackedStatus, len(relPaths))
	for _, rel := range relPaths {
		statuses[rel] = overlayTrackedUnknown
	}
	if len(relPaths) == 0 {
		return statuses, true
	}

	prefix, ok, unknown := gitWorktreePrefix(dstDir)
	switch {
	case unknown:
		return statuses, true
	case !ok:
		return nil, false
	}

	rootRelByClean := make(map[string]string, len(relPaths))
	pathspecs := make([]string, 0, len(relPaths))
	for _, rel := range relPaths {
		rootRel := cleanGitRelPath(path.Join(prefix, filepath.ToSlash(rel)))
		rootRelByClean[rel] = rootRel
		pathspecs = append(pathspecs, ":(top,literal,icase)"+rootRel)
		statuses[rel] = overlayTrackedNo
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	args := append([]string{"-C", dstDir, "ls-files", "-z", "--"}, pathspecs...)
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Env = trackedOverlayGitEnv()
	output, err := cmd.Output()
	if ctx.Err() != nil || err != nil {
		for _, rel := range relPaths {
			statuses[rel] = overlayTrackedUnknown
		}
		return statuses, true
	}
	for _, tracked := range strings.Split(string(output), "\x00") {
		tracked = cleanGitRelPath(tracked)
		if tracked == "" {
			continue
		}
		for rel, rootRel := range rootRelByClean {
			if strings.EqualFold(tracked, rootRel) {
				statuses[rel] = overlayTrackedYes
			}
		}
	}
	return statuses, true
}

func gitWorktreePrefix(dstDir string) (string, bool, bool) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", "-C", dstDir, "rev-parse", "--show-toplevel", "--show-prefix")
	cmd.Env = trackedOverlayGitEnv()
	output, err := cmd.CombinedOutput()
	if ctx.Err() != nil {
		return "", false, true
	}
	if err != nil {
		text := strings.ToLower(string(output))
		if strings.Contains(text, "not a git repository") || strings.Contains(text, "not a git work tree") {
			return "", false, false
		}
		return "", false, true
	}
	lines := strings.Split(strings.ReplaceAll(string(output), "\r\n", "\n"), "\n")
	if len(lines) < 2 {
		return "", false, true
	}
	return cleanGitRelPath(lines[1]), true, false
}

func uniqueCleanOverlayRelPaths(relPaths []string) []string {
	seen := make(map[string]bool, len(relPaths))
	out := make([]string, 0, len(relPaths))
	for _, rel := range relPaths {
		clean := cleanOverlayRelPath(rel)
		if clean == "." || clean == "" || seen[clean] {
			continue
		}
		seen[clean] = true
		out = append(out, clean)
	}
	return out
}

func cleanOverlayRelPath(relPath string) string {
	return filepath.Clean(relPath)
}

func cleanGitRelPath(relPath string) string {
	relPath = strings.TrimSpace(filepath.ToSlash(relPath))
	if relPath == "" {
		return ""
	}
	clean := path.Clean(relPath)
	if clean == "." {
		return ""
	}
	return clean
}

func warnTrackedOverlaySkip(dstDir, relPath string, status overlayTrackedStatus, warnings io.Writer) {
	if warnings == nil {
		return
	}
	abs := filepath.Clean(filepath.Join(dstDir, relPath))
	key := strings.ToLower(abs)
	if _, loaded := trackedOverlayWarningsSeen.LoadOrStore(key, struct{}{}); loaded {
		return
	}
	reason := "it is tracked by the destination repository"
	if status == overlayTrackedUnknown {
		reason = "destination repository trackedness could not be determined"
	}
	fmt.Fprintf(warnings, "overlay: skipping provider overlay %q: %s\n", relPath, reason) //nolint:errcheck
}

func trackedOverlayGitEnv() []string {
	out := make([]string, 0, len(os.Environ()))
	for _, entry := range os.Environ() {
		key, _, _ := strings.Cut(entry, "=")
		switch key {
		case "GIT_DIR", "GIT_WORK_TREE", "GIT_INDEX_FILE", "GIT_OBJECT_DIRECTORY", "GIT_COMMON_DIR":
			continue
		default:
			out = append(out, entry)
		}
	}
	return out
}
