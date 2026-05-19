package main

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"

	"github.com/gastownhall/gascity/internal/runtime/t3bridge"
	_ "github.com/mattn/go-sqlite3"
	"github.com/spf13/cobra"
)

type sessionAuditRow struct {
	ThreadID         string
	Title            string
	ProjectTitle     string
	WorkspaceRoot    string
	ProjectionStatus string
	RuntimeStatus    string
	RuntimeProvider  string
	RuntimePID       int
	Metadata         map[string]string
	SessionEnv       map[string]string
}

func auditExpectedFolder(session sessionAuditRecord, row sessionAuditRow) string {
	if v := strings.TrimSpace(row.Metadata["gc.startupTemplate"]); v != "" {
		return v
	}
	if v := strings.TrimSpace(row.SessionEnv["GC_TEMPLATE"]); v != "" {
		return v
	}
	if v := strings.TrimSpace(session.Template); v != "" {
		return v
	}
	return ""
}

func auditActualFolder(row sessionAuditRow) string {
	if v := strings.TrimSpace(row.Metadata["gc.agent"]); v != "" {
		return v
	}
	if v := strings.TrimSpace(row.SessionEnv["GC_AGENT"]); v != "" {
		return v
	}
	return ""
}

type sessionAuditRecord struct {
	ID          string `json:"ID"`
	State       string `json:"State"`
	Template    string `json:"Template"`
	SessionName string `json:"SessionName"`
	Alias       string `json:"Alias"`
	Provider    string `json:"Provider"`
	WorkDir     string `json:"WorkDir"`
}

