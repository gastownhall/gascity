package k8s

import (
	"archive/tar"
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/runtime"
	corev1 "k8s.io/api/core/v1"
)

func TestTarDirStripsOwnership(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "sub", "file.txt"), []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}

	var buf bytes.Buffer
	if err := tarDir(dir, &buf); err != nil {
		t.Fatal(err)
	}

	tr := tar.NewReader(&buf)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if hdr.Uid != 0 || hdr.Gid != 0 {
			t.Errorf("entry %q: want UID/GID 0/0, got %d/%d", hdr.Name, hdr.Uid, hdr.Gid)
		}
		if hdr.Uname != "" || hdr.Gname != "" {
			t.Errorf("entry %q: want empty Uname/Gname, got %q/%q", hdr.Name, hdr.Uname, hdr.Gname)
		}
	}
}

func TestTarFileStripsOwnership(t *testing.T) {
	f := filepath.Join(t.TempDir(), "test.txt")
	if err := os.WriteFile(f, []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}

	info, err := os.Stat(f)
	if err != nil {
		t.Fatal(err)
	}

	var buf bytes.Buffer
	if err := tarFile(f, info, "test.txt", &buf); err != nil {
		t.Fatal(err)
	}

	tr := tar.NewReader(&buf)
	hdr, err := tr.Next()
	if err != nil {
		t.Fatal(err)
	}
	if hdr.Uid != 0 || hdr.Gid != 0 {
		t.Errorf("want UID/GID 0/0, got %d/%d", hdr.Uid, hdr.Gid)
	}
	if hdr.Uname != "" || hdr.Gname != "" {
		t.Errorf("want empty Uname/Gname, got %q/%q", hdr.Uname, hdr.Gname)
	}
}

func TestStageFilesStagesKiroPackOverlayAtWorkspaceRoot(t *testing.T) {
	workDir := t.TempDir()
	projectInstructions := filepath.Join(workDir, "AGENTS.md")
	if err := os.WriteFile(projectInstructions, []byte("project instructions"), 0o600); err != nil {
		t.Fatalf("WriteFile(%q): %v", projectInstructions, err)
	}

	packOverlay := t.TempDir()
	agentConfig := filepath.Join(packOverlay, "per-provider", "kiro", ".kiro", "agents", "gascity.json")
	if err := os.MkdirAll(filepath.Dir(agentConfig), 0o755); err != nil {
		t.Fatalf("MkdirAll(%q): %v", filepath.Dir(agentConfig), err)
	}
	if err := os.WriteFile(agentConfig, []byte(`{"name":"gascity"}`), 0o644); err != nil {
		t.Fatalf("WriteFile(%q): %v", agentConfig, err)
	}
	fallbackInstructions := filepath.Join(packOverlay, "per-provider", "kiro", "AGENTS.md")
	if err := os.WriteFile(fallbackInstructions, []byte("fallback instructions"), 0o644); err != nil {
		t.Fatalf("WriteFile(%q): %v", fallbackInstructions, err)
	}

	ops := newCapturingStageOps()
	err := stageFiles(context.Background(), ops, "gc-kiro", runtime.Config{
		WorkDir:         workDir,
		ProviderName:    "kiro",
		PackOverlayDirs: []string{packOverlay},
	}, "", io.Discard)
	if err != nil {
		t.Fatalf("stageFiles: %v", err)
	}

	if got := ops.files["/workspace/.kiro/agents/gascity.json"]; got != `{"name":"gascity"}` {
		t.Fatalf("staged Kiro agent config = %q, want root gascity config", got)
	}
	if _, ok := ops.files["/workspace/per-provider/kiro/.kiro/agents/gascity.json"]; ok {
		t.Fatal("Kiro provider overlay should be flattened, not staged under per-provider/kiro")
	}
	if got := ops.files["/workspace/AGENTS.md"]; got != "project instructions" {
		t.Fatalf("staged AGENTS.md = %q, want project instructions preserved", got)
	}
}

