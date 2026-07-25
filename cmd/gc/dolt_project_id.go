package main

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	gcapi "github.com/gastownhall/gascity/internal/api"
	"github.com/gastownhall/gascity/internal/beads/contract"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/fsys"
	mysql "github.com/go-sql-driver/mysql"
	"github.com/spf13/cobra"
)

type managedDoltProjectIDReport struct {
	ProjectID           string
	MetadataUpdated     bool
	DatabaseUpdated     bool
	IdentityFileUpdated bool
	Source              string
	Layer               string
}

var (
	projectIdentityDisplayPath  = filepath.ToSlash(contract.ProjectIdentityPath(""))
	projectIdentityProjectIDRef = projectIdentityDisplayPath + "#project.id"
)

type reconcileAction int

const (
	actionNoOp reconcileAction = iota
	actionRefuseL1L3Mismatch
	actionRepairL2
	actionSeedL3
	actionRepairL2SeedL3
	actionSeedL2
	actionSeedL2L3
	actionMigrateFromL2
	actionRefuseLegacyMismatch
	actionMigrateL1SeedL3
	actionAdoptFromL3SeedL2
	actionGenerate
)

type reconcileDecision struct {
	Action     reconcileAction
	ResolvedID string
	L1ID       string
	L2ID       string
	L3ID       string
	Source     string
	Layer      string
	WriteL1    bool
	WriteL2    bool
	WriteL3    bool
}