func newSessionAuditEnvCmd(stdout, stderr io.Writer) *cobra.Command {
	var dbPath string
	cmd := &cobra.Command{
		Use:   "audit-env",
		Short: "Audit session bindings and persisted GC env against T3 state",
		Long: `Audit live GC sessions against the T3 projection database.

Checks that each active session has:
- a matching T3 thread binding
- gc.sessionName / gc.sessionEnv metadata
- a projected thread session row
- a provider runtime row
- the persisted GC_* env keys needed for recovery and reuse
- the live provider process env keys needed for runtime correctness when a PID is available

This audits both T3-persisted GC metadata and live /proc process environments when the provider runtime reports a PID.`,
		Args: cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			if cmdSessionAuditEnv(dbPath, stdout, stderr) != 0 {
				return errExit
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&dbPath, "db", "", "Path to T3 projection database (default: ~/.t3/dev/state-proj.sqlite)")
	return cmd
}

func cmdSessionAuditEnv(dbPath string, stdout, stderr io.Writer) int {
	if strings.TrimSpace(dbPath) == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			fmt.Fprintf(stderr, "gc session audit-env: resolve home: %v\n", err) //nolint:errcheck
			return 1
		}
		dbPath = filepath.Join(home, ".t3", "dev", "state-proj.sqlite")
	}
	if _, err := os.Stat(dbPath); err != nil {
		fmt.Fprintf(stderr, "gc session audit-env: projection db %q: %v\n", dbPath, err) //nolint:errcheck
		return 1
	}

	cityPath, cityErr := resolveCity()
	if cityErr != nil {
		fmt.Fprintf(stderr, "gc session audit-env: %v\n", cityErr) //nolint:errcheck
		return 1
	}

	sessions, sessionErr := loadSessionAuditRecords(cityPath)
	rows, err := loadSessionAuditRows(dbPath)
	if err != nil {
		fmt.Fprintf(stderr, "gc session audit-env: query db: %v\n", err) //nolint:errcheck
		return 1
	}

	bindings := map[string][]sessionAuditRow{}
	dbRunning := make([]sessionAuditRow, 0)
	for _, row := range rows {
		sessionName := auditDeriveSessionName(row.Metadata, row.SessionEnv)
		if sessionName != "" {
			bindings[sessionName] = append(bindings[sessionName], row)
		}
		if isAuditRunningLike(row.RuntimeStatus) || isAuditRunningLike(row.ProjectionStatus) {
			dbRunning = append(dbRunning, row)
		}
	}

	active := make([]sessionAuditRecord, 0)
	asleep := make([]sessionAuditRecord, 0)
	for _, session := range sessions {
		switch session.State {
		case "active":
			active = append(active, session)
		case "asleep":
			asleep = append(asleep, session)
		}
	}

	fmt.Fprintln(stdout, "=== Summary ===")                        //nolint:errcheck
	fmt.Fprintf(stdout, "projection_db=%s\n", dbPath)              //nolint:errcheck
	fmt.Fprintf(stdout, "active_sessions=%d\n", len(active))       //nolint:errcheck
	fmt.Fprintf(stdout, "asleep_sessions=%d\n", len(asleep))       //nolint:errcheck
	fmt.Fprintf(stdout, "db_running_threads=%d\n", len(dbRunning)) //nolint:errcheck
	if sessionErr != "" {
		fmt.Fprintf(stdout, "session_list_error=%s\n", sessionErr) //nolint:errcheck
	}

	fmt.Fprintln(stdout, "\n=== Active Session Audit ===") //nolint:errcheck
	failures := make([]string, 0)
	for _, session := range active {
		sessionName := strings.TrimSpace(session.SessionName)
		if sessionName == "" {
			sessionName = strings.TrimSpace(session.Template)
		}
		if sessionName == "" {
			continue
		}
		matches := bindings[sessionName]
		if len(matches) == 0 {
			failures = append(failures, fmt.Sprintf("%s: no matching thread metadata", sessionName))
			fmt.Fprintf(stdout, "FAIL %s: no matching thread metadata\n", sessionName) //nolint:errcheck
			continue
		}
		row := matches[0]
		issues := auditSessionIssues(session, row, true)
		statusLine := fmt.Sprintf(
			"  gc=%s thread=%s project=%s projection=%s runtime=%s provider=%s pid=%s folderExpected=%s folderActual=%s",
			session.State,
			row.ThreadID,
			row.ProjectTitle,
			auditEmptyDash(row.ProjectionStatus),
			auditEmptyDash(row.RuntimeStatus),
			auditEmptyDash(row.RuntimeProvider),
			auditPID(row.RuntimePID),
			auditEmptyDash(auditExpectedFolder(session, row)),
			auditEmptyDash(auditActualFolder(row)),
		)
		if len(issues) > 0 {
			failures = append(failures, fmt.Sprintf("%s: %s", sessionName, strings.Join(issues, "; ")))
			fmt.Fprintf(stdout, "FAIL %s: %s\n%s\n", sessionName, strings.Join(issues, "; "), statusLine) //nolint:errcheck
			continue
		}
		fmt.Fprintf(stdout, "OK   %s\n%s\n", sessionName, statusLine) //nolint:errcheck
	}

	fmt.Fprintln(stdout, "\n=== Asleep Session Audit ===") //nolint:errcheck
	for _, session := range asleep {
		sessionName := strings.TrimSpace(session.SessionName)
		if sessionName == "" {
			sessionName = strings.TrimSpace(session.Template)
		}
		if sessionName == "" {
			continue
		}
		matches := bindings[sessionName]
		if len(matches) == 0 {
			fmt.Fprintf(stdout, "UNBOUND %s: no persisted thread binding yet\n", sessionName) //nolint:errcheck
			continue
		}
		row := matches[0]
		issues := auditSessionIssues(session, row, false)
		statusLine := fmt.Sprintf(
			"  gc=%s thread=%s project=%s projection=%s runtime=%s provider=%s pid=%s folderExpected=%s folderActual=%s",
			session.State,
			row.ThreadID,
			row.ProjectTitle,
			auditEmptyDash(row.ProjectionStatus),
			auditEmptyDash(row.RuntimeStatus),
			auditEmptyDash(row.RuntimeProvider),
			auditPID(row.RuntimePID),
			auditEmptyDash(auditExpectedFolder(session, row)),
			auditEmptyDash(auditActualFolder(row)),
		)
		if len(issues) > 0 {
			fmt.Fprintf(stdout, "WARN %s: %s\n%s\n", sessionName, strings.Join(issues, "; "), statusLine) //nolint:errcheck
			continue
		}
		fmt.Fprintf(stdout, "OK   %s\n%s\n", sessionName, statusLine) //nolint:errcheck
	}

	fmt.Fprintln(stdout, "\n=== Folder Detail ===") //nolint:errcheck
	focus := map[string]struct{}{}
	for _, session := range append(active, asleep...) {
		name := strings.TrimSpace(session.SessionName)
		if name != "" {
			focus[name] = struct{}{}
		}
	}
	if len(focus) == 0 {
		for name := range bindings {
			if name != "" {
				focus[name] = struct{}{}
			}
		}
	}
	focusNames := make([]string, 0, len(focus))
	for name := range focus {
		focusNames = append(focusNames, name)
	}
	sort.Strings(focusNames)
	for _, sessionName := range focusNames {
		matches := bindings[sessionName]
		if len(matches) == 0 {
			fmt.Fprintf(stdout, "MISS %s: no matching thread metadata\n", sessionName) //nolint:errcheck
			continue
		}
		for _, row := range matches {
			expectedFolder := auditExpectedFolder(sessionAuditRecord{SessionName: sessionName}, row)
			actualFolder := auditActualFolder(row)
			fmt.Fprintf(
				stdout,
				"HIT  %s thread=%s title=%q project=%s projection=%s runtime=%s pid=%s folderExpected=%s folderActual=%s template=%s gc.rig=%s GC_SESSION_NAME=%s GC_AGENT=%s GC_RIG=%s\n",
				sessionName,
				row.ThreadID,
				row.Title,
				row.ProjectTitle,
				auditEmptyDash(row.ProjectionStatus),
				auditEmptyDash(row.RuntimeStatus),
				auditPID(row.RuntimePID),
				auditEmptyDash(expectedFolder),
				auditEmptyDash(actualFolder),
				auditEmptyDash(row.Metadata["gc.startupTemplate"]),
				auditEmptyDash(row.Metadata["gc.rig"]),
				auditEmptyDash(row.SessionEnv["GC_SESSION_NAME"]),
				auditEmptyDash(row.SessionEnv["GC_AGENT"]),
				auditEmptyDash(row.SessionEnv["GC_RIG"]),
			) //nolint:errcheck
		}
	}

	fmt.Fprintln(stdout, "\n=== Result ===") //nolint:errcheck
	if len(failures) == 0 {
		fmt.Fprintln(stdout, "All active sessions have matching T3 thread bindings and required GC env metadata.") //nolint:errcheck
		return 0
	}
	for _, failure := range failures {
		fmt.Fprintf(stdout, "- %s\n", failure) //nolint:errcheck
	}
	return 1
}

