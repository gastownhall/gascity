package k8s

import (
	"archive/tar"
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/citylayout"
	"github.com/gastownhall/gascity/internal/overlay"
	"github.com/gastownhall/gascity/internal/pathutil"
	"github.com/gastownhall/gascity/internal/runtime"
)

// cityRootRuntimeInputStagePaths are source/config surfaces the K8s provider
// must preserve when it presents /workspace as the pod-side city root. Mutable
// runtime state stays out; nested workdirs are staged separately.
var cityRootRuntimeInputStagePaths = []string{
	citylayout.CityConfigFile,
	"pack.toml",
	"AGENTS.md",
	"CLAUDE.md",
	"GEMINI.md",
	citylayout.PromptsRoot,
	citylayout.FormulasRoot,
	citylayout.OrdersRoot,
	citylayout.HooksRoot,
	citylayout.ScriptsRoot,
	"assets",
	"commands",
	"doctor",
	"mcp",
	"overlay",
	"overlays",
	"packs",
	"skills",
	"template-fragments",
	".gc/settings.json",
	citylayout.SystemRoot,
	citylayout.CachePacksRoot,
	citylayout.CacheIncludesRoot,
}

// stageFiles copies city root runtime inputs, overlay, copy_files, and workdir
// content into the pod via the init container, then signals it to exit.
func stageFiles(ctx context.Context, ops k8sOps, podName string, cfg runtime.Config, ctrlCity string, warn io.Writer) error {
	// Wait for init container to be running (up to 60s).
	if err := waitForInitContainer(ctx, ops, podName, "stage", 60*time.Second); err != nil {
		return err
	}

	podWorkDir := projectedPodWorkDirForControllerPath(cfg.WorkDir, ctrlCity)
	if needsCityRootRuntimeInputStaging(cfg.WorkDir, ctrlCity) {
		if err := stageCityRootRuntimeInputsToPod(ctx, ops, podName, ctrlCity); err != nil {
			return err
		}
		if err := stageControllerSourceRootsToPod(ctx, ops, podName, cfg, ctrlCity); err != nil {
			return err
		}
	}
	// Copy the session work_dir into the pod after city inputs so the active
	// workdir wins where paths overlap.
	if cfg.WorkDir != "" && cfg.WorkDir != ctrlCity {
		if err := copyDirToPod(ctx, ops, podName, "stage", cfg.WorkDir, podWorkDir); err != nil {
			fmt.Fprintf(warn, "gc: warning: staging workdir %s to %s: %v\n", cfg.WorkDir, podWorkDir, err) //nolint:errcheck
		}
	}

	if err := stageProviderOverlaysToPod(ctx, ops, podName, cfg, podWorkDir, warn); err != nil {
		return err
	}

	// Copy each copy_files entry.
	for _, entry := range cfg.CopyFiles {
		dst := "/workspace"
		if entry.RelDst != "" {
			dst = "/workspace/" + entry.RelDst
		}
		if err := copyToPod(ctx, ops, podName, "stage", entry.Src, dst); err != nil {
			fmt.Fprintf(warn, "gc: warning: staging copy_file %s → %s: %v\n", entry.Src, dst, err) //nolint:errcheck
		}
	}

	// Mirror .gc/ into city volume when GC_CITY differs from work_dir.
	if ctrlCity != "" && ctrlCity != cfg.WorkDir {
		_, _ = ops.execInPod(ctx, podName, "stage",
			[]string{"sh", "-c", "cp -a /workspace/.gc /city-stage/.gc 2>/dev/null || true"}, nil)
	}

	// Signal init container to exit.
	_, err := ops.execInPod(ctx, podName, "stage",
		[]string{"touch", "/workspace/.gc-ready"}, nil)
	return err
}