func newEnsureProjectIDCmd(stdout, stderr io.Writer) *cobra.Command {
	var (
		metadataPath string
		host         string
		port         string
		user         string
		database     string
		cityPath     string
	)
	cmd := &cobra.Command{
		Use:    "ensure-project-id",
		Short:  "Ensure local metadata and the Dolt metadata table share a project identity",
		Hidden: true,
		Args:   cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			rec, closeRecorder := openProjectIdentityEventRecorder(cityPath, stderr)
			defer closeRecorder()
			report, err := ensureManagedDoltProjectIDWithRecorder(metadataPath, host, port, user, database, cityPath, rec)
			if err != nil {
				fmt.Fprintf(stderr, "gc dolt-state ensure-project-id: %v\n", err) //nolint:errcheck
				return errExit
			}
			for _, line := range managedDoltProjectIDFields(report) {
				if _, writeErr := fmt.Fprintln(stdout, line); writeErr != nil {
					fmt.Fprintf(stderr, "gc dolt-state ensure-project-id: %v\n", writeErr) //nolint:errcheck
					return errExit
				}
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&metadataPath, "metadata", "", "path to .beads/metadata.json")
	cmd.Flags().StringVar(&host, "host", "", "Dolt host")
	cmd.Flags().StringVar(&port, "port", "", "Dolt port")
	cmd.Flags().StringVar(&user, "user", "", "Dolt user")
	cmd.Flags().StringVar(&database, "database", "", "Dolt database")
	cmd.Flags().StringVar(&cityPath, "city", "", "city root (required for event emission)")
	_ = cmd.MarkFlagRequired("city")
	_ = cmd.MarkFlagRequired("metadata")
	_ = cmd.MarkFlagRequired("port")
	_ = cmd.MarkFlagRequired("database")
	return cmd
}

func openProjectIdentityEventRecorder(cityPath string, stderr io.Writer) (events.Recorder, func()) {
	rec, err := events.NewFileRecorder(filepath.Join(cityPath, ".gc", "events.jsonl"), io.Discard)
	if err != nil {
		fmt.Fprintf(stderr, "gc dolt-state ensure-project-id: events recorder unavailable: %v\n", err) //nolint:errcheck
		return events.Discard, func() {}
	}
	return rec, func() {
		_ = rec.Close()
	}
}

func ensureManagedDoltProjectIDWithRecorder(metadataPath, host, port, user, database string, cityPath string, rec events.Recorder) (managedDoltProjectIDReport, error) {
	metadataPath = strings.TrimSpace(metadataPath)
	if metadataPath == "" {
		return managedDoltProjectIDReport{}, fmt.Errorf("missing metadata path")
	}
	scopeRoot, err := scopeRootFromMetadataPath(metadataPath)
	if err != nil {
		return managedDoltProjectIDReport{}, err
	}
	database = strings.TrimSpace(database)
	if database == "" {
		return managedDoltProjectIDReport{}, fmt.Errorf("missing database")
	}

	fs := fsys.OSFS{}
	identityProjectID, identityOK, err := contract.ReadProjectIdentity(fs, scopeRoot)
	if err != nil {
		return managedDoltProjectIDReport{}, err
	}

	metadataProjectID, err := readManagedMetadataProjectID(metadataPath)
	if err != nil {
		return managedDoltProjectIDReport{}, err
	}
	metadataOK := metadataProjectID != ""

	db, err := managedDoltOpenDatabase(host, port, user, database)
	if err != nil {
		return managedDoltProjectIDReport{}, err
	}
	defer db.Close() //nolint:errcheck

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := db.PingContext(ctx); err != nil {
		return managedDoltProjectIDReport{}, err
	}

	databaseProjectID, ok, err := readDatabaseProjectID(ctx, db)
	if err != nil {
		return managedDoltProjectIDReport{}, err
	}

	decision := decideReconcile(identityProjectID, identityOK, metadataProjectID, metadataOK, databaseProjectID, ok)
	seedL3 := func(ctx context.Context, projectID string) (bool, error) {
		return seedDatabaseProjectID(ctx, db, projectID)
	}
	return applyReconcileDecision(ctx, fs, scopeRoot, metadataPath, decision, cityPath, rec, seedL3)
}

func managedDoltProjectIDFields(report managedDoltProjectIDReport) []string {
	return []string{
		"project_id\t" + report.ProjectID,
		"metadata_updated\t" + strconv.FormatBool(report.MetadataUpdated),
		"database_updated\t" + strconv.FormatBool(report.DatabaseUpdated),
		"source\t" + report.Source,
		"identity_file_updated\t" + strconv.FormatBool(report.IdentityFileUpdated),
		"layer\t" + report.Layer,
	}
}

func scopeRootFromMetadataPath(metadataPath string) (string, error) {
	cleaned := filepath.Clean(strings.TrimSpace(metadataPath))
	if filepath.Base(cleaned) != "metadata.json" || filepath.Base(filepath.Dir(cleaned)) != ".beads" {
		return "", fmt.Errorf("metadata path %q is not <scope>/.beads/metadata.json", metadataPath)
	}
	return filepath.Dir(filepath.Dir(cleaned)), nil
}

func decideReconcile(l1 string, l1ok bool, l2 string, l2ok bool, l3 string, l3ok bool) reconcileDecision {
	if l1ok {
		switch {
		case l2ok && l3ok:
			switch {
			case l1 == l2 && l2 == l3:
				return reconcileDecision{Action: actionNoOp, ResolvedID: l1, Source: "match", Layer: "l1"}
			case l1 == l2 && l1 != l3:
				return reconcileDecision{Action: actionRefuseL1L3Mismatch, L1ID: l1, L2ID: l2, L3ID: l3}
			case l1 != l2 && l1 == l3:
				return reconcileDecision{Action: actionRepairL2, ResolvedID: l1, L1ID: l1, L2ID: l2, L3ID: l3, Source: "l2-repair", Layer: "l1", WriteL2: true}
			default:
				return reconcileDecision{Action: actionRefuseL1L3Mismatch, L1ID: l1, L2ID: l2, L3ID: l3}
			}
		case l2ok && !l3ok:
			if l1 == l2 {
				return reconcileDecision{Action: actionSeedL3, ResolvedID: l1, L1ID: l1, L2ID: l2, Source: "l3-seed", Layer: "l1", WriteL3: true}
			}
			return reconcileDecision{Action: actionRepairL2SeedL3, ResolvedID: l1, L1ID: l1, L2ID: l2, Source: "l2-repair-l3-seed", Layer: "l1", WriteL2: true, WriteL3: true}
		case !l2ok && l3ok:
			if l1 == l3 {
				return reconcileDecision{Action: actionSeedL2, ResolvedID: l1, L1ID: l1, L3ID: l3, Source: "l2-seed", Layer: "l1", WriteL2: true}
			}
			return reconcileDecision{Action: actionRefuseL1L3Mismatch, L1ID: l1, L3ID: l3}
		default:
			return reconcileDecision{Action: actionSeedL2L3, ResolvedID: l1, L1ID: l1, Source: "l2-l3-seed", Layer: "l1", WriteL2: true, WriteL3: true}
		}
	}

	switch {
	case l2ok && l3ok:
		if l2 == l3 {
			return reconcileDecision{Action: actionMigrateFromL2, ResolvedID: l2, L2ID: l2, L3ID: l3, Source: "l1-migrate-from-l2", Layer: "l2", WriteL1: true}
		}
		return reconcileDecision{Action: actionRefuseLegacyMismatch, L2ID: l2, L3ID: l3}
	case l2ok && !l3ok:
		return reconcileDecision{Action: actionMigrateL1SeedL3, ResolvedID: l2, L2ID: l2, Source: "l1-migrate-l3-seed", Layer: "l2", WriteL1: true, WriteL3: true}
	case !l2ok && l3ok:
		return reconcileDecision{Action: actionAdoptFromL3SeedL2, ResolvedID: l3, L3ID: l3, Source: "l1-adopt-l2-seed", Layer: "l3", WriteL1: true, WriteL2: true}
	default:
		return reconcileDecision{Action: actionGenerate, Source: "generated", Layer: "generated", WriteL1: true, WriteL2: true, WriteL3: true}
	}
}

func applyReconcileDecision(ctx context.Context, fs fsys.FS, scopeRoot string, metadataPath string, decision reconcileDecision, cityPath string, rec events.Recorder, seedL3 func(context.Context, string) (bool, error)) (managedDoltProjectIDReport, error) {
	report := managedDoltProjectIDReport{
		ProjectID: decision.ResolvedID,
		Source:    decision.Source,
		Layer:     decision.Layer,
	}

	switch decision.Action {
	case actionNoOp:
		return report, nil
	case actionRefuseL1L3Mismatch:
		return managedDoltProjectIDReport{}, formatL1L3MismatchError(decision.L1ID, decision.L3ID)
	case actionRefuseLegacyMismatch:
		return managedDoltProjectIDReport{}, formatLegacyL2L3MismatchError(decision.L2ID, decision.L3ID)
	case actionRepairL2:
		updated, err := writeManagedMetadataProjectID(metadataPath, decision.ResolvedID)
		if err != nil {
			return managedDoltProjectIDReport{}, err
		}
		report.MetadataUpdated = updated
		if updated {
			emitProjectIdentityStampedEvent(rec, cityPath, scopeRoot, "cache_repair", "L2", decision.L2ID, decision.ResolvedID)
		}
		return report, nil
	case actionSeedL3:
		updated, err := seedL3(ctx, decision.ResolvedID)
		if err != nil {
			return managedDoltProjectIDReport{}, err
		}
		report.DatabaseUpdated = updated
		if updated {
			emitProjectIdentityStampedEvent(rec, cityPath, scopeRoot, "cache_repair", "L3", "", decision.ResolvedID)
		}
		return report, nil
	case actionRepairL2SeedL3:
		metaUpdated, err := writeManagedMetadataProjectID(metadataPath, decision.ResolvedID)
		if err != nil {
			return managedDoltProjectIDReport{}, err
		}
		if metaUpdated {
			emitProjectIdentityStampedEvent(rec, cityPath, scopeRoot, "cache_repair", "L2", decision.L2ID, decision.ResolvedID)
		}
		dbUpdated, err := seedL3(ctx, decision.ResolvedID)
		if err != nil {
			return managedDoltProjectIDReport{}, err
		}
		if dbUpdated {
			emitProjectIdentityStampedEvent(rec, cityPath, scopeRoot, "cache_repair", "L3", "", decision.ResolvedID)
		}
		report.MetadataUpdated = metaUpdated
		report.DatabaseUpdated = dbUpdated
		return report, nil
	case actionSeedL2:
		updated, err := writeManagedMetadataProjectID(metadataPath, decision.ResolvedID)
		if err != nil {
			return managedDoltProjectIDReport{}, err
		}
		report.MetadataUpdated = updated
		if updated {
			emitProjectIdentityStampedEvent(rec, cityPath, scopeRoot, "cache_repair", "L2", "", decision.ResolvedID)
		}
		return report, nil
	case actionSeedL2L3:
		metaUpdated, err := writeManagedMetadataProjectID(metadataPath, decision.ResolvedID)
		if err != nil {
			return managedDoltProjectIDReport{}, err
		}
		if metaUpdated {
			emitProjectIdentityStampedEvent(rec, cityPath, scopeRoot, "cache_repair", "L2", "", decision.ResolvedID)
		}
		dbUpdated, err := seedL3(ctx, decision.ResolvedID)
		if err != nil {
			return managedDoltProjectIDReport{}, err
		}
		if dbUpdated {
			emitProjectIdentityStampedEvent(rec, cityPath, scopeRoot, "cache_repair", "L3", "", decision.ResolvedID)
		}
		report.MetadataUpdated = metaUpdated
		report.DatabaseUpdated = dbUpdated
		return report, nil
	case actionMigrateFromL2:
		updated, err := writeProjectIdentityIfNeeded(fs, scopeRoot, decision.ResolvedID)
		if err != nil {
			return managedDoltProjectIDReport{}, err
		}
		report.IdentityFileUpdated = updated
		if updated {
			emitProjectIdentityStampedEvent(rec, cityPath, scopeRoot, "migrated_from_metadata", "L1", "", decision.ResolvedID)
		}
		return report, nil
	case actionMigrateL1SeedL3:
		identityUpdated, err := writeProjectIdentityIfNeeded(fs, scopeRoot, decision.ResolvedID)
		if err != nil {
			return managedDoltProjectIDReport{}, err
		}
		if identityUpdated {
			emitProjectIdentityStampedEvent(rec, cityPath, scopeRoot, "migrated_from_metadata", "L1", "", decision.ResolvedID)
		}
		dbUpdated, err := seedL3(ctx, decision.ResolvedID)
		if err != nil {
			return managedDoltProjectIDReport{}, err
		}
		if dbUpdated {
			emitProjectIdentityStampedEvent(rec, cityPath, scopeRoot, "migrated_from_metadata", "L3", "", decision.ResolvedID)
		}
		report.IdentityFileUpdated = identityUpdated
		report.DatabaseUpdated = dbUpdated
		return report, nil
	case actionAdoptFromL3SeedL2:
		identityUpdated, err := writeProjectIdentityIfNeeded(fs, scopeRoot, decision.ResolvedID)
		if err != nil {
			return managedDoltProjectIDReport{}, err
		}
		if identityUpdated {
			emitProjectIdentityStampedEvent(rec, cityPath, scopeRoot, "migrated_from_database", "L1", "", decision.ResolvedID)
		}
		metaUpdated, err := writeManagedMetadataProjectID(metadataPath, decision.ResolvedID)
		if err != nil {
			return managedDoltProjectIDReport{}, err
		}
		if metaUpdated {
			emitProjectIdentityStampedEvent(rec, cityPath, scopeRoot, "migrated_from_database", "L2", "", decision.ResolvedID)
		}
		report.IdentityFileUpdated = identityUpdated
		report.MetadataUpdated = metaUpdated
		return report, nil
	case actionGenerate:
		projectID, err := generateLocalProjectID()
		if err != nil {
			return managedDoltProjectIDReport{}, err
		}
		identityUpdated, metaUpdated, dbUpdated, err := writeProjectIdentityToAllLayers(ctx, fs, scopeRoot, projectID, seedL3)
		if err != nil {
			return managedDoltProjectIDReport{}, err
		}
		report.ProjectID = projectID
		report.IdentityFileUpdated = identityUpdated
		report.MetadataUpdated = metaUpdated
		report.DatabaseUpdated = dbUpdated
		if identityUpdated {
			emitProjectIdentityStampedEvent(rec, cityPath, scopeRoot, "generated", "L1", "", projectID)
		}
		if metaUpdated {
			emitProjectIdentityStampedEvent(rec, cityPath, scopeRoot, "generated", "L2", "", projectID)
		}
		if dbUpdated {
			emitProjectIdentityStampedEvent(rec, cityPath, scopeRoot, "generated", "L3", "", projectID)
		}
		return report, nil
	default:
		return managedDoltProjectIDReport{}, fmt.Errorf("unknown project identity reconcile action %d", decision.Action)
	}
}

func emitProjectIdentityStampedEvent(rec events.Recorder, cityPath, scopeRoot, source, layer, oldID, newID string) {
	if rec == nil {
		return
	}
	payload := gcapi.ProjectIdentityStampedPayload{
		ScopeRoot: projectIdentityEventScopeRoot(cityPath, scopeRoot),
		Source:    source,
		Layer:     layer,
		OldID:     oldID,
		NewID:     newID,
	}
	data, err := json.Marshal(payload)
	if err != nil {
		return
	}
	rec.Record(events.Event{
		Type:    events.ProjectIdentityStamped,
		Actor:   "gc dolt-state ensure-project-id",
		Subject: payload.ScopeRoot,
		Payload: data,
	})
}

func projectIdentityEventScopeRoot(cityPath, scopeRoot string) string {
	cityPath = strings.TrimSpace(cityPath)
	scopeRoot = strings.TrimSpace(scopeRoot)
	if cityPath == "" || scopeRoot == "" {
		return filepath.ToSlash(filepath.Clean(scopeRoot))
	}
	rel, err := filepath.Rel(filepath.Clean(cityPath), filepath.Clean(scopeRoot))
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return filepath.ToSlash(filepath.Clean(scopeRoot))
	}
	if rel == "." {
		return ""
	}
	return filepath.ToSlash(rel)
}

