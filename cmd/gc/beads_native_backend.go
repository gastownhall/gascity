package main

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/lib/pq"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/beads/contract"
	"github.com/gastownhall/gascity/internal/fsys"
	"github.com/gastownhall/gascity/internal/pgauth"
)

// initBeadsViaNativeBackend initializes a scope through the backend seam built
// into stock upstream bd. It deliberately bypasses the external-plugin setup
// hook: native backends have no endpoint process for Gas City to supervise.
func initBeadsViaNativeBackend(cityPath, dir, prefix string) (bool, error) {
	driver, packSQLitePath, ok := nativeBeadsBackendForCity(cityPath)
	if !ok {
		return false, nil
	}
	cfg, err := loadCityConfig(cityPath, io.Discard)
	if err != nil {
		return true, err
	}
	scope := beadsBackendScopeContextForCityScope(cityPath, dir, prefix, "")
	args := []string{
		"init",
		"--backend=" + driver,
		"--prefix=" + prefix,
		"--skip-hooks",
		"--skip-agents",
		"--init-if-missing",
		"--quiet",
	}
	env := nativeBackendInitEnv(cityPath, dir)
	metadataState := contract.MetadataState{Backend: driver}
	switch driver {
	case "sqlite":
		sqlitePath := strings.TrimSpace(cfg.Beads.SQLitePath)
		if sqlitePath == "" {
			sqlitePath = packSQLitePath
		}
		if sqlitePath != "" {
			args = append(args, "--sqlite-path="+sqlitePath)
		}
		metadataState.SQLitePath = sqlitePath
		if metadataState.SQLitePath == "" {
			metadataState.SQLitePath = "beads.db"
		}
	case "postgres":
		schema := strings.TrimSpace(cfg.Beads.PostgresSchema)
		if schema == "" || !samePath(cityPath, dir) {
			schema = strings.TrimSpace(scope.Namespace)
		}
		if schema == "" {
			schema = prefix
		}
		postgresURL := strings.TrimSpace(cfg.Beads.PostgresURL)
		if postgresURL == "" {
			postgresURL = pgauth.PostgresURL()
		}
		credentialScope := dir
		if postgresURL == "" && !samePath(cityPath, dir) {
			cityMeta, found, loadErr := contract.LoadMetadataState(
				fsys.OSFS{}, filepath.Join(cityPath, ".beads", "metadata.json"),
			)
			if loadErr != nil {
				return true, fmt.Errorf("load city postgres metadata: %w", loadErr)
			}
			if found && cityMeta.Backend == "postgres" {
				postgresURL = strings.TrimSpace(cityMeta.PostgresDSN)
				credentialScope = cityPath
			}
		}
		if postgresURL == "" {
			postgresURL, err = nativePostgresProvisioner(cityPath, schema)
			if err != nil {
				return true, err
			}
			credentialScope = cityPath
		}
		postgresURL, resolved, err := postgresInitURL(cityPath, credentialScope, postgresURL)
		if err != nil {
			return true, err
		}
		if resolved.Password == "" && !samePath(cityPath, dir) && !samePath(credentialScope, cityPath) {
			if cityURL, cityResolved, cityErr := postgresInitURL(cityPath, cityPath, postgresURL); cityErr == nil && cityResolved.Password != "" {
				postgresURL, resolved = cityURL, cityResolved
			}
		}
		if resolved.Password != "" {
			env["BEADS_PG_PASSWORD"] = resolved.Password
			env["BEADS_POSTGRES_PASSWORD"] = resolved.Password // compatibility with pre-1.1 bd builds
			if err := writeNativePostgresPassword(dir, resolved.Password); err != nil {
				return true, err
			}
		}
		args = append(args, "--pg-url="+postgresURL, "--pg-schema="+schema)
	default:
		return true, fmt.Errorf("unsupported native beads backend %q", driver)
	}

	runner := beads.ExecCommandRunnerWithEnv(env)
	if _, err := runner(dir, "bd", args...); err != nil {
		if isBdAlreadyInitializedError(err) {
			return true, nil
		}
		return true, fmt.Errorf("bd native %s init for %s: %w", driver, dir, err)
	}
	// gc may have pre-seeded Dolt connection fields before pack composition
	// selected the native backend. Upstream bd owns and writes the native
	// fields; this pass only removes stale fields belonging to other backends.
	if _, err := contract.EnsureCanonicalMetadata(
		fsys.OSFS{}, filepath.Join(dir, ".beads", "metadata.json"), metadataState,
	); err != nil {
		return true, fmt.Errorf("normalize native %s metadata for %s: %w", driver, dir, err)
	}
	return true, nil
}

var postgresIdentifierPattern = regexp.MustCompile(`^[a-z_][a-z0-9_]{0,62}$`)

var nativePostgresProvisioner = provisionLocalNativePostgres

