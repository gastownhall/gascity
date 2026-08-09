package contract

import (
	"errors"
	"fmt"
	"net/url"
	"strings"
)

// ErrPostgresDSNUnsupportedForm reports a postgres_dsn gc cannot derive an
// endpoint from. Errors from ParsePostgresDSNEndpoint wrap it when the DSN is
// not a postgres:// URL — most often because it uses libpq's keyword/value
// form ("host=... port=..."), which bd accepts at init time but gc does not.
// Callers match with errors.Is.
var ErrPostgresDSNUnsupportedForm = errors.New("postgres_dsn must be a postgres:// (or postgresql://) URL")

// PostgresEndpoint is the effective connection tuple for a postgres-backed
// scope: the values gc projects to bd subprocesses (BEADS_POSTGRES_* names)
// and keys credential lookups on ([host:port] credentials-file sections).
//
// ParsePostgresDSNEndpoint always populates Host and Port (Port defaults to
// 5432) but may leave User or Database empty when the DSN omits them.
// LoadMetadataState rejects a postgres scope whose endpoint is incomplete, so
// an endpoint derived from a state it returned always has all four.
type PostgresEndpoint struct {
	Host     string
	Port     string
	User     string
	Database string
}

// ParsePostgresDSNEndpoint derives the endpoint from a postgres:// (or
// postgresql://) URL — the password-free shape bd persists to metadata.json
// as postgres_dsn (`bd init --backend=postgres`). A missing port defaults to
// 5432. Query parameters (sslmode etc.) are bd's business and are ignored. A
// userinfo password is ignored: bd never persists one, and the password
// reaches bd via BEADS_PG_PASSWORD at command time.
//
// libpq's keyword/value form ("host=... port=...") is not supported. bd
// accepts it at init time, but gc requires the URL form to derive an endpoint
// deterministically, and fails with ErrPostgresDSNUnsupportedForm rather than
// deriving a partial one.
func ParsePostgresDSNEndpoint(dsn string) (PostgresEndpoint, error) {
	dsn = strings.TrimSpace(dsn)
	if dsn == "" {
		return PostgresEndpoint{}, errors.New("postgres_dsn is empty")
	}
	if isLibpqKeywordValueDSN(dsn) {
		return PostgresEndpoint{}, fmt.Errorf("%w: libpq keyword/value form is not supported", ErrPostgresDSNUnsupportedForm)
	}
	u, err := url.Parse(dsn)
	if err != nil {
		return PostgresEndpoint{}, fmt.Errorf("parse postgres_dsn: %w", err)
	}
	switch strings.ToLower(u.Scheme) {
	case "postgres", "postgresql":
	default:
		return PostgresEndpoint{}, fmt.Errorf("%w, got scheme %q", ErrPostgresDSNUnsupportedForm, u.Scheme)
	}
	host := u.Hostname()
	if host == "" {
		return PostgresEndpoint{}, errors.New("postgres_dsn has no host")
	}
	port := u.Port()
	if port == "" {
		port = "5432"
	}
	user := ""
	if u.User != nil {
		user = u.User.Username()
	}
	return PostgresEndpoint{
		Host:     host,
		Port:     port,
		User:     user,
		Database: strings.TrimPrefix(u.Path, "/"),
	}, nil
}

// isLibpqKeywordValueDSN reports whether dsn is libpq's keyword/value form:
// whitespace-separated key=value pairs with no URL scheme. Recognizing it
// explicitly lets the failure name the unsupported form instead of reporting
// a confusing empty scheme.
func isLibpqKeywordValueDSN(dsn string) bool {
	if strings.Contains(dsn, "://") {
		return false
	}
	fields := strings.Fields(dsn)
	if len(fields) == 0 {
		return false
	}
	for _, field := range fields {
		key, _, ok := strings.Cut(field, "=")
		if !ok || key == "" {
			return false
		}
	}
	return true
}

// PostgresEndpoint returns the effective endpoint for a postgres-backed
// scope. The discrete postgres_host/port/user/database fields win outright
// when all four are present; otherwise each missing field is filled from
// postgres_dsn. A scope with neither a DSN nor the complete discrete set is
// an error, as is one whose DSN does not parse.
//
// LoadMetadataState applies the same rules and additionally rejects an
// incomplete result, so a postgres state it returns always derives a full
// tuple here.
func (s MetadataState) PostgresEndpoint() (PostgresEndpoint, error) {
	ep := PostgresEndpoint{
		Host:     strings.TrimSpace(s.PostgresHost),
		Port:     strings.TrimSpace(s.PostgresPort),
		User:     strings.TrimSpace(s.PostgresUser),
		Database: strings.TrimSpace(s.PostgresDatabase),
	}
	if len(missingPostgresEndpointFields(ep)) == 0 {
		return ep, nil
	}
	dsn := strings.TrimSpace(s.PostgresDSN)
	if dsn == "" {
		return PostgresEndpoint{}, errors.New("postgres scope requires postgres_dsn or all of postgres_host, postgres_port, postgres_user, postgres_database")
	}
	derived, err := ParsePostgresDSNEndpoint(dsn)
	if err != nil {
		return PostgresEndpoint{}, err
	}
	if ep.Host == "" {
		ep.Host = derived.Host
	}
	if ep.Port == "" {
		ep.Port = derived.Port
	}
	if ep.User == "" {
		ep.User = derived.User
	}
	if ep.Database == "" {
		ep.Database = derived.Database
	}
	return ep, nil
}

// missingPostgresEndpointFields names the metadata fields an endpoint still
// lacks, in metadata declaration order, so rejections can tell an operator
// exactly what the DSN did not supply.
func missingPostgresEndpointFields(ep PostgresEndpoint) []string {
	var missing []string
	for _, field := range []struct {
		name  string
		value string
	}{
		{"postgres_host", ep.Host},
		{"postgres_port", ep.Port},
		{"postgres_user", ep.User},
		{"postgres_database", ep.Database},
	} {
		if field.value == "" {
			missing = append(missing, field.name)
		}
	}
	return missing
}