func writeProjectIdentityIfNeeded(fs fsys.FS, scopeRoot string, id string) (bool, error) {
	existing, ok, err := contract.ReadProjectIdentity(fs, scopeRoot)
	if err != nil {
		return false, err
	}
	if ok {
		if existing == id {
			return false, nil
		}
		return false, fmt.Errorf("identity %s already has project.id %q, refusing to overwrite with %q", contract.ProjectIdentityPath(scopeRoot), existing, id)
	}
	if err := contract.WriteProjectIdentity(fs, scopeRoot, id); err != nil {
		return false, err
	}
	return true, nil
}

func writeProjectIdentityToAllLayers(ctx context.Context, fs fsys.FS, scopeRoot string, id string, seedL3 func(context.Context, string) (bool, error)) (l1Updated, l2Updated, l3Updated bool, err error) {
	l1Updated, err = writeProjectIdentityIfNeeded(fs, scopeRoot, id)
	if err != nil {
		return false, false, false, err
	}
	metadataPath := filepath.Join(scopeRoot, ".beads", "metadata.json")
	l2Updated, err = writeManagedMetadataProjectID(metadataPath, id)
	if err != nil {
		return l1Updated, false, false, err
	}
	l3Updated, err = seedL3(ctx, id)
	if err != nil {
		return l1Updated, l2Updated, false, err
	}
	return l1Updated, l2Updated, l3Updated, nil
}

