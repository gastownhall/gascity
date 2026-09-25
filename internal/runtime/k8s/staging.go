package k8s

import (
	"archive/tar"
	"bytes"
	"context"
	"errors"
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

	// Wait for exec endpoint to be ready. The kubelet reports Running
	// before the CRI exec handler is set up, so we poll a trivial command.
	if err := waitForExecReady(ctx, ops, podName, 120*time.Second); err != nil {
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
	providers := runtime.EffectiveOverlayProviderNames(cfg)
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

// waitForExecReady polls exec with a trivial command until it succeeds.
// The kubelet reports a container as Running before the CRI exec handler
// (SPDY) is fully set up, causing "container not found" errors if we
// exec too early. This is especially common on K3s with containerd.
func waitForExecReady(ctx context.Context, ops k8sOps, podName string, timeout time.Duration) error {
	const container = "stage"

	deadline := time.Now().Add(timeout)
	var lastErr error
	for time.Now().Before(deadline) {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
		_, err := ops.execInPod(ctx, podName, container, []string{"true"}, nil)
		if err == nil {
			return nil
		}
		lastErr = err
		if err := ctx.Err(); err != nil {
			return err
		}

		delay := 500 * time.Millisecond
		if remaining := time.Until(deadline); remaining < delay {
			delay = remaining
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(delay):
		}
	}
	if lastErr != nil {
		return fmt.Errorf("exec not ready in %s/%s after %s: %w", podName, container, timeout, lastErr)
	}
	return fmt.Errorf("exec not ready in %s/%s after %s", podName, container, timeout)
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

	// Stream the tar archive of the source directory straight into the pod's
	// tar extractor so the archive is never held in memory.
	err = streamArchive(
		func(w io.Writer, entriesComplete func()) error {
			if err := tarDirWithWalkComplete(srcDir, w, entriesComplete); err != nil {
				return fmt.Errorf("creating tar of %s: %w", srcDir, err)
			}
			return nil
		},
		func(r io.Reader) error {
			output, execErr := ops.execInPod(ctx, podName, container, tarExtractCommand(dstDir), r)
			if execErr != nil {
				return execErr
			}
			if strings.TrimSpace(output) != archiveExtractAck {
				return errors.New("pod tar extraction did not acknowledge completion")
			}
			return nil
		})
	if err != nil {
		return fmt.Errorf("copying directory %s to pod %s:%s: %w", srcDir, podName, dstDir, err)
	}
	return nil
}

var (
	// errArchiveConsumerDone is delivered to the producer once the consumer
	// returns, so a producer still writing unblocks and stops.
	errArchiveConsumerDone = errors.New("archive consumer finished")
	errArchiveIncomplete   = errors.New("tar archive upload incomplete")
)

const archiveExtractAck = "gc-stage-ok"

func tarExtractCommand(dstDir string) []string {
	return []string{"sh", "-c", `tar xf - -C "$1" && printf '%s\n' "$2"`, "sh", dstDir, archiveExtractAck}
}

// streamArchive pipes what produce writes into consume without buffering the
// whole archive. Memory stays bounded because the pipe is unbuffered: produce
// blocks until consume reads. Once consume returns, it closes the pipe and
// waits for the producer. The pod transport itself may still block before
// consume returns if the remote side stops reading.
//
// A producer failure is returned in preference to the consumer's error, which
// may be caused by the truncated stream. If the consumer returns success before
// the producer finishes the entries, the upload is incomplete. A tar extractor
// may stop reading after the first zero trailer block, so completing the walk
// (rather than writing every trailer byte) is the success boundary.
func streamArchive(produce func(io.Writer, func()) error, consume func(io.Reader) error) error {
	pr, pw := io.Pipe()
	type producerResult struct {
		err             error
		entriesComplete bool
	}
	produced := make(chan producerResult, 1)
	go func() {
		entriesComplete := false
		err := produce(pw, func() { entriesComplete = true })
		_ = pw.CloseWithError(err) // nil closes with io.EOF
		produced <- producerResult{err: err, entriesComplete: entriesComplete}
	}()

	defer pr.CloseWithError(errArchiveConsumerDone) // also release the producer if consume panics
	consumeErr := consume(pr)
	_ = pr.CloseWithError(errArchiveConsumerDone)
	result := <-produced

	if result.err != nil && !errors.Is(result.err, errArchiveConsumerDone) {
		return result.err
	}
	if consumeErr != nil {
		return consumeErr
	}
	if !result.entriesComplete {
		return errArchiveIncomplete
	}
	return nil
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
//
// The end-of-archive trailer is written only on success. Some tar extractors
// also accept a stream with no trailer, so callers must check the producer error.
func tarDir(dir string, w io.Writer) error {
	return tarDirWithWalkComplete(dir, w, nil)
}

func tarDirWithWalkComplete(dir string, w io.Writer, onComplete func()) error {
	tw := tar.NewWriter(w)
	if err := walkTar(dir, tw); err != nil {
		return err
	}
	if onComplete != nil {
		onComplete()
	}
	return tw.Close()
}

func walkTar(dir string, tw *tar.Writer) error {
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