func loadSessionAuditRecords(cityPath string) ([]sessionAuditRecord, string) {
	exe, err := os.Executable()
	if err != nil {
		return nil, err.Error()
	}
	cmd := exec.Command(exe, "--city", cityPath, "session", "list", "--json")
	cmd.Stderr = os.Stderr
	out, err := cmd.Output()
	if err != nil {
		return nil, err.Error()
	}
	var sessions []sessionAuditRecord
	if err := json.Unmarshal(out, &sessions); err != nil {
		return nil, err.Error()
	}
	return sessions, ""
}

func loadSessionAuditRows(dbPath string) ([]sessionAuditRow, error) {
	rows, err := loadSessionAuditRowsSQLite(dbPath)
	if err == nil {
		return rows, nil
	}
	if !strings.Contains(strings.ToLower(err.Error()), "malformed") {
		return nil, err
	}
	return loadSessionAuditRowsViaDoltlite(dbPath)
}

func loadSessionAuditRowsSQLite(dbPath string) ([]sessionAuditRow, error) {
	db, err := sql.Open("sqlite3", dbPath)
	if err != nil {
		return nil, err
	}
	defer db.Close()

	const query = `
		SELECT
			t.thread_id,
			t.title,
			p.title,
			p.workspace_root,
			t.custom_metadata,
			COALESCE(s.status, ''),
			COALESCE(r.status, ''),
			COALESCE(r.provider_name, s.provider_name, ''),
			COALESCE(r.runtime_payload_json, '')
		FROM projection_threads t
		JOIN projection_projects p ON p.project_id = t.project_id
		LEFT JOIN projection_thread_sessions s ON s.thread_id = t.thread_id
		LEFT JOIN provider_session_runtime r ON r.thread_id = t.thread_id
		WHERE t.deleted_at IS NULL
		  AND p.deleted_at IS NULL
		ORDER BY t.updated_at DESC, t.thread_id ASC
	`

	rows, err := db.Query(query)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	result := make([]sessionAuditRow, 0)
	for rows.Next() {
		var (
			threadID         string
			title            string
			projectTitle     string
			workspaceRoot    string
			customMetadata   string
			projectionStatus string
			runtimeStatus    string
			runtimeProvider  string
			runtimePayload   string
		)
		if err := rows.Scan(&threadID, &title, &projectTitle, &workspaceRoot, &customMetadata, &projectionStatus, &runtimeStatus, &runtimeProvider, &runtimePayload); err != nil {
			return nil, err
		}
		metadata := map[string]string{}
		_ = json.Unmarshal([]byte(customMetadata), &metadata)
		env := t3bridge.ParseSessionEnv(strings.TrimSpace(metadata["gc.sessionEnv"]))
		if env == nil {
			env = map[string]string{}
		}
		result = append(result, sessionAuditRow{
			ThreadID:         threadID,
			Title:            title,
			ProjectTitle:     projectTitle,
			WorkspaceRoot:    workspaceRoot,
			ProjectionStatus: projectionStatus,
			RuntimeStatus:    runtimeStatus,
			RuntimeProvider:  runtimeProvider,
			RuntimePID:       auditRuntimePID(runtimePayload),
			Metadata:         metadata,
			SessionEnv:       env,
		})
	}
	return result, rows.Err()
}