func formatL1L3MismatchError(l1, l3 string) error {
	return fmt.Errorf(
		"PROJECT IDENTITY MISMATCH — refusing to connect:\n"+
			"  canonical "+projectIdentityProjectIDRef+" = %q\n"+
			"  database metadata._project_id              = %q\n"+
			"\n"+
			"The git-tracked identity does not match the database stamp. "+
			"The database may belong to a different rig, or the identity "+
			"file may have been changed without re-stamping the database. "+
			"Inspect both values and resolve manually before reconnecting.",
		l1, l3,
	)
}

func formatLegacyL2L3MismatchError(l2, l3 string) error {
	return fmt.Errorf(
		"LEGACY PROJECT IDENTITY MISMATCH — refusing to connect:\n"+
			"  metadata.json project_id      = %q\n"+
			"  database metadata._project_id  = %q\n"+
			"\n"+
			"This rig predates the canonical "+projectIdentityDisplayPath+" file. "+
			"The two legacy storage layers disagree, so we cannot safely "+
			"seed the canonical layer from either one. Inspect both values "+
			"and decide which is correct, then create "+projectIdentityDisplayPath+" "+
			"with the chosen value to unblock reconcile.",
		l2, l3,
	)
}

func managedDoltOpenDatabase(host, port, user, database string) (*sql.DB, error) {
	host = managedDoltConnectHost(host)
	port = strings.TrimSpace(port)
	if port == "" {
		return nil, fmt.Errorf("missing port")
	}
	user = strings.TrimSpace(user)
	if user == "" {
		user = "root"
	}
	database = strings.TrimSpace(database)
	if database == "" {
		return nil, fmt.Errorf("missing database")
	}
	cfg := mysql.NewConfig()
	cfg.User = user
	cfg.Passwd = managedDoltPassword()
	cfg.Net = "tcp"
	cfg.Addr = host + ":" + port
	cfg.DBName = database
	cfg.Timeout = 5 * time.Second
	cfg.ReadTimeout = 5 * time.Second
	cfg.WriteTimeout = 5 * time.Second
	cfg.AllowNativePasswords = true
	return sql.Open("mysql", cfg.FormatDSN())
}

