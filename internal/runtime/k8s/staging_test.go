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
	"testing"

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
	if len(cmd) == 5 && cmd[0] == "tar" && cmd[1] == "xf" && cmd[2] == "-" && cmd[3] == "-C" && stdin != nil {
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
	return "", nil
}