// stageLaunchFiles copies bounded launch material into the pod before the
// agent container starts. Large prompts, command strings, and pre_start bodies
// live in this EmptyDir-backed filesystem instead of Kubernetes argv/env.
func stageLaunchFiles(ctx context.Context, ops k8sOps, podName string, cfg runtime.Config, p *Provider) error {
	if err := waitForInitContainer(ctx, ops, podName, "launch", 60*time.Second); err != nil {
		return err
	}
	stageDir, err := os.MkdirTemp("", "gc-k8s-launch-")
	if err != nil {
		return fmt.Errorf("preparing launch material: %w", err)
	}
	defer os.RemoveAll(stageDir) //nolint:errcheck

	material := buildPodLaunchMaterial(cfg, p)
	if err := writePodLaunchMaterial(stageDir, material); err != nil {
		return err
	}
	if err := copyDirToPod(ctx, ops, podName, "launch", stageDir, podLaunchDir); err != nil {
		return fmt.Errorf("staging launch material: %w", err)
	}
	_, err = ops.execInPod(ctx, podName, "launch", []string{"touch", podLaunchReadyMarker}, nil)
	return err
}

func writePodLaunchMaterial(root string, material podLaunchMaterial) error {
	if err := os.MkdirAll(filepath.Join(root, "pre-start"), 0o755); err != nil {
		return fmt.Errorf("creating launch pre-start dir: %w", err)
	}
	if err := os.WriteFile(filepath.Join(root, "entrypoint.sh"), []byte(material.Entrypoint), 0o755); err != nil {
		return fmt.Errorf("writing launch entrypoint: %w", err)
	}
	if err := os.WriteFile(filepath.Join(root, "agent-launch.sh"), []byte(material.Agent), 0o755); err != nil {
		return fmt.Errorf("writing agent launch script: %w", err)
	}
	if material.HasPrompt {
		if err := os.WriteFile(filepath.Join(root, "prompt.txt"), []byte(material.Prompt), 0o600); err != nil {
			return fmt.Errorf("writing launch prompt: %w", err)
		}
	}
	for i, cmd := range material.PreStart {
		name := fmt.Sprintf("%03d.sh", i)
		if err := os.WriteFile(filepath.Join(root, "pre-start", name), []byte("#!/bin/sh\n"+cmd+"\n"), 0o755); err != nil {
			return fmt.Errorf("writing pre_start[%d]: %w", i, err)
		}
	}
	return nil
}

func projectedPodWorkDirForControllerPath(workDir, ctrlCity string) string {
	return projectedPodWorkDirForControllerPathRoot(workDir, ctrlCity, defaultPodWorkspaceRoot)
}

func projectedPodWorkDirForControllerPathRoot(workDir, ctrlCity, podCityRoot string) string {
	if podPath, ok := projectedPodPathForControllerPathRoot(ctrlCity, workDir, podCityRoot); ok {
		return podPath
	}
	podCityRoot = strings.TrimRight(strings.TrimSpace(podCityRoot), "/")
	if podCityRoot == "" {
		return defaultPodWorkspaceRoot
	}
	return podCityRoot
}

func projectedPodPathForControllerPath(ctrlCity, controllerPath string) (string, bool) {
	return projectedPodPathForControllerPathRoot(ctrlCity, controllerPath, defaultPodWorkspaceRoot)
}

func projectedPodPathForControllerPathRoot(ctrlCity, controllerPath, podCityRoot string) (string, bool) {
	ctrlCity = strings.TrimSpace(ctrlCity)
	controllerPath = strings.TrimSpace(controllerPath)
	podCityRoot = strings.TrimRight(strings.TrimSpace(podCityRoot), "/")
	if podCityRoot == "" {
		podCityRoot = defaultPodWorkspaceRoot
	}
	if ctrlCity == "" || controllerPath == "" {
		return "", false
	}
	rel, err := filepath.Rel(filepath.Clean(ctrlCity), filepath.Clean(controllerPath))
	if err != nil || pathutil.IsOutsideDir(rel) {
		return "", false
	}
	if rel == "." {
		return podCityRoot, true
	}
	return path.Join(podCityRoot, filepath.ToSlash(rel)), true
}