func readManagedMetadataProjectID(metadataPath string) (string, error) {
	data, err := os.ReadFile(metadataPath)
	if err != nil {
		return "", err
	}
	var meta map[string]any
	if err := json.Unmarshal(data, &meta); err != nil {
		return "", fmt.Errorf("parse metadata %s: %w", metadataPath, err)
	}
	raw, ok := meta["project_id"]
	if !ok || raw == nil {
		return "", nil
	}
	switch value := raw.(type) {
	case string:
		return strings.TrimSpace(value), nil
	default:
		projectID := strings.TrimSpace(fmt.Sprint(value))
		if projectID == "" || projectID == "<nil>" || strings.EqualFold(projectID, "null") {
			return "", nil
		}
		return projectID, nil
	}
}

func writeManagedMetadataProjectID(metadataPath, projectID string) (bool, error) {
	data, err := os.ReadFile(metadataPath)
	if err != nil {
		return false, err
	}
	var meta map[string]any
	if err := json.Unmarshal(data, &meta); err != nil {
		return false, fmt.Errorf("parse metadata %s: %w", metadataPath, err)
	}
	if strings.TrimSpace(fmt.Sprint(meta["project_id"])) == projectID {
		return false, nil
	}
	meta["project_id"] = projectID
	encoded, err := json.MarshalIndent(meta, "", "  ")
	if err != nil {
		return false, err
	}
	encoded = append(encoded, '\n')
	if err := os.WriteFile(metadataPath, encoded, 0o644); err != nil {
		return false, err
	}
	return true, nil
}