func provisionLocalNativePostgres(cityPath, schema string) (string, error) {
	provisionCfg := pgauth.ProvisionConfigFromEnv()
	database := provisionCfg.Database
	if database == "" {
		database = "gc_" + filepath.Base(filepath.Clean(cityPath))
	}
	database = sanitizeNativePostgresIdentifier(database)
	user := provisionCfg.User
	if user == "" {
		user = database
	}
	user = sanitizeNativePostgresIdentifier(user)
	schema = sanitizeNativePostgresIdentifier(schema)
	host := provisionCfg.Host
	if host == "" {
		host = "127.0.0.1"
	}
	port := provisionCfg.Port
	if port == "" {
		port = "5432"
	}
	password := provisionCfg.Password
	if password == "" {
		var random [32]byte
		if _, err := rand.Read(random[:]); err != nil {
			return "", fmt.Errorf("generate native postgres password: %w", err)
		}
		password = base64.RawURLEncoding.EncodeToString(random[:])
	}

	adminURL := provisionCfg.AdminURL
	adminDSN := adminURL
	if adminDSN == "" {
		localDSN, dsnErr := localPostgresAdminDSN("postgres")
		if dsnErr != nil {
			return "", dsnErr
		}
		adminDSN = localDSN
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	adminDB, err := sql.Open("postgres", adminDSN)
	if err != nil {
		return "", fmt.Errorf("open local postgres admin connection: %w", err)
	}
	defer func() { _ = adminDB.Close() }()
	if err := adminDB.PingContext(ctx); err != nil {
		return "", fmt.Errorf("native postgres local provisioning requires local admin access (set GC_POSTGRES_ADMIN_URL for an explicit admin connection): %w", err)
	}
	var roleExists bool
	if err := adminDB.QueryRowContext(ctx, "SELECT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = $1)", user).Scan(&roleExists); err != nil {
		return "", fmt.Errorf("query native postgres role: %w", err)
	}
	roleSQL := "CREATE ROLE " + pq.QuoteIdentifier(user) + " LOGIN PASSWORD " + pq.QuoteLiteral(password)
	if roleExists {
		roleSQL = "ALTER ROLE " + pq.QuoteIdentifier(user) + " WITH LOGIN PASSWORD " + pq.QuoteLiteral(password)
	}
	if _, err := adminDB.ExecContext(ctx, roleSQL); err != nil {
		return "", fmt.Errorf("provision native postgres role %q: %w", user, err)
	}
	var databaseExists bool
	if err := adminDB.QueryRowContext(ctx, "SELECT EXISTS (SELECT 1 FROM pg_database WHERE datname = $1)", database).Scan(&databaseExists); err != nil {
		return "", fmt.Errorf("query native postgres database: %w", err)
	}
	if !databaseExists {
		createDatabase := "CREATE DATABASE " + pq.QuoteIdentifier(database) + " OWNER " + pq.QuoteIdentifier(user) + " TEMPLATE template0 LC_COLLATE 'C' LC_CTYPE 'C'"
		if _, err := adminDB.ExecContext(ctx, createDatabase); err != nil {
			return "", fmt.Errorf("provision native postgres database %q: %w", database, err)
		}
	} else if _, err := adminDB.ExecContext(ctx, "ALTER DATABASE "+pq.QuoteIdentifier(database)+" OWNER TO "+pq.QuoteIdentifier(user)); err != nil {
		return "", fmt.Errorf("adopt native postgres database %q: %w", database, err)
	}

	databaseAdminDSN, err := postgresAdminDatabaseURL(adminURL, database)
	if err != nil {
		return "", err
	}
	databaseAdmin, err := sql.Open("postgres", databaseAdminDSN)
	if err != nil {
		return "", fmt.Errorf("open provisioned native postgres database: %w", err)
	}
	defer func() { _ = databaseAdmin.Close() }()
	for _, statement := range []string{
		"CREATE SCHEMA IF NOT EXISTS " + pq.QuoteIdentifier(schema) + " AUTHORIZATION " + pq.QuoteIdentifier(user),
		"ALTER SCHEMA " + pq.QuoteIdentifier(schema) + " OWNER TO " + pq.QuoteIdentifier(user),
		"GRANT ALL PRIVILEGES ON DATABASE " + pq.QuoteIdentifier(database) + " TO " + pq.QuoteIdentifier(user),
		"GRANT USAGE, CREATE ON SCHEMA " + pq.QuoteIdentifier(schema) + " TO " + pq.QuoteIdentifier(user),
	} {
		if _, err := databaseAdmin.ExecContext(ctx, statement); err != nil {
			return "", fmt.Errorf("provision native postgres schema %q: %w", schema, err)
		}
	}
	if err := writeNativePostgresPassword(cityPath, password); err != nil {
		return "", err
	}
	connection := &url.URL{
		Scheme:   "postgres",
		User:     url.User(user),
		Host:     net.JoinHostPort(host, port),
		Path:     "/" + database,
		RawQuery: "sslmode=disable",
	}
	return connection.String(), nil
}

func postgresAdminDatabaseURL(adminURL, database string) (string, error) {
	if adminURL == "" {
		return localPostgresAdminDSN(database)
	}
	parsed, err := url.Parse(adminURL)
	if err != nil {
		return "", fmt.Errorf("parse postgres admin URL: %w", err)
	}
	if parsed.Scheme == "" {
		return adminURL + " dbname=" + database, nil
	}
	parsed.Path = "/" + database
	return parsed.String(), nil
}

func localPostgresAdminDSN(database string) (string, error) {
	for _, socketDir := range []string{"/var/run/postgresql", "/tmp"} {
		if _, err := os.Stat(filepath.Join(socketDir, ".s.PGSQL.5432")); err == nil {
			return "host=" + socketDir + " dbname=" + database + " sslmode=disable", nil
		}
	}
	return "", errors.New("native postgres local provisioning could not find a local PostgreSQL socket; set GC_POSTGRES_ADMIN_URL for an explicit admin connection")
}

func sanitizeNativePostgresIdentifier(raw string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(strings.TrimSpace(raw)) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '_':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	value := strings.Trim(b.String(), "_")
	for strings.Contains(value, "__") {
		value = strings.ReplaceAll(value, "__", "_")
	}
	if value == "" {
		value = "beads"
	}
	if value[0] >= '0' && value[0] <= '9' {
		value = "beads_" + value
	}
	if len(value) > 63 {
		value = value[:63]
	}
	if !postgresIdentifierPattern.MatchString(value) {
		return "beads"
	}
	return value
}

func writeNativePostgresPassword(scopeRoot, password string) error {
	beadsDir := filepath.Join(scopeRoot, ".beads")
	if err := os.MkdirAll(beadsDir, 0o700); err != nil {
		return fmt.Errorf("create native postgres credential directory: %w", err)
	}
	quoted := "'" + strings.ReplaceAll(password, "'", `'"'"'`) + "'"
	if err := os.WriteFile(filepath.Join(beadsDir, ".env"), []byte("BEADS_PG_PASSWORD="+quoted+"\n"), 0o600); err != nil {
		return fmt.Errorf("write native postgres credentials: %w", err)
	}
	if err := os.Chmod(filepath.Join(beadsDir, ".env"), 0o600); err != nil {
		return fmt.Errorf("protect native postgres credentials: %w", err)
	}
	gitignorePath := filepath.Join(beadsDir, ".gitignore")
	gitignore, _ := os.ReadFile(gitignorePath)
	if !strings.Contains("\n"+string(gitignore)+"\n", "\n.env\n") {
		if len(gitignore) > 0 && gitignore[len(gitignore)-1] != '\n' {
			gitignore = append(gitignore, '\n')
		}
		gitignore = append(gitignore, []byte(".env\n")...)
		if err := os.WriteFile(gitignorePath, gitignore, 0o600); err != nil {
			return fmt.Errorf("ignore native postgres credentials: %w", err)
		}
	}
	return nil
}

func nativeBackendInitEnv(cityPath, scopeRoot string) map[string]string {
	env := cityRuntimeEnvMapForCity(cityPath)
	env["BEADS_DIR"] = filepath.Join(scopeRoot, ".beads")
	env["BD_EXPORT_AUTO"] = "false"
	applyBdContributorRoutingOptOut(env)
	applyBdCLIRemoteSyncOptOut(env)
	applyBdAutoBackupOptOut(env)
	return env
}

func postgresInitURL(cityPath, scopeRoot, raw string) (string, pgauth.Resolved, error) {
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Scheme != "postgres" && parsed.Scheme != "postgresql" {
		return "", pgauth.Resolved{}, fmt.Errorf("invalid native postgres URL: %q", raw)
	}
	if parsed.User == nil {
		return "", pgauth.Resolved{}, fmt.Errorf("native postgres URL must include a username")
	}
	if _, hasPassword := parsed.User.Password(); hasPassword {
		return raw, pgauth.Resolved{User: parsed.User.Username()}, nil
	}
	host := parsed.Hostname()
	port := parsed.Port()
	if port == "" {
		port = "5432"
	}
	resolved, resolveErr := pgauth.ResolveFromEnv(nil, scopeRoot, pgauth.Endpoint{
		Host: host,
		Port: port,
		User: parsed.User.Username(),
	})
	if resolveErr != nil {
		// Passwordless/trust and driver-native authentication remain valid for
		// upstream bd. Only fail when a configured credential source is invalid.
		if errors.Is(resolveErr, pgauth.ErrNoPasswordResolvable) {
			return raw, pgauth.Resolved{User: parsed.User.Username()}, nil
		}
		return "", pgauth.Resolved{}, fmt.Errorf("resolving native postgres credentials: %w", resolveErr)
	}
	parsed.User = url.UserPassword(parsed.User.Username(), resolved.Password)
	if resolved.Password != "" {
		emitPostgresCredentialResolved(cityPath, scopeRoot, contract.MetadataState{
			Backend:          "postgres",
			PostgresHost:     host,
			PostgresPort:     port,
			PostgresUser:     parsed.User.Username(),
			PostgresDatabase: strings.TrimPrefix(parsed.Path, "/"),
		}, resolved.Source)
	}
	return parsed.String(), resolved, nil
}
