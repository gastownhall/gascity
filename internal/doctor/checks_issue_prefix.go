package doctor

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
	"time"
)

// IssuePrefixConfigCheck verifies that a managed-bd Dolt database has its
// `config` table populated with the expected `issue_prefix` row.
//
// Background (#1232): bd 1.0.3+ refuses `bd config set issue_prefix` with
// "issue_prefix is reserved for setup". Older versions of the gc-beads-bd
// lifecycle script swallowed that rejection and exited successfully with
// the row missing. `bd create` then fails at runtime with
// "database not initialized: issue_prefix config is missing" — affecting
// every gc command that writes beads (gc session attach, gc convoy create,
// gc sling, order dispatch, etc.).
//
// The lifecycle script now writes the row via direct SQL when bd rejects
// the call, but pre-existing cities are still in the broken state until
// something re-runs init. This check surfaces drift on any patrol cycle
// and `gc doctor --fix` writes the missing row.
type IssuePrefixConfigCheck struct {
	// Label identifies this check instance (e.g., "city" or rig name).
	Label string
	// Prefix is the canonical issue_prefix value the row should hold.
	Prefix string
	// DoltDatabase is the database name to USE before querying config.
	DoltDatabase string
	// Host is the Dolt server hostname (typically 127.0.0.1).
	Host string
	// Port is the Dolt server port. Empty disables the check (no managed
	// server running for this city — Dolt-related checks short-circuit).
	Port string
	// User is the SQL user (typically "root").
	User string
	// Password is the SQL password, usually from GC_DOLT_PASSWORD.
	Password string
	// Skip disables the check when the city does not use managed bd.
	Skip bool

	// runner is the function used to execute SQL. Tests inject a stub;
	// the default shells out to the dolt binary.
	runner doltSQLRunner

	// observedValue is populated by Run for use by Fix and for verbose
	// reporting.
	observedValue string
}

// doltSQLRunner executes a SQL query against the managed Dolt server and
// returns its stdout as a single string. The query may contain multiple
// statements; runners must propagate the server's first error.
type doltSQLRunner func(ctx context.Context, query, host, port, user, password string) (string, error)

// NewIssuePrefixConfigCheck creates a new issue-prefix drift check for
// the given scope.
func NewIssuePrefixConfigCheck(label, prefix, doltDatabase, host, port, user, password string, skip bool) *IssuePrefixConfigCheck {
	return &IssuePrefixConfigCheck{
		Label:        label,
		Prefix:       prefix,
		DoltDatabase: doltDatabase,
		Host:         host,
		Port:         port,
		User:         user,
		Password:     password,
		Skip:         skip,
		runner:       defaultDoltSQLRunner,
	}
}

// Name returns the check identifier.
func (c *IssuePrefixConfigCheck) Name() string {
	return "issue-prefix-config:" + c.Label
}