func loadSessionAuditRowsViaDoltlite(dbPath string) ([]sessionAuditRow, error) {
	const helper = `
const { DatabaseSync } = require('/data/projects/t3code/packages/doltlite');
const dbPath = process.argv[1];
const db = new DatabaseSync(dbPath, { readonly: true });
const sql = "SELECT " +
  "t.thread_id AS thread_id, " +
  "t.title AS thread_title, " +
  "p.title AS project_title, " +
  "p.workspace_root AS workspace_root, " +
  "t.custom_metadata AS custom_metadata, " +
  "COALESCE(s.status, '') AS projection_status, " +
  "COALESCE(r.status, '') AS runtime_status, " +
  "COALESCE(r.provider_name, s.provider_name, '') AS runtime_provider, " +
  "COALESCE(r.runtime_payload_json, '') AS runtime_payload_json " +
  "FROM projection_threads t " +
  "JOIN projection_projects p ON p.project_id = t.project_id " +
  "LEFT JOIN projection_thread_sessions s ON s.thread_id = t.thread_id " +
  "LEFT JOIN provider_session_runtime r ON r.thread_id = t.thread_id " +
  "WHERE t.deleted_at IS NULL AND p.deleted_at IS NULL " +
  "ORDER BY t.updated_at DESC, t.thread_id ASC";
const rows = db.prepare(sql).all();
console.log(JSON.stringify(rows));
db.close();
`
	cmd := exec.Command("node", "-e", helper, dbPath)
	out, err := cmd.Output()
	if err != nil {
		return nil, err
	}
	var rawRows []map[string]interface{}
	if err := json.Unmarshal(out, &rawRows); err != nil {
		return nil, err
	}
	result := make([]sessionAuditRow, 0, len(rawRows))
	for _, raw := range rawRows {
		customMetadata := auditJSONField(raw, "custom_metadata")
		metadata := map[string]string{}
		_ = json.Unmarshal([]byte(customMetadata), &metadata)
		env := t3bridge.ParseSessionEnv(strings.TrimSpace(metadata["gc.sessionEnv"]))
		if env == nil {
			env = map[string]string{}
		}
		result = append(result, sessionAuditRow{
			ThreadID:         auditJSONField(raw, "thread_id"),
			Title:            auditJSONField(raw, "thread_title"),
			ProjectTitle:     auditJSONField(raw, "project_title"),
			WorkspaceRoot:    auditJSONField(raw, "workspace_root"),
			ProjectionStatus: auditJSONField(raw, "projection_status"),
			RuntimeStatus:    auditJSONField(raw, "runtime_status"),
			RuntimeProvider:  auditJSONField(raw, "runtime_provider"),
			RuntimePID:       auditRuntimePID(auditJSONField(raw, "runtime_payload_json")),
			Metadata:         metadata,
			SessionEnv:       env,
		})
	}
	return result, nil
}

