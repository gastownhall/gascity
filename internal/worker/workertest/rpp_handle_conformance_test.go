package workertest

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/gastownhall/gascity/internal/runtime"
	sessionexec "github.com/gastownhall/gascity/internal/runtime/exec"
	"github.com/gastownhall/gascity/internal/runtime/runtimecontract/reference"
	worker "github.com/gastownhall/gascity/internal/worker"
)

// rppHandleProfile labels the WC-RPP results: the non-certifying
// alternate-transport proof profile, the golden reference pack driven
// through the real exec provider.
const rppHandleProfile ProfileID = "reference/rpp-ops"

// rppSessionName is the session every WC-RPP check starts on the golden
// reference.
const rppSessionName = "rpp-worker"

// TestRPPHandleConformance emits the WC-RPP rows through the SuiteReporter
// artifact path, driving a RuntimeHandle over exec.NewSeamBackedForCity.
func TestRPPHandleConformance(t *testing.T) {
	reporter := NewSuiteReporter(t, "rpp-handle", map[string]string{
		"tier": "worker-core",
	})
	reporter.Require(t, vanishedNudgeNotDeliveredResult(t))
}

func TestRPPHandleCatalogRegistersVanishedNudgeRequirement(t *testing.T) {
	catalog := RPPHandleCatalog()
	if len(catalog) != 1 || catalog[0].Code != RequirementRPPVanishedNudgeNotDelivered {
		t.Fatalf("RPPHandleCatalog() = %+v, want only %s", catalog, RequirementRPPVanishedNudgeNotDelivered)
	}
	if catalog[0].Group == "" || catalog[0].Description == "" {
		t.Fatalf("requirement %s has empty group or description", catalog[0].Code)
	}
}

// rppGoldenPack writes the golden reference with two changes: `exec` runs
// nothing (no check drives a host tmux), and `observe` removes the box right
// after answering while $S/vanish exists, so the session vanishes between the
// handle's liveness read and its delivery. It returns the script and the
// vanish flag.
func rppGoldenPack(t *testing.T) (script, vanish string) {
	t.Helper()
	dir := t.TempDir()
	stateDir := filepath.Join(dir, "state")
	if err := os.Mkdir(stateDir, 0o755); err != nil {
		t.Fatal(err)
	}
	bodies := reference.Bodies()
	bodies["exec"] = `cat >/dev/null`
	bodies["observe"] += `; if [ -f "$S/vanish" ]; then rm -f "$S/vanish"; rm -rf "$box"; fi`
	script = filepath.Join(dir, "golden")
	if err := os.WriteFile(script, []byte(reference.Script(stateDir, bodies)), 0o755); err != nil {
		t.Fatal(err)
	}
	return script, filepath.Join(stateDir, "vanish")
}

// rppNudge starts a session on a fresh golden pack and nudges it through a
// RuntimeHandle; vanish makes the session disappear after the handle's
// liveness read.
func rppNudge(t *testing.T, vanish bool) (worker.NudgeResult, error) {
	t.Helper()
	script, flag := rppGoldenPack(t)
	sp := sessionexec.NewSeamBackedForCity(script, "rpp-conformance", t.TempDir())
	cfg := runtime.Config{WorkDir: t.TempDir(), Command: "sleep 300", ProcessNames: []string{"sleep"}, AcceptStartupDialogs: new(bool)}
	if err := sp.Start(context.Background(), rppSessionName, cfg); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { _ = sp.Stop(rppSessionName) })
	handle, err := worker.NewRuntimeHandle(worker.RuntimeHandleConfig{
		Provider:     sp,
		SessionName:  rppSessionName,
		ProviderName: "rpp-reference",
		ProcessNames: cfg.ProcessNames,
	})
	if err != nil {
		t.Fatalf("NewRuntimeHandle: %v", err)
	}
	if vanish {
		if err := os.WriteFile(flag, nil, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return handle.Nudge(context.Background(), testAgent(t), worker.NudgeRequest{
		Text:     "check deploy status",
		Delivery: worker.NudgeDeliveryImmediate,
		Source:   "mail",
	})
}

// vanishedNudgeNotDeliveredResult evaluates WC-RPP-001: a live session takes
// the nudge, and one that vanished after the liveness read refuses it with
// ErrSessionNotFound. Setup failures fail the test loudly; only a contract
// violation is a failing result.
func vanishedNudgeNotDeliveredResult(t *testing.T) Result {
	t.Helper()
	if result, err := rppNudge(t, false); err != nil || !result.Delivered {
		return Fail(rppHandleProfile, RequirementRPPVanishedNudgeNotDelivered,
			fmt.Sprintf("nudge to a live session = (delivered=%t, %v), want delivered", result.Delivered, err))
	}
	result, err := rppNudge(t, true)
	if result.Delivered || !errors.Is(err, runtime.ErrSessionNotFound) {
		return Fail(rppHandleProfile, RequirementRPPVanishedNudgeNotDelivered,
			fmt.Sprintf("nudge to a vanished session = (delivered=%t, %v), want undelivered with runtime.ErrSessionNotFound", result.Delivered, err))
	}
	return Pass(rppHandleProfile, RequirementRPPVanishedNudgeNotDelivered,
		"a live session takes the nudge; one that vanished after the liveness read refuses it with runtime.ErrSessionNotFound")
}
