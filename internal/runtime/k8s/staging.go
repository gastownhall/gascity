package k8s

import (
	"archive/tar"
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/runtime"
)

// stageFiles copies overlay, copy_files, and rig workdir into the pod
// via the init container, then signals it to exit.
func stageFiles(ctx context.Context, ops k8sOps, podName string, cfg runtime.Config, ctrlCity string, warn io.Writer) error {
	// Wait for init container to be running (up to 60s).
	if err := waitForInitContainer(ctx, ops, podName, 60*time.Second); err != nil {
		return err
	}

	// Copy rig work_dir into the pod.
	podWorkDir := "/workspace"
	if ctrlCity != "" && cfg.WorkDir != "" && cfg.WorkDir != ctrlCity {
		if rel, ok := strings.CutPrefix(cfg.WorkDir, ctrlCity+"/"); ok {
			podWorkDir = "/workspace/" + rel
		}
	}
	if cfg.WorkDir != "" && cfg.WorkDir != ctrlCity {
		if err := copyDirToPod(ctx, ops, podName, "stage", cfg.WorkDir, podWorkDir); err != nil {
			fmt.Fprintf(warn, "gc: warning: staging workdir %s to %s: %v\n", cfg.WorkDir, podWorkDir, err) //nolint:errcheck
		}
	}

	// Copy pack-level overlays (lower priority, merged additively).
	for _, od := range cfg.PackOverlayDirs {
		if err := copyDirToPod(ctx, ops, podName, "stage", od, "/workspace"); err != nil {
			fmt.Fprintf(warn, "gc: warning: staging pack overlay %s: %v\n", od, err) //nolint:errcheck
		}
	}

	// Copy agent overlay_dir (highest priority, overwrites on conflict).
	if cfg.OverlayDir != "" {
		if err := copyDirToPod(ctx, ops, podName, "stage", cfg.OverlayDir, "/workspace"); err != nil {
			fmt.Fprintf(warn, "gc: warning: staging overlay %s: %v\n", cfg.OverlayDir, err) //nolint:errcheck
		}
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

// waitForInitContainer waits for the init container to be running.
func waitForInitContainer(ctx context.Context, ops k8sOps, podName string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		pod, err := ops.getPod(ctx, podName)
		if err != nil {
			time.Sleep(time.Second)
			continue
		}
		if len(pod.Status.InitContainerStatuses) > 0 {
			state := pod.Status.InitContainerStatuses[0].State
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
	return fmt.Errorf("init container not running in pod %s after %s", podName, timeout)
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
