package k8s

import (
	"archive/tar"
	"bytes"
	"context"
	"errors"
	"fmt"
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

func TestStageFilesPreservesOwnedCodexHookAndStagesUnownedSiblings(t *testing.T) {
	cityRoot := t.TempDir()
	workDir := filepath.Join(cityRoot, "sessions", "operator")
	canonicalPath := filepath.Join(workDir, ".codex", "hooks.json")
	if err := os.MkdirAll(filepath.Dir(canonicalPath), 0o755); err != nil {
		t.Fatalf("mkdir canonical hook dir: %v", err)
	}
	canonical := `{"hooks":{"SessionStart":[{"matcher":"startup"}]}}`
	if err := os.WriteFile(canonicalPath, []byte(canonical), 0o644); err != nil {
		t.Fatalf("write canonical hook: %v", err)
	}

	packOverlay := t.TempDir()
	for rel, contents := range map[string]string{
		filepath.Join("per-provider", "codex", ".codex", "hooks.json"): `{"hooks":{"SessionStart":[{"matcher":""}]}}`,
		filepath.Join("per-provider", "codex", "AGENTS.codex.md"):      "codex sibling",
		filepath.Join(".gemini", "settings.json"):                      `{"hooks":{"BeforeAgent":[]}}`,
	} {
		path := filepath.Join(packOverlay, rel)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatalf("mkdir overlay %s: %v", rel, err)
		}
		if err := os.WriteFile(path, []byte(contents), 0o644); err != nil {
			t.Fatalf("write overlay %s: %v", rel, err)
		}
	}

	ops := newCapturingStageOps()
	err := stageFiles(context.Background(), ops, "gc-codex", runtime.Config{
		WorkDir:                       workDir,
		ProviderName:                  "codex",
		PackOverlayDirs:               []string{packOverlay},
		ReconcilerOwnedMergeablePaths: []string{filepath.Join(".codex", "hooks.json")},
		CopyFiles: []runtime.CopyEntry{{
			Src: canonicalPath, RelDst: filepath.Join(".codex", "hooks.json"), Probed: true,
			ContentHash: runtime.HashPathContent(canonicalPath),
		}},
	}, cityRoot, io.Discard)
	if err != nil {
		t.Fatalf("stageFiles: %v", err)
	}

	if got := ops.files["/workspace/sessions/operator/.codex/hooks.json"]; got != canonical {
		t.Fatalf("owned Codex hook = %q, want canonical %q", got, canonical)
	}
	if _, exists := ops.files["/workspace/.codex/hooks.json"]; exists {
		t.Fatal("owned Codex hook was staged at city root instead of the named session workdir")
	}
	if got := ops.files["/workspace/sessions/operator/AGENTS.codex.md"]; got != "codex sibling" {
		t.Fatalf("ordinary overlay sibling = %q, want staged", got)
	}
	if got := ops.files["/workspace/sessions/operator/.gemini/settings.json"]; !strings.Contains(got, `"BeforeAgent"`) {
		t.Fatalf("unowned mergeable sibling = %q, want staged", got)
	}
}

func TestStageFilesFailsWhenOwnedCodexHookCopyFails(t *testing.T) {
	cityRoot := t.TempDir()
	workDir := filepath.Join(cityRoot, "sessions", "operator")
	hookPath := filepath.Join(workDir, ".codex", "hooks.json")
	if err := os.MkdirAll(filepath.Dir(hookPath), 0o755); err != nil {
		t.Fatalf("mkdir hook dir: %v", err)
	}
	if err := os.WriteFile(hookPath, []byte(`{"hooks":{}}`), 0o644); err != nil {
		t.Fatalf("write hook: %v", err)
	}
	ops := newCapturingStageOps()
	ops.failDestination = "/workspace/sessions/operator/.codex"
	err := stageFiles(context.Background(), ops, "gc-codex", runtime.Config{
		WorkDir:                       workDir,
		ReconcilerOwnedMergeablePaths: []string{filepath.Join(".codex", "hooks.json")},
		CopyFiles: []runtime.CopyEntry{{
			Src: hookPath, RelDst: filepath.Join(".codex", "hooks.json"), Probed: true,
			ContentHash: runtime.HashPathContent(hookPath),
		}},
	}, cityRoot, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "staging reconciler-owned copy_file") {
		t.Fatalf("stageFiles error = %v, want fatal owned CopyFile context", err)
	}
}

func TestStageFilesFailsWhenOwnedSourceChangesDuringCopy(t *testing.T) {
	workDir := t.TempDir()
	hookPath := filepath.Join(workDir, ".codex", "hooks.json")
	if err := os.MkdirAll(filepath.Dir(hookPath), 0o755); err != nil {
		t.Fatalf("mkdir hook dir: %v", err)
	}
	original := []byte(`{"hooks":{}}`)
	if err := os.WriteFile(hookPath, original, 0o644); err != nil {
		t.Fatalf("write hook: %v", err)
	}

	ops := newCapturingStageOps()
	ops.mutateSourceOnMkdir = hookPath
	ops.mutateMkdirDestination = "/workspace/.codex"
	ops.mutateContents = bytes.Repeat([]byte("x"), len(original))
	err := stageFiles(context.Background(), ops, "gc-codex", runtime.Config{
		WorkDir:                       workDir,
		ReconcilerOwnedMergeablePaths: []string{filepath.Join(".codex", "hooks.json")},
		CopyFiles: []runtime.CopyEntry{{
			Src: hookPath, RelDst: filepath.Join(".codex", "hooks.json"), Probed: true,
			ContentHash: runtime.HashPathContent(hookPath),
		}},
	}, workDir, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "pod destination") || !strings.Contains(err.Error(), "digest mismatch") {
		t.Fatalf("stageFiles error = %v, want remote destination digest mismatch", err)
	}
	if !ops.mutatedSource {
		t.Fatal("test did not mutate source after validation and before tar reopen")
	}
	if ops.readySignaled {
		t.Fatal("init container was released after owned destination digest mismatch")
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
	files                  map[string]string
	failDestination        string
	mutateSourceOnMkdir    string
	mutateMkdirDestination string
	mutateContents         []byte
	mutatedSource          bool
	readySignaled          bool
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
	if len(cmd) == 3 && cmd[0] == "mkdir" && cmd[1] == "-p" &&
		cmd[2] == o.mutateMkdirDestination && o.mutateSourceOnMkdir != "" && !o.mutatedSource {
		if err := os.WriteFile(o.mutateSourceOnMkdir, o.mutateContents, 0o644); err != nil {
			return "", err
		}
		o.mutatedSource = true
	}
	if len(cmd) == 5 && cmd[0] == "tar" && cmd[1] == "xf" && cmd[2] == "-" && cmd[3] == "-C" && stdin != nil {
		if o.failDestination != "" && cmd[4] == o.failDestination {
			return "", errors.New("injected copy failure")
		}
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
	}
	if len(cmd) == 5 && cmd[0] == "sh" && cmd[1] == "-c" && cmd[3] == "gc-read-owned" {
		contents, ok := o.files[cmd[4]]
		if !ok {
			return "", fmt.Errorf("missing pod destination %s", cmd[4])
		}
		return contents, nil
	}
	if len(cmd) == 2 && cmd[0] == "touch" && cmd[1] == "/workspace/.gc-ready" {
		o.readySignaled = true
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
