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
	"strconv"
	"strings"
	"time"

	mysql "github.com/go-sql-driver/mysql"
	"github.com/spf13/cobra"
)

type managedDoltProjectIDReport struct {
	ProjectID       string
	MetadataUpdated bool
	DatabaseUpdated bool
	Source          string
}

func newEnsureProjectIDCmd(stdout, stderr io.Writer) *cobra.Command {
	var (
		metadataPath string
		host         string
		port         string
		user         string
		database     string
	)
	cmd := &cobra.Command{
		Use:    "ensure-project-id",
		Short:  "Ensure local metadata and the Dolt metadata table share a project identity",
		Hidden: true,
		Args:   cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			report, err := ensureManagedDoltProjectID(metadataPath, host, port, user, database)
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
	_ = cmd.MarkFlagRequired("metadata")
	_ = cmd.MarkFlagRequired("port")
	_ = cmd.MarkFlagRequired("database")
	return cmd
}

func ensureManagedDoltProjectID(metadataPath, host, port, user, database string) (managedDoltProjectIDReport, error) {
	metadataPath = strings.TrimSpace(metadataPath)
	if metadataPath == "" {
		return managedDoltProjectIDReport{}, fmt.Errorf("missing metadata path")
	}
	database = strings.TrimSpace(database)
	if database == "" {
		return managedDoltProjectIDReport{}, fmt.Errorf("missing database")
	}

	metadataProjectID, err := readManagedMetadataProjectID(metadataPath)
	if err != nil {
		return managedDoltProjectIDReport{}, err
	}

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

	report := managedDoltProjectIDReport{}
	switch {
	case metadataProjectID != "" && ok:
		if metadataProjectID != databaseProjectID {
			return managedDoltProjectIDReport{}, fmt.Errorf("metadata project_id %q does not match database _project_id %q", metadataProjectID, databaseProjectID)
		}
		report.ProjectID = metadataProjectID
		report.Source = "existing"
		return report, nil
	case metadataProjectID != "":
		updated, err := seedDatabaseProjectID(ctx, db, metadataProjectID)
		if err != nil {
			return managedDoltProjectIDReport{}, err
		}
		report.ProjectID = metadataProjectID
		report.DatabaseUpdated = updated
		report.Source = "metadata"
		return report, nil
	case ok:
		updated, err := writeManagedMetadataProjectID(metadataPath, databaseProjectID)
		if err != nil {
			return managedDoltProjectIDReport{}, err
		}
		report.ProjectID = databaseProjectID
		report.MetadataUpdated = updated
		report.Source = "database"
		return report, nil
	default:
		projectID, err := generateLocalProjectID()
		if err != nil {
			return managedDoltProjectIDReport{}, err
		}
		metaUpdated, err := writeManagedMetadataProjectID(metadataPath, projectID)
		if err != nil {
			return managedDoltProjectIDReport{}, err
		}
		dbUpdated, err := seedDatabaseProjectID(ctx, db, projectID)
		if err != nil {
			return managedDoltProjectIDReport{}, err
		}
		report.ProjectID = projectID
		report.MetadataUpdated = metaUpdated
		report.DatabaseUpdated = dbUpdated
		report.Source = "generated"
		return report, nil
	}
}

func managedDoltProjectIDFields(report managedDoltProjectIDReport) []string {
	return []string{
		"project_id\t" + report.ProjectID,
		"metadata_updated\t" + strconv.FormatBool(report.MetadataUpdated),
		"database_updated\t" + strconv.FormatBool(report.DatabaseUpdated),
		"source\t" + report.Source,
	}
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