func needsCityRootRuntimeInputStaging(workDir, ctrlCity string) bool {
	ctrlCity = strings.TrimSpace(ctrlCity)
	if ctrlCity == "" {
		return false
	}
	workDir = strings.TrimSpace(workDir)
	if workDir == "" {
		return true
	}
	_, ok := projectedPodPathForControllerPath(ctrlCity, workDir)
	return ok
}

func stageCityRootRuntimeInputsToPod(ctx context.Context, ops k8sOps, podName string, ctrlCity string) error {
	stageDir, err := os.MkdirTemp("", "gc-k8s-city-root-")
	if err != nil {
		return fmt.Errorf("preparing city root runtime inputs: %w", err)
	}
	defer os.RemoveAll(stageDir) //nolint:errcheck

	staged := false
	for _, rel := range cityRootRuntimeInputStagePaths {
		src := filepath.Join(ctrlCity, filepath.FromSlash(rel))
		if _, err := os.Stat(src); os.IsNotExist(err) {
			continue
		} else if err != nil {
			return fmt.Errorf("checking city root runtime input %s: %w", rel, err)
		}
		dst := filepath.Join(stageDir, filepath.FromSlash(rel))
		if err := stageCityRootRuntimeInputPath(src, dst); err != nil {
			return fmt.Errorf("staging city root runtime input %s: %w", rel, err)
		}
		staged = true
	}
	if !staged {
		return nil
	}
	if err := copyDirToPod(ctx, ops, podName, "stage", stageDir, "/workspace"); err != nil {
		return fmt.Errorf("staging city root runtime inputs: %w", err)
	}
	return nil
}

func stageCityRootRuntimeInputPath(src, dst string) error {
	info, err := os.Stat(src)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return runtime.StagePath(src, dst)
	}
	return overlay.CopyDirWithSkip(src, dst, skipCityRootRuntimeInputPath, io.Discard)
}

type controllerSourceRootStagePath struct {
	controllerPath string
	podPath        string
}

func controllerSourceRootStagePaths(cfg runtime.Config, ctrlCity string) []controllerSourceRootStagePath {
	ctrlCity = strings.TrimSpace(ctrlCity)
	if ctrlCity == "" {
		return nil
	}

	seen := map[string]bool{}
	var paths []controllerSourceRootStagePath
	for _, key := range []string{"GC_RIG_ROOT", "GC_STORE_ROOT", "GT_ROOT"} {
		controllerPath := strings.TrimSpace(cfg.Env[key])
		if controllerPath == "" {
			continue
		}
		cleanControllerPath := filepath.Clean(controllerPath)
		if seen[cleanControllerPath] {
			continue
		}
		podPath, ok := projectedPodPathForControllerPath(ctrlCity, cleanControllerPath)
		if !ok || podPath == "/workspace" {
			continue
		}
		rel := strings.TrimPrefix(podPath, "/workspace/")
		if rel == "" || rel == ".gc" || strings.HasPrefix(rel, ".gc/") {
			continue
		}
		seen[cleanControllerPath] = true
		paths = append(paths, controllerSourceRootStagePath{
			controllerPath: cleanControllerPath,
			podPath:        podPath,
		})
	}
	return paths
}

func stageControllerSourceRootsToPod(ctx context.Context, ops k8sOps, podName string, cfg runtime.Config, ctrlCity string) error {
	for _, stagePath := range controllerSourceRootStagePaths(cfg, ctrlCity) {
		info, err := os.Stat(stagePath.controllerPath)
		if err != nil {
			return fmt.Errorf("checking controller source root %s: %w", stagePath.controllerPath, err)
		}
		if !info.IsDir() {
			return fmt.Errorf("checking controller source root %s: not a directory", stagePath.controllerPath)
		}
		if err := copyDirToPod(ctx, ops, podName, "stage", stagePath.controllerPath, stagePath.podPath); err != nil {
			return fmt.Errorf("staging controller source root %s to %s: %w", stagePath.controllerPath, stagePath.podPath, err)
		}
	}
	return nil
}