func seedDatabaseProjectID(ctx context.Context, db *sql.DB, projectID string) (bool, error) {
	existing, ok, err := readDatabaseProjectID(ctx, db)
	if err != nil {
		return false, err
	}
	if ok {
		if existing != projectID {
			return false, fmt.Errorf("database _project_id %q does not match desired %q", existing, projectID)
		}
		return false, nil
	}
	if err := ensureDatabaseMetadataTable(ctx, db); err != nil {
		return false, err
	}
	if _, err := db.ExecContext(ctx, "INSERT INTO metadata (`key`, value) VALUES ('_project_id', ?) ON DUPLICATE KEY UPDATE value = VALUES(value)", projectID); err != nil {
		return false, fmt.Errorf("seed database _project_id: %w", err)
	}
	return true, nil
}

func ensureDatabaseMetadataTable(ctx context.Context, db *sql.DB) error {
	_, err := db.ExecContext(ctx, "CREATE TABLE IF NOT EXISTS metadata (`key` VARCHAR(255) PRIMARY KEY, value LONGTEXT)")
	if err != nil {
		return fmt.Errorf("ensure metadata table: %w", err)
	}
	return nil
}

func generateLocalProjectID() (string, error) {
	buf := make([]byte, 16)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return "gc-local-" + hex.EncodeToString(buf), nil
}