// Run queries the Dolt config table and reports drift.
func (c *IssuePrefixConfigCheck) Run(_ *CheckContext) *CheckResult {
	r := &CheckResult{Name: c.Name()}
	if c.Skip {
		r.Status = StatusOK
		r.Message = "skipped (file backend, external dolt endpoint, or GC_DOLT=skip)"
		return r
	}
	if strings.TrimSpace(c.Port) == "" {
		// Server isn't running — defer to dolt-server / beads-store
		// checks which already report on availability. We can't query
		// for drift if there's no server to ask.
		r.Status = StatusOK
		r.Message = "skipped (managed dolt not running)"
		return r
	}
	if !validIssuePrefixIdent(c.DoltDatabase) {
		r.Status = StatusWarning
		r.Message = fmt.Sprintf("invalid dolt database name: %q", c.DoltDatabase)
		return r
	}
	if !validIssuePrefixIdent(c.Prefix) {
		r.Status = StatusWarning
		r.Message = fmt.Sprintf("invalid issue prefix: %q", c.Prefix)
		return r
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	query := fmt.Sprintf(
		"USE `%s`; SELECT value FROM config WHERE `key`='issue_prefix';",
		c.DoltDatabase,
	)
	runner := c.runner
	if runner == nil {
		runner = defaultDoltSQLRunner
	}
	out, err := runner(ctx, query, c.Host, c.Port, c.User, c.Password)
	if err != nil {
		// The most common reason: the config table is missing entirely
		// (a fresh bd init that failed to materialize schema), or the
		// database doesn't exist. Either way, surface the error and
		// suggest --fix; Fix() will attempt to repair.
		r.Status = StatusError
		r.Message = fmt.Sprintf("query config in %s: %v", c.DoltDatabase, err)
		r.FixHint = "run gc doctor --fix to write the missing issue_prefix row (issue #1232)"
		c.observedValue = ""
		return r
	}
	c.observedValue = extractIssuePrefixValue(out)

	if c.observedValue == c.Prefix {
		r.Status = StatusOK
		r.Message = fmt.Sprintf("issue_prefix=%s in %s", c.Prefix, c.DoltDatabase)
		return r
	}
	if c.observedValue == "" {
		r.Status = StatusError
		r.Message = fmt.Sprintf("issue_prefix row missing in %s.config (expected %s)", c.DoltDatabase, c.Prefix)
		r.FixHint = "run gc doctor --fix to write the missing issue_prefix row (issue #1232)"
		return r
	}
	r.Status = StatusError
	r.Message = fmt.Sprintf("issue_prefix=%s in %s.config, expected %s", c.observedValue, c.DoltDatabase, c.Prefix)
	r.FixHint = "run gc doctor --fix to overwrite the drifted issue_prefix value (issue #1232)"
	return r
}

// CanFix reports that drift is auto-repairable.
func (c *IssuePrefixConfigCheck) CanFix() bool { return true }

// Fix writes the canonical prefix into the config table and commits.
// Idempotent: safe to call when the row already matches.
func (c *IssuePrefixConfigCheck) Fix(_ *CheckContext) error {
	if c.Skip {
		return nil
	}
	if strings.TrimSpace(c.Port) == "" {
		return fmt.Errorf("managed dolt not running; start it before running --fix")
	}
	if !validIssuePrefixIdent(c.DoltDatabase) {
		return fmt.Errorf("invalid dolt database name: %q", c.DoltDatabase)
	}
	if !validIssuePrefixIdent(c.Prefix) {
		return fmt.Errorf("invalid issue prefix: %q", c.Prefix)
	}
	runner := c.runner
	if runner == nil {
		runner = defaultDoltSQLRunner
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	query := fmt.Sprintf(
		"USE `%s`; "+
			"INSERT INTO config (`key`, value) VALUES ('issue_prefix', '%s') "+
			"ON DUPLICATE KEY UPDATE value='%s'; "+
			"CALL DOLT_COMMIT('-Am', 'set issue_prefix=%s');",
		c.DoltDatabase, c.Prefix, c.Prefix, c.Prefix,
	)
	if _, err := runner(ctx, query, c.Host, c.Port, c.User, c.Password); err != nil {
		return fmt.Errorf("writing issue_prefix to %s.config: %w", c.DoltDatabase, err)
	}
	// Verify the row landed.
	verifyQuery := fmt.Sprintf(
		"USE `%s`; SELECT value FROM config WHERE `key`='issue_prefix';",
		c.DoltDatabase,
	)
	out, err := runner(ctx, verifyQuery, c.Host, c.Port, c.User, c.Password)
	if err != nil {
		return fmt.Errorf("verifying issue_prefix in %s.config: %w", c.DoltDatabase, err)
	}
	if got := extractIssuePrefixValue(out); got != c.Prefix {
		return fmt.Errorf("verification failed: %s.config has issue_prefix=%q, want %q", c.DoltDatabase, got, c.Prefix)
	}
	return nil
}

// validIssuePrefixIdent restricts identifiers used in interpolated SQL to
// the same set the bd lifecycle script accepts: alphanumeric plus
// hyphens and underscores. Stops both injection and accidental quoting
// failures from creative pack authors.
func validIssuePrefixIdent(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z':
		case r >= 'A' && r <= 'Z':
		case r >= '0' && r <= '9':
		case r == '_' || r == '-':
		default:
			return false
		}
	}
	return true
}

// extractIssuePrefixValue parses the dolt sql output for the SELECT
// query. The default `-q` formatter renders results as a small ASCII
// table:
//
//	+-------+
//	| value |
//	+-------+
//	| ga    |
//	+-------+
//
// The first `|...|` cell row is the column header; the second is the
// data row we want. An empty result ("Empty set") or a missing data
// row both surface as the empty string.
func extractIssuePrefixValue(out string) string {
	if strings.Contains(out, "Empty set") {
		return ""
	}
	var cells []string
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "+") {
			continue
		}
		if strings.HasPrefix(line, "|") && strings.HasSuffix(line, "|") {
			cells = append(cells, strings.TrimSpace(strings.Trim(line, "|")))
		}
	}
	// cells[0] is the column header, cells[1] is the first data row.
	if len(cells) < 2 {
		return ""
	}
	return cells[1]
}

// defaultDoltSQLRunner shells out to the dolt binary with the supplied
// connection parameters. Empty password is supported; --no-tls matches
// how the bd lifecycle script connects to the loopback server.
func defaultDoltSQLRunner(ctx context.Context, query, host, port, user, password string) (string, error) {
	args := []string{
		"--host", host,
		"--port", port,
		"--user", user,
		"--no-tls",
	}
	// Dolt accepts an empty --password but not the absence of the flag
	// when the server enforces credentials, so always pass it.
	args = append(args, "--password", password, "sql", "-q", query)
	cmd := exec.CommandContext(ctx, "dolt", args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return string(out), fmt.Errorf("dolt sql failed: %w (output: %s)", err, strings.TrimSpace(string(out)))
	}
	return string(out), nil
}
