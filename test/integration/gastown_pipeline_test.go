//go:build integration

package integration

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestGastown_PipelineHumanToWorker validates the full pipeline:
// human creates work → agent processes → bead closes.
func TestGastown_PipelineHumanToWorker(t *testing.T) {
	agents := []gasTownAgent{
		{Name: "worker", StartCommand: "bash " + agentScript("one-shot.sh")},
	}
	cityDir := setupGasTownCityNoGuard(t, agents)

	// Human creates work and assigns to worker.
	beadID := createBead(t, cityDir, "Build login page")
	claimBead(t, cityDir, "worker", beadID)

	// Wait for worker to process.
	waitForBeadStatus(t, cityDir, beadID, "closed", 10*time.Second)

	// Verify events were recorded.
	verifyEvents(t, cityDir, "bead.created")
	verifyEvents(t, cityDir, "bead.closed")
}

// TestGastown_PipelineMailAndWork validates the pipeline:
// send mail to agent → agent reads mail → agent creates work → work processed.
func TestGastown_PipelineMailAndWork(t *testing.T) {
	agents := []gasTownAgent{
		{Name: "mayor", StartCommand: "bash " + agentScript("mayor-dispatch.sh")},
	}
	cityDir := setupGasTownCityNoGuard(t, agents)

	// Human sends dispatch request to mayor.
	sendMail(t, cityDir, "mayor", "Fix authentication bug")

	// Wait for mayor to read mail and create work bead.
	deadline := time.Now().Add(10 * time.Second)
	var created bool
	for time.Now().Before(deadline) {
		out, _ := bd(cityDir, "list")
		// Look for the bead created by the mayor.
		if strings.Contains(out, "Fix authentication bug") {
			created = true
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	if !created {
		out, _ := bd(cityDir, "list")
		t.Fatalf("timed out waiting for mayor to create work bead\nbead list:\n%s", out)
	}
}

// TestGastown_PipelinePoolDrain validates the full pipeline with pool:
// create multiple beads → pool agent drains them all.
func TestGastown_PipelinePoolDrain(t *testing.T) {
	agents := []gasTownAgent{
		{Name: "polecat", StartCommand: "bash " + agentScript("loop.sh"), Pool: &poolConfig{
			Min: 1, Max: 5, Check: "echo 1",
		}},
	}
	cityDir := setupGasTownCityNoGuard(t, agents)

	// Create 5 beads for the pool to drain.
	var beadIDs []string
	for i := 0; i < 5; i++ {
		id := createBead(t, cityDir, "Pool work item")
		beadIDs = append(beadIDs, id)
	}

	// Wait for all beads to close.
	for _, id := range beadIDs {
		waitForBeadStatus(t, cityDir, id, "closed", 15*time.Second)
	}

	// Verify all events recorded.
	verifyEvents(t, cityDir, "bead.created")
	verifyEvents(t, cityDir, "bead.closed")
}

// TestGastown_PipelineConvoyTracking validates convoy tracking over a pipeline:
// create convoy → create beads → process beads → convoy auto-closes.
func TestGastown_PipelineConvoyTracking(t *testing.T) {
	agents := []gasTownAgent{
		{Name: "worker", StartCommand: "bash " + agentScript("loop.sh")},
	}
	cityDir := setupGasTownCityNoGuard(t, agents)

	// Create work beads.
	bead1 := createBead(t, cityDir, "Task A")
	bead2 := createBead(t, cityDir, "Task B")

	// Create convoy tracking both beads.
	out, err := gc(cityDir, "convoy", "create", "Sprint 1", bead1, bead2)
	if err != nil {
		t.Fatalf("gc convoy create failed: %v\noutput: %s", err, out)
	}

	// Verify convoy shows progress against the two tracked issues. The worker
	// may pick up one bead immediately, so the initial snapshot can already
	// be at 1/2 instead of 0/2.
	convoyID := extractBeadID(t, out)
	out, err = gc(cityDir, "convoy", "status", convoyID)
	if err != nil {
		t.Fatalf("gc convoy status failed: %v\noutput: %s", err, out)
	}
	if !strings.Contains(out, "/2 closed") {
		t.Errorf("expected convoy progress for two issues:\n%s", out)
	}

	// Wait for worker to close both beads.
	waitForBeadStatus(t, cityDir, bead1, "closed", 15*time.Second)
	waitForBeadStatus(t, cityDir, bead2, "closed", 15*time.Second)

	// Auto-close the convoy.
	out, err = gc(cityDir, "convoy", "check")
	if err != nil {
		t.Fatalf("gc convoy check failed: %v\noutput: %s", err, out)
	}
	if !strings.Contains(out, "auto-closed") {
		// Already auto-closed or 0 auto-closed is fine.
		t.Logf("convoy check output: %s", out)
	}
}

// TestGastown_PipelineMailChain validates a mail chain between agents.
func TestGastown_PipelineMailChain(t *testing.T) {
	agents := []gasTownAgent{
		{Name: "mayor", StartCommand: "bash " + agentScript("loop-mail.sh")},
		{Name: "deacon", StartCommand: "sleep 3600"},
	}
	cityDir := setupGasTownCityNoGuard(t, agents)

	// Human sends to mayor.
	sendMail(t, cityDir, "mayor", "Status report please")

	// Wait for mayor to reply.
	waitForMail(t, cityDir, "human", "ack from mayor", 10*time.Second)

	// Verify events show the mail flow.
	verifyEvents(t, cityDir, "mail.sent")
	verifyEvents(t, cityDir, "mail.read")
}

// TestGastown_PipelineGitCommitMerge validates the full git pipeline:
// create work bead → polecat branches/commits/pushes → hands off to refinery →
// refinery fetches/merges to main/pushes → closes bead → verify commit on main.
func TestGastown_PipelineGitCommitMerge(t *testing.T) {
	// Set up git infrastructure: bare repo + two independent working copies.
	bareRepo := setupBareGitRepo(t)
	polecatRepo := setupWorkingRepo(t, bareRepo)
	refineryRepo := setupWorkingRepo(t, bareRepo)

	agents := []gasTownAgent{
		{
			Name:         "polecat",
			StartCommand: "bash " + agentScript("polecat-git.sh"),
			Env:          map[string]string{"GIT_WORK_DIR": polecatRepo, "GC_HANDOFF_TO": "refinery"},
		},
		{
			Name:         "refinery",
			StartCommand: "bash " + agentScript("refinery-git.sh"),
			Env:          map[string]string{"GIT_WORK_DIR": refineryRepo},
		},
	}
	cityDir := setupGasTownCityNoGuard(t, agents)

	// Create work and assign to polecat.
	beadID := createBead(t, cityDir, "Implement feature X")
	out, err := bd(cityDir, "update", beadID, "--assignee=polecat")
	if err != nil {
		t.Fatalf("bd update --assignee=polecat failed: %v\noutput: %s", err, out)
	}

	// Wait for the full pipeline: polecat commits → hands off → refinery merges → closes.
	waitForBeadStatus(t, cityDir, beadID, "closed", 20*time.Second)

	// Verify: fresh clone from bare repo should have the fix on main.
	verifyDir := filepath.Join(t.TempDir(), "verify")
	runGitCmd(t, "", "git", "clone", bareRepo, verifyDir)

	fixFile := filepath.Join(verifyDir, "fix-"+beadID+".txt")
	data, err := os.ReadFile(fixFile)
	if err != nil {
		// Dump git log for debugging.
		cmd := exec.Command("git", "log", "--oneline", "-10")
		cmd.Dir = verifyDir
		logOut, _ := cmd.CombinedOutput()
		t.Fatalf("fix file not found on main: %v\ngit log:\n%s", err, logOut)
	}

	content := strings.TrimSpace(string(data))
	want := "fix for " + beadID
	if content != want {
		t.Errorf("fix file content = %q, want %q", content, want)
	}
}