func TestStageFilesStagesKiroPackOverlayAtPodWorkDirForRigWorkDir(t *testing.T) {
	cityRoot := t.TempDir()
	workDir := filepath.Join(cityRoot, "rigs", "team")
	if err := os.MkdirAll(workDir, 0o755); err != nil {
		t.Fatalf("MkdirAll(%q): %v", workDir, err)
	}
	rigInstructions := filepath.Join(workDir, "AGENTS.md")
	if err := os.WriteFile(rigInstructions, []byte("rig instructions"), 0o600); err != nil {
		t.Fatalf("WriteFile(%q): %v", rigInstructions, err)
	}
	rigFile := filepath.Join(workDir, "task.txt")
	if err := os.WriteFile(rigFile, []byte("rig payload"), 0o644); err != nil {
		t.Fatalf("WriteFile(%q): %v", rigFile, err)
	}

	packOverlay := t.TempDir()
	agentConfig := filepath.Join(packOverlay, "per-provider", "kiro", ".kiro", "agents", "gascity.json")
	if err := os.MkdirAll(filepath.Dir(agentConfig), 0o755); err != nil {
		t.Fatalf("MkdirAll(%q): %v", filepath.Dir(agentConfig), err)
	}
	if err := os.WriteFile(agentConfig, []byte(`{"name":"gascity"}`), 0o644); err != nil {
		t.Fatalf("WriteFile(%q): %v", agentConfig, err)
	}
	fallbackInstructions := filepath.Join(packOverlay, "per-provider", "kiro", "AGENTS.md")
	if err := os.WriteFile(fallbackInstructions, []byte("fallback instructions"), 0o644); err != nil {
		t.Fatalf("WriteFile(%q): %v", fallbackInstructions, err)
	}

	ops := newCapturingStageOps()
	err := stageFiles(context.Background(), ops, "gc-kiro", runtime.Config{
		WorkDir:         workDir,
		ProviderName:    "kiro",
		PackOverlayDirs: []string{packOverlay},
	}, cityRoot, io.Discard)
	if err != nil {
		t.Fatalf("stageFiles: %v", err)
	}

	if got := ops.files["/workspace/rigs/team/.kiro/agents/gascity.json"]; got != `{"name":"gascity"}` {
		t.Fatalf("staged Kiro agent config = %q, want rig workdir gascity config", got)
	}
	if _, ok := ops.files["/workspace/.kiro/agents/gascity.json"]; ok {
		t.Fatal("rig-mode Kiro agent config should be staged under pod workdir, not workspace root")
	}
	if _, ok := ops.files["/workspace/per-provider/kiro/.kiro/agents/gascity.json"]; ok {
		t.Fatal("Kiro provider overlay should be flattened, not staged under per-provider/kiro")
	}
	if got := ops.files["/workspace/rigs/team/AGENTS.md"]; got != "rig instructions" {
		t.Fatalf("staged rig AGENTS.md = %q, want rig instructions preserved", got)
	}
	if got := ops.files["/workspace/rigs/team/task.txt"]; got != "rig payload" {
		t.Fatalf("staged rig workdir payload = %q, want copied under rig-relative workspace path", got)
	}
}

func TestStageFilesUsesConcreteProviderOverlayName(t *testing.T) {
	workDir := t.TempDir()
	packOverlay := t.TempDir()

	kiroConfig := filepath.Join(packOverlay, "per-provider", "kiro", ".kiro", "agents", "gascity.json")
	if err := os.MkdirAll(filepath.Dir(kiroConfig), 0o755); err != nil {
		t.Fatalf("MkdirAll(%q): %v", filepath.Dir(kiroConfig), err)
	}
	if err := os.WriteFile(kiroConfig, []byte(`{"name":"gascity"}`), 0o644); err != nil {
		t.Fatalf("WriteFile(%q): %v", kiroConfig, err)
	}
	claudeInstructions := filepath.Join(packOverlay, "per-provider", "claude", "CLAUDE.md")
	if err := os.MkdirAll(filepath.Dir(claudeInstructions), 0o755); err != nil {
		t.Fatalf("MkdirAll(%q): %v", filepath.Dir(claudeInstructions), err)
	}
	if err := os.WriteFile(claudeInstructions, []byte("claude instructions"), 0o644); err != nil {
		t.Fatalf("WriteFile(%q): %v", claudeInstructions, err)
	}

	ops := newCapturingStageOps()
	err := stageFiles(context.Background(), ops, "gc-kiro", runtime.Config{
		WorkDir:             workDir,
		ProviderName:        "claude",
		ProviderOverlayName: "kiro",
		PackOverlayDirs:     []string{packOverlay},
	}, "", io.Discard)
	if err != nil {
		t.Fatalf("stageFiles: %v", err)
	}

	if got := ops.files["/workspace/.kiro/agents/gascity.json"]; got != `{"name":"gascity"}` {
		t.Fatalf("staged Kiro agent config = %q, want root gascity config", got)
	}
	if _, ok := ops.files["/workspace/CLAUDE.md"]; ok {
		t.Fatal("staged Claude overlay for Kiro provider inheriting Claude launch behavior")
	}
}