func auditJSONField(raw map[string]interface{}, key string) string {
	if s, ok := raw[key].(string); ok {
		return s
	}
	return ""
}

func auditDeriveSessionName(metadata map[string]string, env map[string]string) string {
	if v := strings.TrimSpace(t3bridge.SessionNameFromMetadata(metadata)); v != "" {
		return v
	}
	if v := strings.TrimSpace(env["GC_SESSION_NAME"]); v != "" {
		return v
	}
	agent := strings.TrimSpace(metadata["gc.agent"])
	if agent == "" {
		return ""
	}
	if strings.Contains(agent, "/") {
		return strings.Replace(agent, "/", "--", 1)
	}
	return agent
}

func auditRequiredEnv(metadata map[string]string, env map[string]string) []string {
	required := []string{
		"GC_SESSION_NAME",
		"GC_AGENT",
		"GC_ALIAS",
		"GC_CITY",
		"GC_CITY_PATH",
		"GC_TEMPLATE",
		"GC_PROVIDER",
	}
	rig := strings.TrimSpace(metadata["gc.rig"])
	if rig == "" {
		rig = strings.TrimSpace(env["GC_RIG"])
	}
	if rig != "" {
		required = append(required, "GC_RIG", "GC_RIG_ROOT")
	}
	return required
}