func skipCityRootRuntimeInputPath(relPath string, isDir bool) bool {
	if !isDir {
		return false
	}
	switch filepath.Base(relPath) {
	case ".git", ".hg", ".svn":
		return true
	default:
		return false
	}
}

func stageProviderOverlaysToPod(ctx context.Context, ops k8sOps, podName string, cfg runtime.Config, podWorkDir string, warn io.Writer) error {
	if len(cfg.PackOverlayDirs) == 0 && cfg.OverlayDir == "" {
		return nil
	}
	if podWorkDir == "" {
		podWorkDir = "/workspace"
	}

	stageDir, err := os.MkdirTemp("", "gc-k8s-overlays-")
	if err != nil {
		return fmt.Errorf("preparing provider overlays: %w", err)
	}
	defer os.RemoveAll(stageDir) //nolint:errcheck

	seedExistingInstructions(cfg.WorkDir, stageDir, warn)
	providers := runtime.OverlayProviderNames(cfg)
	for _, od := range cfg.PackOverlayDirs {
		if err := stageProviderOverlay(od, stageDir, providers, "pack overlay", warn); err != nil {
			return err
		}
	}
	if cfg.OverlayDir != "" {
		if err := stageProviderOverlay(cfg.OverlayDir, stageDir, providers, "overlay", warn); err != nil {
			return err
		}
	}
	if err := copyDirToPod(ctx, ops, podName, "stage", stageDir, podWorkDir); err != nil {
		return fmt.Errorf("staging provider overlays: %w", err)
	}
	return nil
}

func seedExistingInstructions(workDir, stageDir string, warn io.Writer) {
	if workDir == "" {
		return
	}
	src := filepath.Join(workDir, "AGENTS.md")
	if _, err := os.Stat(src); os.IsNotExist(err) {
		return
	} else if err != nil {
		fmt.Fprintf(warn, "gc: warning: checking existing AGENTS.md: %v\n", err) //nolint:errcheck
		return
	}
	if err := runtime.StagePath(src, filepath.Join(stageDir, "AGENTS.md")); err != nil {
		fmt.Fprintf(warn, "gc: warning: preserving existing AGENTS.md: %v\n", err) //nolint:errcheck
	}
}

func stageProviderOverlay(srcDir, dstDir string, providers []string, label string, warn io.Writer) error {
	var warnings bytes.Buffer
	if err := runtime.StageProviderOverlayDir(srcDir, dstDir, providers, &warnings); err != nil {
		return fmt.Errorf("staging %s %s: %w", label, srcDir, err)
	}
	if warnings.Len() > 0 {
		fmt.Fprintf(warn, "gc: warning: staging %s %s: %s\n", label, srcDir, strings.TrimSpace(warnings.String())) //nolint:errcheck
	}
	return nil
}

// waitForInitContainer waits for the named init container to be running.
func waitForInitContainer(ctx context.Context, ops k8sOps, podName string, containerName string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		pod, err := ops.getPod(ctx, podName)
		if err != nil {
			time.Sleep(time.Second)
			continue
		}
		for _, status := range pod.Status.InitContainerStatuses {
			if status.Name != containerName {
				continue
			}
			state := status.State
			if state.Running != nil {
				return nil
			}
			if state.Terminated != nil {
				// Already finished (shouldn't happen since it waits for sentinel).
				return nil
			}
		}
		time.Sleep(time.Second)
	}
	return fmt.Errorf("init container %s not running in pod %s after %s", containerName, podName, timeout)
}