func TestStageFilesSurfacesKiroPreservationWarning(t *testing.T) {
	workDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(workDir, "AGENTS.md"), []byte("project instructions"), 0o600); err != nil {
		t.Fatalf("write project instructions: %v", err)
	}

	packOverlay := t.TempDir()
	fallbackInstructions := filepath.Join(packOverlay, "per-provider", "kiro", "AGENTS.md")
	if err := os.MkdirAll(filepath.Dir(fallbackInstructions), 0o755); err != nil {
		t.Fatalf("mkdir Kiro fallback instructions: %v", err)
	}
	if err := os.WriteFile(fallbackInstructions, []byte("fallback instructions"), 0o644); err != nil {
		t.Fatalf("write Kiro fallback instructions: %v", err)
	}

	var warnings bytes.Buffer
	ops := newCapturingStageOps()
	err := stageFiles(context.Background(), ops, "gc-kiro", runtime.Config{
		WorkDir:         workDir,
		ProviderName:    "kiro",
		PackOverlayDirs: []string{packOverlay},
	}, "", &warnings)
	if err != nil {
		t.Fatalf("stageFiles: %v", err)
	}
	if got := ops.files["/workspace/AGENTS.md"]; got != "project instructions" {
		t.Fatalf("staged AGENTS.md = %q, want project instructions preserved", got)
	}
	if got := warnings.String(); !strings.Contains(got, "overlay: preserving existing") || !strings.Contains(got, "AGENTS.md") {
		t.Fatalf("warnings = %q, want Kiro preservation warning", got)
	}
}

func TestStageFilesPropagatesFatalProviderOverlayError(t *testing.T) {
	workDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(workDir, "AGENTS.md"), []byte("project instructions"), 0o600); err != nil {
		t.Fatalf("write project instructions: %v", err)
	}

	packOverlay := t.TempDir()
	nestedInstructions := filepath.Join(packOverlay, "per-provider", "kiro", "AGENTS.md", "nested.md")
	if err := os.MkdirAll(filepath.Dir(nestedInstructions), 0o755); err != nil {
		t.Fatalf("mkdir Kiro nested instructions: %v", err)
	}
	if err := os.WriteFile(nestedInstructions, []byte("nested instructions"), 0o644); err != nil {
		t.Fatalf("write Kiro nested instructions: %v", err)
	}

	var warnings bytes.Buffer
	ops := newCapturingStageOps()
	err := stageFiles(context.Background(), ops, "gc-kiro", runtime.Config{
		WorkDir:         workDir,
		ProviderName:    "kiro",
		PackOverlayDirs: []string{packOverlay},
	}, "", &warnings)
	if err == nil {
		t.Fatal("stageFiles succeeded, want fatal provider overlay error")
	}
	if got := err.Error(); !strings.Contains(got, "staging pack overlay") || !strings.Contains(got, "AGENTS.md") {
		t.Fatalf("stageFiles error = %q, want pack overlay AGENTS.md context", got)
	}
	if strings.Contains(warnings.String(), "staging pack overlay") {
		t.Fatalf("fatal provider overlay error was demoted to warning: %q", warnings.String())
	}
}

func TestWaitForExecReadySucceedsImmediately(t *testing.T) {
	ops := &execReadyOps{}

	if err := waitForExecReady(context.Background(), ops, "pod", time.Second); err != nil {
		t.Fatalf("waitForExecReady: %v", err)
	}
	if got := ops.calls; got != 1 {
		t.Fatalf("exec calls = %d, want 1", got)
	}
	if got := ops.commands[0]; len(got) != 1 || got[0] != "true" {
		t.Fatalf("probe command = %v, want [true]", got)
	}
}