func auditSessionIssues(session sessionAuditRecord, row sessionAuditRow, requireRunning bool) []string {
	issues := make([]string, 0)
	if strings.TrimSpace(row.Metadata["gc.agent"]) == "" {
		issues = append(issues, "missing gc.agent")
	}
	expectedFolder := auditExpectedFolder(session, row)
	actualFolder := auditActualFolder(row)
	if expectedFolder != "" && actualFolder != "" && expectedFolder != actualFolder {
		issues = append(issues, "folder drift")
	}
	if auditDeriveSessionName(row.Metadata, row.SessionEnv) == "" {
		issues = append(issues, "missing gc.sessionName")
	}
	if strings.TrimSpace(row.Metadata["gc.sessionEnv"]) == "" {
		issues = append(issues, "missing gc.sessionEnv")
	}
	if missing := auditMissingKeys(auditRequiredEnv(row.Metadata, row.SessionEnv), row.SessionEnv); len(missing) > 0 {
		issues = append(issues, "missing env keys: "+strings.Join(missing, ", "))
	}
	if strings.TrimSpace(row.ProjectionStatus) == "" {
		issues = append(issues, "missing projection_thread_sessions row")
	}
	if strings.TrimSpace(row.RuntimeStatus) == "" {
		issues = append(issues, "missing provider_session_runtime row")
	}
	if workDir := strings.TrimSpace(session.WorkDir); workDir != "" && strings.TrimSpace(row.WorkspaceRoot) != "" && workDir != row.WorkspaceRoot {
		issues = append(issues, "workspace root drift")
	}
	if provider := normalizeAuditProvider(session.Provider); provider != "" &&
		normalizeAuditProvider(row.RuntimeProvider) != "" &&
		provider != normalizeAuditProvider(row.RuntimeProvider) {
		issues = append(issues, "provider drift")
	}
	if requireRunning {
		if !isAuditRunningLike(row.ProjectionStatus) && !isAuditRunningLike(row.RuntimeStatus) {
			issues = append(issues, "session not materially alive in T3")
		}
		if row.RuntimePID <= 0 {
			issues = append(issues, "missing persisted pid")
		} else if procEnv, err := auditReadProcessEnv(row.RuntimePID); err != nil {
			issues = append(issues, "process env unavailable: "+err.Error())
		} else if missing := auditMissingKeys(auditRequiredEnv(row.Metadata, row.SessionEnv), procEnv); len(missing) > 0 {
			issues = append(issues, "process env missing keys: "+strings.Join(missing, ", "))
		} else if mismatches := auditEnvMismatches(auditRequiredEnv(row.Metadata, row.SessionEnv), row.SessionEnv, procEnv); len(mismatches) > 0 {
			issues = append(issues, "process env drift: "+strings.Join(mismatches, ", "))
		}
	} else if isAuditRunningLike(row.ProjectionStatus) || isAuditRunningLike(row.RuntimeStatus) {
		issues = append(issues, "session asleep in GC but still running in T3")
	}
	return issues
}

func normalizeAuditProvider(provider string) string {
	switch strings.TrimSpace(provider) {
	case "claude", "claudeAgent":
		return "claudeAgent"
	case "codex":
		return "codex"
	default:
		return strings.TrimSpace(provider)
	}
}

func auditMissingKeys(required []string, env map[string]string) []string {
	missing := make([]string, 0)
	for _, key := range required {
		if strings.TrimSpace(env[key]) == "" {
			missing = append(missing, key)
		}
	}
	return missing
}

func isAuditRunningLike(value string) bool {
	value = strings.ToLower(strings.TrimSpace(value))
	return value == "running" || value == "ready"
}

func auditEmptyDash(value string) string {
	if strings.TrimSpace(value) == "" {
		return "-"
	}
	return value
}

func auditPID(pid int) string {
	if pid <= 0 {
		return "-"
	}
	return fmt.Sprintf("%d", pid)
}

func auditRuntimePID(raw string) int {
	if strings.TrimSpace(raw) == "" {
		return 0
	}
	var payload map[string]interface{}
	if err := json.Unmarshal([]byte(raw), &payload); err != nil {
		return 0
	}
	if value, ok := payload["pid"].(float64); ok {
		return int(value)
	}
	return 0
}

func auditReadProcessEnv(pid int) (map[string]string, error) {
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/environ", pid))
	if err != nil {
		return nil, err
	}
	env := map[string]string{}
	for _, entry := range strings.Split(string(data), "\x00") {
		if entry == "" {
			continue
		}
		key, value, ok := strings.Cut(entry, "=")
		if !ok {
			continue
		}
		env[key] = value
	}
	return env, nil
}

func auditEnvMismatches(keys []string, expected map[string]string, actual map[string]string) []string {
	mismatches := make([]string, 0)
	for _, key := range keys {
		want := strings.TrimSpace(expected[key])
		got := strings.TrimSpace(actual[key])
		if want == "" || got == "" {
			continue
		}
		if want != got {
			mismatches = append(mismatches, key)
		}
	}
	return mismatches
}
