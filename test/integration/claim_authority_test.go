//go:build integration && unix

package integration

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"

	_ "github.com/go-sql-driver/mysql"
)

// TestClaimAuthorityRefusesRawRoutedClaim is the AC-1 contract of issue
// #4437: on an armed Dolt sql-server bead store, a raw bd client claiming
// `gc.routed_to` work as an identity with no live session — the incident's
// mac-Mac-4026 shape — is refused by the claim-authority triggers with the
// SQLSTATE 45000 marker, and the bead is left untouched.
//
// Phase 0 reproduces the incident on the same store before arming: the raw
// claim simply succeeds, which is exactly the client-side-guard bypass the
// boundary exists to close. Phase 1 arms and re-runs the identical claim.
//
// Requires: Dolt and bd binaries configured via PATH or the integration
// override env vars.
func TestClaimAuthorityRefusesRawRoutedClaim(t *testing.T) {
	requireDoltIntegration(t)
	env := newIsolatedToolEnv(t, true)

	rootDir := t.TempDir()
	port := startSharedDoltServer(t, env, filepath.Join(rootDir, "dolt"))

	wsDir := filepath.Join(rootDir, "ws")
	if err := os.MkdirAll(wsDir, 0o755); err != nil {
		t.Fatalf("creating workspace: %v", err)
	}
	gitInit := exec.Command("git", "init", "--quiet")
	gitInit.Dir = wsDir
	if out, err := gitInit.CombinedOutput(); err != nil {
		t.Fatalf("git init: %v\n%s", err, out)
	}
	runBDInit(t, env, wsDir, "ca", port)
	bdEnv := isolateBdHomeEnv(env)

	dbName := doltDatabaseName(t, wsDir)
	db, err := sql.Open("mysql", fmt.Sprintf("root@tcp(127.0.0.1:%s)/%s", port, dbName))
	if err != nil {
		t.Fatalf("opening control connection: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	if err := waitForDB(ctx, db); err != nil {
		t.Fatalf("control connection never became ready: %v", err)
	}
	capability, err := beads.ProbeClaimAuthorityCapability(ctx, db)
	if err != nil {
		t.Fatalf("probing claim-authority capability: %v", err)
	}
	if !capability.CanHost() {
		t.Fatalf("bd store cannot host the boundary: %s", capability.Reason())
	}
	// Arming stages and commits dolt_schemas on one session, so it needs a
	// pinned connection rather than the pool.
	conn, err := db.Conn(ctx)
	if err != nil {
		t.Fatalf("pinning arm connection: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })

	// A routed work bead: gc.routed_to names the rig/pool that owns it.
	issueID := createRoutedIssue(t, bdEnv, wsDir, "ca-1", `{"gc.routed_to":"rig-alpha/pool-a"}`)

	// Phase 0 — the incident: before arming, the raw claim succeeds.
	if out, err := bdUpdateClaim(t, bdEnv, wsDir, issueID, "mac-Mac-4026"); err != nil {
		t.Fatalf("pre-arm raw claim must succeed (the unguarded behavior under fix): %v\n%s", err, out)
	}
	assignee, status := issueAssigneeStatus(t, db, issueID)
	if assignee != "mac-Mac-4026" || status != "in_progress" {
		t.Fatalf("pre-arm claim did not take: assignee=%q status=%q", assignee, status)
	}
	// Hand the bead back so the armed phases start from the same state.
	if _, err := db.ExecContext(ctx, "UPDATE issues SET assignee = NULL, status = 'open' WHERE id = ?", issueID); err != nil {
		t.Fatalf("releasing pre-arm claim: %v", err)
	}

	// Phase 1 — arm the boundary.
	changed, err := beads.ArmClaimAuthority(ctx, conn, capability.Tables)
	if err != nil {
		t.Fatalf("arming: %v", err)
	}
	if !changed {
		t.Fatal("first arm must install the triggers")
	}
	if changed, err := beads.ArmClaimAuthority(ctx, conn, capability.Tables); err != nil || changed {
		t.Fatalf("second arm must be a no-op: changed=%v err=%v", changed, err)
	}

	// Phase 2 — the raw claim is refused, through bd and raw SQL alike,
	// each refusal carrying the marker.
	out, err := bdUpdateClaim(t, bdEnv, wsDir, issueID, "mac-Mac-4026")
	if err == nil {
		t.Fatalf("raw bd claim after arming must fail, got success:\n%s", out)
	}
	if !strings.Contains(string(out), beads.ClaimAuthorityMarker) {
		t.Fatalf("bd claim refusal lacks the marker %q:\n%s", beads.ClaimAuthorityMarker, out)
	}
	out, err = bdRun(t, bdEnv, wsDir, "update", issueID, "--assignee", "mac-Mac-4026")
	if err == nil {
		t.Fatalf("raw bd --assignee after arming must fail, got success:\n%s", out)
	}
	if !strings.Contains(string(out), beads.ClaimAuthorityMarker) {
		t.Fatalf("bd --assignee refusal lacks the marker:\n%s", out)
	}
	if _, err := db.ExecContext(ctx, "UPDATE issues SET assignee = 'mac-Mac-4026', status = 'in_progress' WHERE id = ?", issueID); err == nil {
		t.Fatal("raw SQL claim after arming must fail, got success")
	} else if !strings.Contains(err.Error(), beads.ClaimAuthorityMarker) {
		t.Fatalf("raw SQL refusal lacks the marker: %v", err)
	}

	// Phase 3 — the bead is untouched: still open, still unassigned.
	assignee, status = issueAssigneeStatus(t, db, issueID)
	if assignee != "" || status != "open" {
		t.Fatalf("refused claims must leave the bead open and unassigned, got assignee=%q status=%q", assignee, status)
	}

	// Phase 4 — an identity the controller published is authorized: the
	// identical raw claim shape succeeds, proving the trigger refuses
	// unauthorized claimants rather than the write shape itself.
	pub := beads.ClaimAuthorityPublisher{
		Tag:  beads.ClaimAuthorityCityTag(rootDir, "claim-authority-test"),
		City: "claim-authority-test",
		Mode: "auto",
	}
	if _, err := beads.PublishClaimAuthority(ctx, db, pub, []beads.ClaimAuthorityEntry{
		{Identity: "gc-session-live", Source: beads.ClaimAuthoritySession},
	}, time.Now().UTC(), true); err != nil {
		t.Fatalf("publishing authority row: %v", err)
	}
	if out, err := bdUpdateClaim(t, bdEnv, wsDir, issueID, "gc-session-live"); err != nil {
		t.Fatalf("authorized claim must succeed: %v\n%s", err, out)
	}
	assignee, status = issueAssigneeStatus(t, db, issueID)
	if assignee != "gc-session-live" || status != "in_progress" {
		t.Fatalf("authorized claim did not take: assignee=%q status=%q", assignee, status)
	}
}

// bdUpdateClaim runs the incident's raw claim: BEADS_ACTOR names the
// claiming identity, exactly as a co-tenant bd client would.
func bdUpdateClaim(t *testing.T, env []string, dir, issueID, actor string) ([]byte, error) {
	t.Helper()
	claimEnv := append(append([]string(nil), env...), "BEADS_ACTOR="+actor)
	cmd := exec.Command(bdBinary, "update", issueID, "--claim")
	cmd.Dir = dir
	cmd.Env = claimEnv
	return cmd.CombinedOutput()
}

// bdRun runs one bd command in dir under env, returning combined output.
func bdRun(t *testing.T, env []string, dir string, args ...string) ([]byte, error) {
	t.Helper()
	cmd := exec.Command(bdBinary, args...)
	cmd.Dir = dir
	cmd.Env = env
	return cmd.CombinedOutput()
}

// createRoutedIssue creates one issue carrying metadata JSON and returns
// its ID.
func createRoutedIssue(t *testing.T, env []string, dir, prefix, metadata string) string {
	t.Helper()
	out, err := bdRun(t, env, dir, "create", prefix+" routed work", "--json", "-t", "task", "--metadata", metadata)
	if err != nil {
		t.Fatalf("bd create: %v\n%s", err, out)
	}
	payload := extractJSONPayload(string(out))
	var one struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal([]byte(payload), &one); err == nil && one.ID != "" {
		return one.ID
	}
	var many []struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal([]byte(payload), &many); err != nil || len(many) == 0 || many[0].ID == "" {
		t.Fatalf("bd create --json output carried no issue id:\n%s", out)
	}
	return many[0].ID
}

// issueAssigneeStatus reads the bead's assignee and status over the control
// connection.
func issueAssigneeStatus(t *testing.T, db *sql.DB, issueID string) (string, string) {
	t.Helper()
	var assignee, status sql.NullString
	if err := db.QueryRow("SELECT assignee, status FROM issues WHERE id = ?", issueID).Scan(&assignee, &status); err != nil {
		t.Fatalf("reading issue %s: %v", issueID, err)
	}
	return assignee.String, status.String
}

// waitForDB pings until the server answers or ctx ends.
func waitForDB(ctx context.Context, db *sql.DB) error {
	deadline := time.Now().Add(30 * time.Second)
	for {
		err := db.PingContext(ctx)
		if err == nil {
			return nil
		}
		if time.Now().After(deadline) {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(500 * time.Millisecond):
		}
	}
}