// copyDirToPod copies a local directory into the pod via tar-based exec.
func copyDirToPod(ctx context.Context, ops k8sOps, podName, container, srcDir, dstDir string) error {
	info, err := os.Stat(srcDir)
	if err != nil || !info.IsDir() {
		return nil // skip silently if not a directory
	}

	// Create destination directory in the pod.
	_, _ = ops.execInPod(ctx, podName, container,
		[]string{"mkdir", "-p", dstDir}, nil)

	// Build tar archive of the source directory.
	var buf bytes.Buffer
	if err := tarDir(srcDir, &buf); err != nil {
		return fmt.Errorf("creating tar of %s: %w", srcDir, err)
	}

	// Extract in the pod.
	_, err = ops.execInPod(ctx, podName, container,
		[]string{"tar", "xf", "-", "-C", dstDir}, &buf)
	return err
}

// copyToPod copies a single file or directory to the pod.
func copyToPod(ctx context.Context, ops k8sOps, podName, container, src, dst string) error {
	info, err := os.Stat(src)
	if err != nil {
		return nil // skip silently if source doesn't exist
	}

	if info.IsDir() {
		return copyDirToPod(ctx, ops, podName, container, src, dst)
	}

	// Single file: create parent dir, write via tar.
	parentDir := filepath.Dir(dst)
	_, _ = ops.execInPod(ctx, podName, container,
		[]string{"mkdir", "-p", parentDir}, nil)

	var buf bytes.Buffer
	if err := tarFile(src, info, filepath.Base(dst), &buf); err != nil {
		return fmt.Errorf("creating tar of %s: %w", src, err)
	}
	_, err = ops.execInPod(ctx, podName, container,
		[]string{"tar", "xf", "-", "-C", parentDir}, &buf)
	return err
}

// tarDir creates a tar archive of a directory's contents.
func tarDir(dir string, w io.Writer) error {
	tw := tar.NewWriter(w)
	defer func() { _ = tw.Close() }()

	return filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}

		rel, err := filepath.Rel(dir, path)
		if err != nil {
			return err
		}
		if rel == "." {
			return nil
		}

		// Dereference symlinks: use the resolved path for both stat and open
		// to avoid TOCTOU issues if the symlink target changes.
		if info.Mode()&os.ModeSymlink != 0 {
			resolved, err := filepath.EvalSymlinks(path)
			if err != nil {
				return nil // skip broken symlinks
			}
			info, err = os.Stat(resolved)
			if err != nil {
				return nil
			}
			path = resolved
		}

		// Skip sockets and other special file types unsupported by tar.
		if info.Mode()&(os.ModeSocket|os.ModeNamedPipe|os.ModeDevice) != 0 {
			return nil
		}

		header, err := tar.FileInfoHeader(info, "")
		if err != nil {
			return err
		}
		header.Name = rel
		header.Uid = 0
		header.Gid = 0
		header.Uname = ""
		header.Gname = ""

		if err := tw.WriteHeader(header); err != nil {
			return err
		}

		if info.IsDir() {
			return nil
		}

		f, err := os.Open(path)
		if err != nil {
			return err
		}
		defer func() { _ = f.Close() }()
		// Limit copy to declared header size to avoid "write too long" if
		// the file grew between stat and read (e.g., events.jsonl).
		_, err = io.CopyN(tw, f, header.Size)
		return err
	})
}

// tarFile creates a tar archive containing a single file.
func tarFile(path string, info os.FileInfo, name string, w io.Writer) error {
	tw := tar.NewWriter(w)
	defer func() { _ = tw.Close() }()

	header, err := tar.FileInfoHeader(info, "")
	if err != nil {
		return err
	}
	header.Name = name
	header.Uid = 0
	header.Gid = 0
	header.Uname = ""
	header.Gname = ""

	if err := tw.WriteHeader(header); err != nil {
		return err
	}

	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	_, err = io.Copy(tw, f)
	return err
}
