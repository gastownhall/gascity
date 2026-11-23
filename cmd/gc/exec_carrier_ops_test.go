package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/doctor"
	"github.com/gastownhall/gascity/internal/runtime"
	sessionexec "github.com/gastownhall/gascity/internal/runtime/exec"
	"github.com/gastownhall/gascity/internal/runtime/runtimecontract/reference"
)

func TestExecFencingPredicateLockstep(t *testing.T) {
	for mask := 0; mask < 8; mask++ {
		var caps []string
		for i, c := range []string{"proc.exec", "input-fence", "carrier.ops"} {
			if mask&(1<<i) != 0 {
				caps = append(caps, c)
			}
		}
		b, err := json.Marshal(runtime.ProtocolInfo{Capabilities: caps})
		if err != nil {
			t.Fatal(err)
		}
		p := sessionexec.NewProvider(writeHandshakePack(t, string(b)))
		info, err := p.Protocol()
		if err != nil {
			t.Fatal(err)
		}
		want := mask&3 == 3 && mask&4 == 0
		if got := (execHandshake{info: info}).fencesInput(); got != want {
			t.Errorf("%v cmd/gc fences=%v want %v", caps, got, want)
		}
		if got := p.InputFencing("s") == runtime.InputFencingAtomic; got != want {
			t.Errorf("%v exec fences=%v want %v", caps, got, want)
		}
	}
}

func TestExecInputFenceWarningUnderCarrierOps(t *testing.T) {
	script := writeHandshakePack(t, `{"version":0,"capabilities":["observe.v1","report-attachment","proc.exec","input-fence","carrier.ops"]}`)
	cfg := v2DefaultCity(func(c *config.City) { c.Session.Provider = "exec:" + script; c.Daemon.SessionReconciler = "v2" })
	_, warn, hint := execRuntimeDoctorDetail(cfg, "", nil, reconcilerV2)
	if !strings.Contains(warn, "carrier.ops") || !strings.Contains(warn, "refused") {
		t.Errorf("warning = %q", warn)
	}
	if !strings.Contains(hint, "carrier.ops runtime takes no fenced input") {
		t.Errorf("hint = %q, want the one read with the warning", hint)
	}
	r := newSessionReconcilerDoctorCheck(cfg, nil).Run(&doctor.CheckContext{})
	if !strings.Contains(r.FixHint, "carrier.ops runtime takes no fenced input") || strings.Contains(r.FixHint, "declare proc.exec") {
		t.Errorf("hint = %q", r.FixHint)
	}
}

func TestRuntimeConformanceCmd_NonTmuxCarrierPasses(t *testing.T) {
	state := t.TempDir()
	script := filepath.Join(t.TempDir(), "golden")
	if err := os.WriteFile(script, []byte(reference.Script(state, reference.NonTmuxBodies())), 0o755); err != nil {
		t.Fatal(err)
	}
	var out, stderr bytes.Buffer
	cmd := newRuntimeConformanceCmd(&out, &stderr)
	cmd.SetArgs([]string{script})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("Execute: %v\n%s\n%s", err, &out, &stderr)
	}
	if !strings.Contains(out.String(), "PASS RPP-CONN-002") || !strings.Contains(out.String(), "0 failed") {
		t.Errorf("output = %s", &out)
	}
	if _, err := os.Stat(filepath.Join(state, "tmux-rejected")); !os.IsNotExist(err) {
		t.Error("conformance sent tmux")
	}
}