func TestWaitForExecReadyRetriesTransientErrors(t *testing.T) {
	ops := &execReadyOps{
		errors: []error{
			errors.New("container not found"),
			errors.New("container not found"),
			nil,
		},
	}

	if err := waitForExecReady(context.Background(), ops, "pod", 2*time.Second); err != nil {
		t.Fatalf("waitForExecReady: %v", err)
	}
	if got := ops.calls; got != 3 {
		t.Fatalf("exec calls = %d, want 3", got)
	}
}

func TestWaitForExecReadyTimeoutPreservesLastError(t *testing.T) {
	ops := &execReadyOps{errors: []error{errors.New("spdy endpoint unavailable")}}

	err := waitForExecReady(context.Background(), ops, "pod", time.Millisecond)
	if err == nil {
		t.Fatal("waitForExecReady succeeded, want timeout error")
	}
	if !strings.Contains(err.Error(), "exec not ready in pod/stage after 1ms") {
		t.Fatalf("error = %q, want timeout context", err)
	}
	if !errors.Is(err, ops.errors[0]) {
		t.Fatalf("error = %v, want wrapped last exec error %v", err, ops.errors[0])
	}
}

func TestWaitForExecReadyReturnsContextCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	ops := &execReadyOps{errors: []error{errors.New("container not found")}}
	cancel()

	err := waitForExecReady(ctx, ops, "pod", time.Second)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("waitForExecReady error = %v, want context.Canceled", err)
	}
	if got := ops.calls; got != 0 {
		t.Fatalf("exec calls after context cancellation = %d, want 0", got)
	}
}

func TestWaitForExecReadyReturnsContextCancellationDuringDelay(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	firstProbe := make(chan struct{})
	ops := &execReadyOps{
		errors: []error{errors.New("container not found")},
		afterExec: func() {
			go func() {
				time.Sleep(10 * time.Millisecond)
				cancel()
			}()
		},
		firstProbeCh: firstProbe,
	}

	err := waitForExecReady(ctx, ops, "pod", time.Second)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("waitForExecReady error = %v, want context.Canceled", err)
	}
	select {
	case <-firstProbe:
	default:
		t.Fatal("exec probe was not attempted before cancellation")
	}
	if got := ops.calls; got != 1 {
		t.Fatalf("exec calls = %d, want 1", got)
	}
}

type capturingStageOps struct {
	files map[string]string
}

func newCapturingStageOps() *capturingStageOps {
	return &capturingStageOps{files: make(map[string]string)}
}

func (o *capturingStageOps) createPod(context.Context, *corev1.Pod) (*corev1.Pod, error) {
	return nil, nil
}

func (o *capturingStageOps) getPod(context.Context, string) (*corev1.Pod, error) {
	return &corev1.Pod{
		Status: corev1.PodStatus{
			InitContainerStatuses: []corev1.ContainerStatus{{
				State: corev1.ContainerState{
					Running: &corev1.ContainerStateRunning{},
				},
			}},
		},
	}, nil
}

func (o *capturingStageOps) deletePod(context.Context, string, int64) error {
	return nil
}

func (o *capturingStageOps) listPods(context.Context, string, string) ([]corev1.Pod, error) {
	return nil, nil
}

func (o *capturingStageOps) execInPod(_ context.Context, _, _ string, cmd []string, stdin io.Reader) (string, error) {
	if len(cmd) == 6 && cmd[0] == "sh" && cmd[4] != "" && cmd[5] == archiveExtractAck && stdin != nil {
		tr := tar.NewReader(stdin)
		for {
			hdr, err := tr.Next()
			if errors.Is(err, io.EOF) {
				break
			}
			if err != nil {
				return "", err
			}
			if hdr.FileInfo().IsDir() {
				continue
			}
			data, err := io.ReadAll(tr)
			if err != nil {
				return "", err
			}
			o.files[path.Join(cmd[4], hdr.Name)] = string(data)
		}
		return archiveExtractAck + "\n", nil
	}
	return "", nil
}

type execReadyOps struct {
	errors       []error
	calls        int
	commands     [][]string
	afterExec    func()
	firstProbeCh chan<- struct{}
}

func (o *execReadyOps) createPod(context.Context, *corev1.Pod) (*corev1.Pod, error) {
	return nil, nil
}

