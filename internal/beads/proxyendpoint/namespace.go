package proxyendpoint

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// namespaceQuery reads both namespace and database ownership in one snapshot.
// These are the same config/metadata keys read by Beads1.3 WorkspaceConfig and
// its project identity check. No writable storage open or migration is needed.
const namespaceQuery = "SELECT (SELECT value FROM config WHERE `key` = 'issue_prefix'), (SELECT value FROM metadata WHERE `key` = '_project_id')"

// ReadIssuePrefix reads a running workspace's own proxied database without
// starting a provider, opening a writable SDK store, or retaining a session.
// Beads1.3 strict --readonly deliberately refuses this transport; reuse the
// verified endpoint and single-session machinery used by our read-only probes.
// Ambient BEADS_* variables cannot redirect this filesystem-owned lookup.
func ReadIssuePrefix(parent context.Context, scopeRoot string) (string, error) {
	ctx, cancel := context.WithTimeout(parent, ProbeSessionTimeout)
	defer cancel()
	beadsDir := filepath.Join(scopeRoot, ".beads")
	data, err := os.ReadFile(filepath.Join(beadsDir, MetadataFileName))
	if err != nil {
		return "", err
	}
	var meta struct {
		Backend   string `json:"backend"`
		Mode      string `json:"dolt_mode"`
		Database  string `json:"dolt_database"`
		ProjectID string `json:"project_id"`
		DataDir   string `json:"dolt_data_dir"`
	}
	if err = json.Unmarshal(data, &meta); err != nil {
		return "", fmt.Errorf("read workspace metadata: %w", err)
	}
	if meta.Mode != "proxied-server" || (meta.Backend != "" && meta.Backend != "dolt") {
		return "", fmt.Errorf("namespace inspection requires a proxied Dolt workspace")
	}
	if meta.Database == "" {
		return "", fmt.Errorf("workspace metadata must name dolt_database")
	}
	sidecar, err := ReadSidecar(beadsDir)
	if err != nil {
		return "", err
	}
	root := sidecar.ResolvedRootPath(beadsDir)
	if root == "" {
		root = filepath.Join(beadsDir, DefaultRootDirName)
		if meta.DataDir != "" {
			root = resolveUnderBeadsDir(beadsDir, meta.DataDir)
		}
	}
	ep := Inspect(root, DefaultProcessTable())
	if !ep.Verdict.Live() {
		return "", fmt.Errorf("beads proxy is not ready (%s); run bd ping in the project before previewing adoption", ep.Verdict)
	}
	connector, err := probeConnector(ep.Record.Port, meta.Database, probeDriverTimeout(ProbeSessionTimeout))
	if err != nil {
		return "", err
	}
	prefix, err := readNamespaceOver(ctx, connector, meta.ProjectID)
	if err != nil {
		return "", err
	}
	current, err := Read(root)
	if err != nil || current != ep.Record {
		return "", fmt.Errorf("beads proxy changed during namespace inspection; retry adoption preview")
	}
	return prefix, nil
}

func readNamespaceOver(ctx context.Context, connector driver.Connector, projectID string) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	session, err := connector.Connect(ctx)
	if err != nil {
		return "", err
	}
	one := &oneSessionConnector{session: session, driver: connector.Driver()}
	db := sql.OpenDB(one)
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(0)
	defer db.Close() //nolint:errcheck // no retained session or pool
	defer one.closeUntaken()
	conn, err := db.Conn(ctx)
	if err != nil {
		return "", err
	}
	defer conn.Close() //nolint:errcheck // no idle slot, closes the session
	var prefix, actualID sql.NullString
	if err = conn.QueryRowContext(ctx, namespaceQuery).Scan(&prefix, &actualID); err != nil {
		return "", fmt.Errorf("read Beads namespace: %w", err)
	}
	// Match Beads' legacy-identity policy: compare IDs when both exist.
	if projectID != "" && actualID.Valid && actualID.String != "" && actualID.String != projectID {
		return "", fmt.Errorf("beads project identity mismatch; refusing to adopt another project's namespace")
	}
	if !prefix.Valid || strings.TrimSpace(prefix.String) == "" {
		return "", fmt.Errorf("existing Beads database has no issue_prefix")
	}
	return prefix.String, nil
}