func (o *execReadyOps) getPod(context.Context, string) (*corev1.Pod, error) {
	return nil, nil
}

func (o *execReadyOps) deletePod(context.Context, string, int64) error {
	return nil
}

func (o *execReadyOps) listPods(context.Context, string, string) ([]corev1.Pod, error) {
	return nil, nil
}

func (o *execReadyOps) execInPod(_ context.Context, _, _ string, cmd []string, _ io.Reader) (string, error) {
	o.calls++
	o.commands = append(o.commands, append([]string(nil), cmd...))
	if o.firstProbeCh != nil && o.calls == 1 {
		close(o.firstProbeCh)
	}
	if o.afterExec != nil {
		o.afterExec()
	}
	if o.calls <= len(o.errors) {
		return "", o.errors[o.calls-1]
	}
	return "", nil
}

// streamStageOps is a k8sOps whose tar-extract exec is driven by a test hook,
// so tests control how (and whether) the streamed archive is consumed.
type streamStageOps struct {
	capturingStageOps
	onTarStdin func(ctx context.Context, stdin io.Reader) error
	omitAck    bool
}

type stagingWriteFunc func([]byte) (int, error)

func (f stagingWriteFunc) Write(p []byte) (int, error) { return f(p) }

func (o *streamStageOps) execInPod(ctx context.Context, pod, container string, cmd []string, stdin io.Reader) (string, error) {
	if len(cmd) == 6 && cmd[0] == "sh" && stdin != nil {
		if err := o.onTarStdin(ctx, stdin); err != nil {
			return "", err
		}
		if o.omitAck {
			return "", nil
		}
		return archiveExtractAck + "\n", nil
	}
	return o.capturingStageOps.execInPod(ctx, pod, container, cmd, stdin)
}

// writeLargeTree fills dir with files whose archive far exceeds any pipe or
// tar block size, so a producer cannot finish without the consumer reading.
func writeLargeTree(t *testing.T, dir string) {
	t.Helper()
	chunk := bytes.Repeat([]byte("0123456789abcdef"), 64*1024) // 1 MiB
	for i := range 4 {
		if err := os.WriteFile(filepath.Join(dir, "big"+string(rune('0'+i))), chunk, 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func runWithin(t *testing.T, name string, fn func()) {
	t.Helper()
	done := make(chan struct{})
	go func() {
		defer close(done)
		fn()
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatalf("%s did not return: producer or consumer left blocked", name)
	}
}

func TestCopyDirToPodStreamsArchiveToExec(t *testing.T) {
	src := t.TempDir()
	writeLargeTree(t, src)
	if err := os.MkdirAll(filepath.Join(src, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, "sub", "small.txt"), []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}

	got := map[string]int{}
	replacement := bytes.Repeat([]byte("changed!"), 128*1024) // 1 MiB
	ops := &streamStageOps{onTarStdin: func(_ context.Context, stdin io.Reader) error {
		// The producer must still be blocked on an earlier entry here. A
		// prebuilt archive would contain the old big3 bytes.
		if err := os.WriteFile(filepath.Join(src, "big3"), replacement, 0o644); err != nil {
			return err
		}
		tr := tar.NewReader(stdin)
		for {
			hdr, err := tr.Next()
			if errors.Is(err, io.EOF) {
				return nil
			}
			if err != nil {
				return err
			}
			body, err := io.ReadAll(tr)
			if err != nil {
				return err
			}
			got[hdr.Name] = len(body)
			if hdr.Name == "big3" && !bytes.Equal(body, replacement) {
				return errors.New("big3 was archived before exec began consuming stdin")
			}
		}
	}}

	if err := copyDirToPod(context.Background(), ops, "pod", "stage", src, "/dst"); err != nil {
		t.Fatalf("copyDirToPod: %v", err)
	}
	for name, want := range map[string]int{"big0": 1 << 20, "big3": 1 << 20, "sub/small.txt": 5} {
		if got[name] != want {
			t.Errorf("entry %q size = %d, want %d (entries: %v)", name, got[name], want, got)
		}
	}
}

func TestCopyDirToPodPreservesArchiveContents(t *testing.T) {
	src := t.TempDir()
	writeLargeTree(t, src)
	ops := newCapturingStageOps()

	if err := copyDirToPod(context.Background(), ops, "pod", "stage", src, "/dst"); err != nil {
		t.Fatalf("copyDirToPod: %v", err)
	}
	want, err := os.ReadFile(filepath.Join(src, "big2"))
	if err != nil {
		t.Fatal(err)
	}
	if ops.files["/dst/big2"] != string(want) {
		t.Fatalf("extracted /dst/big2 differs from source (len %d vs %d)", len(ops.files["/dst/big2"]), len(want))
	}
}

func TestCopyDirToPodExecFailureBeforeReadingReleasesProducer(t *testing.T) {
	src := t.TempDir()
	writeLargeTree(t, src)
	execErr := errors.New("tar: not found")
	ops := &streamStageOps{onTarStdin: func(context.Context, io.Reader) error { return execErr }}

	var err error
	runWithin(t, "copyDirToPod", func() {
		err = copyDirToPod(context.Background(), ops, "pod", "stage", src, "/dst")
	})
	if !errors.Is(err, execErr) {
		t.Fatalf("copyDirToPod error = %v, want %v", err, execErr)
	}
}

func TestCopyDirToPodExecFailureMidStreamReleasesProducer(t *testing.T) {
	src := t.TempDir()
	writeLargeTree(t, src)
	execErr := errors.New("stream reset by peer")
	ops := &streamStageOps{onTarStdin: func(_ context.Context, stdin io.Reader) error {
		if _, err := io.ReadFull(stdin, make([]byte, 64*1024)); err != nil {
			return err
		}
		return execErr
	}}

	var err error
	runWithin(t, "copyDirToPod", func() {
		err = copyDirToPod(context.Background(), ops, "pod", "stage", src, "/dst")
	})
	if !errors.Is(err, execErr) {
		t.Fatalf("copyDirToPod error = %v, want %v", err, execErr)
	}
}

func TestCopyDirToPodCancellationReleasesProducer(t *testing.T) {
	src := t.TempDir()
	writeLargeTree(t, src)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ops := &streamStageOps{onTarStdin: func(ctx context.Context, stdin io.Reader) error {
		if _, err := io.ReadFull(stdin, make([]byte, 64*1024)); err != nil {
			return err
		}
		cancel()
		<-ctx.Done() // the real exec stops streaming when its context ends
		return ctx.Err()
	}}

	var err error
	runWithin(t, "copyDirToPod", func() {
		err = copyDirToPod(ctx, ops, "pod", "stage", src, "/dst")
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("copyDirToPod error = %v, want context.Canceled", err)
	}
}

func TestTarDirWalkFailureDoesNotWriteTrailer(t *testing.T) {
	src := t.TempDir()
	payload := bytes.Repeat([]byte("x"), 512)
	if err := os.WriteFile(filepath.Join(src, "a.txt"), payload, 0o644); err != nil {
		t.Fatal(err)
	}
	removedDir := filepath.Join(src, "z-dir")
	if err := os.Mkdir(removedDir, 0o755); err != nil {
		t.Fatal(err)
	}
	var archive bytes.Buffer
	removed := false
	w := stagingWriteFunc(func(p []byte) (int, error) {
		n, err := archive.Write(p)
		if err == nil && !removed && bytes.Equal(p, payload) {
			removed = true
			if removeErr := os.Remove(removedDir); removeErr != nil {
				return n, removeErr
			}
		}
		return n, err
	})
	err := tarDir(src, w)
	if !os.IsNotExist(err) {
		t.Fatalf("tarDir error = %v, want missing next directory", err)
	}
	if !removed {
		t.Fatal("test did not remove the directory during the walk")
	}
	if archive.Len() != 1024 { // one complete header and 512-byte body, no trailer
		t.Fatalf("archive length = %d, want 1024 without a success trailer", archive.Len())
	}
}

func TestCopyDirToPodShortSuccessfulExecIsIncomplete(t *testing.T) {
	src := t.TempDir()
	writeLargeTree(t, src)
	ops := &streamStageOps{onTarStdin: func(_ context.Context, stdin io.Reader) error {
		_, err := io.ReadFull(stdin, make([]byte, 64*1024))
		return err // an exec that returns success after reading only part of stdin
	}}
	if err := copyDirToPod(context.Background(), ops, "pod", "stage", src, "/dst"); !errors.Is(err, errArchiveIncomplete) {
		t.Fatalf("copyDirToPod error = %v, want incomplete archive", err)
	}
}

func TestCopyDirToPodRequiresPodAcknowledgement(t *testing.T) {
	src := t.TempDir()
	if err := os.WriteFile(filepath.Join(src, "file"), []byte("contents"), 0o644); err != nil {
		t.Fatal(err)
	}
	ops := &streamStageOps{
		onTarStdin: func(_ context.Context, stdin io.Reader) error {
			_, err := io.Copy(io.Discard, stdin)
			return err
		},
		omitAck: true,
	}
	if err := copyDirToPod(context.Background(), ops, "pod", "stage", src, "/dst"); err == nil || !strings.Contains(err.Error(), "did not acknowledge") {
		t.Fatalf("copyDirToPod error = %v, want missing pod acknowledgement", err)
	}
}

func TestCopyDirToPodAcceptsFirstTarTrailerBlock(t *testing.T) {
	src := t.TempDir()
	if err := os.WriteFile(filepath.Join(src, "file"), bytes.Repeat([]byte("x"), 512), 0o644); err != nil {
		t.Fatal(err)
	}
	ops := &streamStageOps{onTarStdin: func(_ context.Context, stdin io.Reader) error {
		// Header, 512-byte body, and the first zero trailer block. The tar
		// writer can still be blocked writing its second trailer block.
		_, err := io.CopyN(io.Discard, stdin, 3*512)
		return err
	}}
	if err := copyDirToPod(context.Background(), ops, "pod", "stage", src, "/dst"); err != nil {
		t.Fatalf("copyDirToPod: %v", err)
	}
}

func TestStreamArchiveProducerFailureIsReturnedAndConsumerSeesError(t *testing.T) {
	produceErr := errors.New("walk failed")
	var consumerErr error
	err := streamArchive(
		func(w io.Writer, _ func()) error {
			_, _ = w.Write([]byte("partial"))
			return produceErr
		},
		func(r io.Reader) error {
			_, consumerErr = io.ReadAll(r)
			return consumerErr
		})
	if !errors.Is(err, produceErr) {
		t.Fatalf("streamArchive error = %v, want %v", err, produceErr)
	}
	if !errors.Is(consumerErr, produceErr) {
		t.Fatalf("consumer read error = %v, want the producer error", consumerErr)
	}
}

func TestStreamArchiveConsumerStopsEarlyUnblocksProducer(t *testing.T) {
	var writeErr error
	returned := false
	err := streamArchive(
		func(w io.Writer, _ func()) error {
			defer func() { returned = true }()
			buf := make([]byte, 32*1024)
			for {
				if _, err := w.Write(buf); err != nil {
					writeErr = err
					return err
				}
			}
		},
		func(r io.Reader) error {
			_, err := io.ReadFull(r, make([]byte, 1024))
			return err // returns nil without draining the stream
		})
	if !errors.Is(err, errArchiveIncomplete) {
		t.Fatalf("streamArchive error = %v, want incomplete archive", err)
	}
	if !returned {
		t.Fatal("streamArchive returned while the producer was still running")
	}
	if !errors.Is(writeErr, errArchiveConsumerDone) {
		t.Fatalf("producer write error = %v, want errArchiveConsumerDone", writeErr)
	}
}

func TestStreamArchiveConsumerPanicReleasesProducer(t *testing.T) {
	producerDone := make(chan struct{})
	func() {
		defer func() {
			if recovered := recover(); recovered != "consumer panic" {
				t.Errorf("recovered %v, want consumer panic", recovered)
			}
		}()
		_ = streamArchive(
			func(w io.Writer, _ func()) error {
				defer close(producerDone)
				for {
					if _, err := w.Write(make([]byte, 32*1024)); err != nil {
						return err
					}
				}
			},
			func(r io.Reader) error {
				if _, err := io.ReadFull(r, make([]byte, 1024)); err != nil {
					return err
				}
				panic("consumer panic")
			})
	}()
	select {
	case <-producerDone:
	case <-time.After(10 * time.Second):
		t.Fatal("producer remained blocked after consumer panic")
	}
}
