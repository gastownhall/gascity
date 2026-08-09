package contract

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/fsys"
)

// These tests pin gc to bd origin/main's postgres metadata contract:
// `bd init --backend=postgres` persists a password-free postgres_dsn plus a
// per-workspace postgres_schema (beads internal/configfile/configfile.go),
// NOT the draft-era discrete postgres_host/port/user/database fields. The
// password never lands on disk; bd reads BEADS_PG_PASSWORD at command time.

func TestParsePostgresDSNEndpoint(t *testing.T) {
	cases := []struct {
		name    string
		dsn     string
		want    PostgresEndpoint
		wantErr string
	}{
		{
			name: "full url",
			dsn:  "postgres://gc_city@pg.example.test:5432/gascity_infra",
			want: PostgresEndpoint{Host: "pg.example.test", Port: "5432", User: "gc_city", Database: "gascity_infra"},
		},
		{
			name: "postgresql scheme",
			dsn:  "postgresql://gc_city@pg.example.test:5432/gascity_infra",
			want: PostgresEndpoint{Host: "pg.example.test", Port: "5432", User: "gc_city", Database: "gascity_infra"},
		},
		{
			name: "non-default port",
			dsn:  "postgres://bd@db.example.test:6543/beads",
			want: PostgresEndpoint{Host: "db.example.test", Port: "6543", User: "bd", Database: "beads"},
		},
		{
			name: "missing port defaults to 5432",
			dsn:  "postgres://bd@db.example.test/beads",
			want: PostgresEndpoint{Host: "db.example.test", Port: "5432", User: "bd", Database: "beads"},
		},
		{
			name: "missing user",
			dsn:  "postgres://db.example.test:6543/beads",
			want: PostgresEndpoint{Host: "db.example.test", Port: "6543", User: "", Database: "beads"},
		},
		{
			name: "missing database",
			dsn:  "postgres://bd@db.example.test:5432",
			want: PostgresEndpoint{Host: "db.example.test", Port: "5432", User: "bd", Database: ""},
		},
		{
			name: "ipv6 host with port",
			dsn:  "postgres://bd@[2001:db8::1]:6543/beads",
			want: PostgresEndpoint{Host: "2001:db8::1", Port: "6543", User: "bd", Database: "beads"},
		},
		{
			name: "ipv6 host without port defaults to 5432",
			dsn:  "postgres://bd@[::1]/beads",
			want: PostgresEndpoint{Host: "::1", Port: "5432", User: "bd", Database: "beads"},
		},
		{
			name: "percent-encoded user is decoded",
			dsn:  "postgres://gc%40city@db.example.test:5432/beads",
			want: PostgresEndpoint{Host: "db.example.test", Port: "5432", User: "gc@city", Database: "beads"},
		},
		{
			name: "percent-encoded database is decoded",
			dsn:  "postgres://bd@db.example.test:5432/gas%20city",
			want: PostgresEndpoint{Host: "db.example.test", Port: "5432", User: "bd", Database: "gas city"},
		},
		{
			name: "query params are ignored",
			dsn:  "postgres://bd@db.example.test:6543/beads?sslmode=require&connect_timeout=5",
			want: PostgresEndpoint{Host: "db.example.test", Port: "6543", User: "bd", Database: "beads"},
		},
		{
			name: "userinfo password is ignored (never persisted by bd)",
			dsn:  "postgres://bd:sneaky@db.example.test:5432/beads",
			want: PostgresEndpoint{Host: "db.example.test", Port: "5432", User: "bd", Database: "beads"},
		},
		{
			name:    "empty dsn",
			dsn:     "",
			wantErr: "postgres_dsn is empty",
		},
		{
			name:    "non-postgres scheme",
			dsn:     "mysql://bd@db.example.test:3306/beads",
			wantErr: "postgres_dsn must be a postgres:// (or postgresql://) URL",
		},
		{
			name:    "no host",
			dsn:     "postgres:///beads",
			wantErr: "postgres_dsn has no host",
		},
		{
			name:    "libpq keyword/value form is not a URL",
			dsn:     "host=db.example.test port=5432 user=bd dbname=beads",
			wantErr: "postgres_dsn must be a postgres:// (or postgresql://) URL",
		},
		{
			name:    "non-numeric port",
			dsn:     "postgres://bd@db.example.test:notaport/beads",
			wantErr: "port",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ParsePostgresDSNEndpoint(tc.dsn)
			if tc.wantErr != "" {
				if err == nil {
					t.Fatalf("ParsePostgresDSNEndpoint(%q) error = nil, want substring %q (got %+v)", tc.dsn, tc.wantErr, got)
				}
				if !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("ParsePostgresDSNEndpoint(%q) error = %q, want substring %q", tc.dsn, err.Error(), tc.wantErr)
				}
				if got != (PostgresEndpoint{}) {
					t.Fatalf("ParsePostgresDSNEndpoint(%q) = %+v on error, want the zero endpoint (no partial derivation)", tc.dsn, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("ParsePostgresDSNEndpoint(%q) error = %v, want nil", tc.dsn, err)
			}
			if got != tc.want {
				t.Fatalf("ParsePostgresDSNEndpoint(%q) = %+v, want %+v", tc.dsn, got, tc.want)
			}
		})
	}
}

// TestParsePostgresDSNEndpointRejectsLibpqKeywordValue pins the one shape bd
// accepts at init time but gc cannot derive an endpoint from. It must fail by
// name rather than silently deriving a wrong or partial endpoint.
func TestParsePostgresDSNEndpointRejectsLibpqKeywordValue(t *testing.T) {
	const dsn = "host=db.example.test port=6543 user=bd dbname=beads sslmode=require"
	got, err := ParsePostgresDSNEndpoint(dsn)
	if err == nil {
		t.Fatalf("ParsePostgresDSNEndpoint(%q) = %+v, want an error", dsn, got)
	}
	if !errors.Is(err, ErrPostgresDSNUnsupportedForm) {
		t.Fatalf("error %v, want errors.Is(err, ErrPostgresDSNUnsupportedForm)", err)
	}
	if got != (PostgresEndpoint{}) {
		t.Fatalf("ParsePostgresDSNEndpoint(%q) = %+v, want the zero endpoint", dsn, got)
	}
	// The failure must name the unsupported form so an operator knows what to
	// change, not just that "something" is wrong.
	for _, want := range []string{"keyword/value", "postgres://"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error = %q, want substring %q", err.Error(), want)
		}
	}
}

func TestMetadataStatePostgresEndpoint(t *testing.T) {
	cases := []struct {
		name    string
		state   MetadataState
		want    PostgresEndpoint
		wantErr string
	}{
		{
			name: "draft shape: discrete fields pass through untouched",
			state: MetadataState{
				Backend:          "postgres",
				PostgresHost:     "db.example.test",
				PostgresPort:     "5432",
				PostgresUser:     "bd",
				PostgresDatabase: "beads",
			},
			want: PostgresEndpoint{Host: "db.example.test", Port: "5432", User: "bd", Database: "beads"},
		},
		{
			name: "complete discrete fields win over a DSN pointing elsewhere",
			state: MetadataState{
				Backend:          "postgres",
				PostgresDSN:      "postgres://other@elsewhere.test:9999/otherdb",
				PostgresHost:     "db.example.test",
				PostgresPort:     "5432",
				PostgresUser:     "bd",
				PostgresDatabase: "beads",
			},
			want: PostgresEndpoint{Host: "db.example.test", Port: "5432", User: "bd", Database: "beads"},
		},
		{
			name: "complete discrete fields win even when the DSN is unparseable",
			state: MetadataState{
				Backend:          "postgres",
				PostgresDSN:      "host=elsewhere.test port=9999",
				PostgresHost:     "db.example.test",
				PostgresPort:     "5432",
				PostgresUser:     "bd",
				PostgresDatabase: "beads",
			},
			want: PostgresEndpoint{Host: "db.example.test", Port: "5432", User: "bd", Database: "beads"},
		},
		{
			name: "bd-main shape: endpoint derived from postgres_dsn",
			state: MetadataState{
				Backend:        "postgres",
				PostgresDSN:    "postgres://gc_city@pg.example.test:6543/gascity_infra",
				PostgresSchema: "city_x",
			},
			want: PostgresEndpoint{Host: "pg.example.test", Port: "6543", User: "gc_city", Database: "gascity_infra"},
		},
		{
			name: "incomplete discrete set falls back to the DSN for the missing fields",
			state: MetadataState{
				Backend:      "postgres",
				PostgresDSN:  "postgres://gc_city@pg.example.test:5432/gascity_infra",
				PostgresHost: "override.example.test",
			},
			want: PostgresEndpoint{Host: "override.example.test", Port: "5432", User: "gc_city", Database: "gascity_infra"},
		},
		{
			name:    "neither DSN nor discrete fields",
			state:   MetadataState{Backend: "postgres"},
			wantErr: "postgres scope requires postgres_dsn or all of postgres_host, postgres_port, postgres_user, postgres_database",
		},
		{
			name: "incomplete discrete set with an unparseable DSN surfaces the parse error",
			state: MetadataState{
				Backend:      "postgres",
				PostgresDSN:  "host=db.example.test port=5432",
				PostgresHost: "db.example.test",
			},
			wantErr: "keyword/value",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := tc.state.PostgresEndpoint()
			if tc.wantErr != "" {
				if err == nil {
					t.Fatalf("PostgresEndpoint() error = nil, want substring %q (got %+v)", tc.wantErr, got)
				}
				if !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("PostgresEndpoint() error = %q, want substring %q", err.Error(), tc.wantErr)
				}
				if got != (PostgresEndpoint{}) {
					t.Fatalf("PostgresEndpoint() = %+v on error, want the zero endpoint (no partial derivation)", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("PostgresEndpoint() error = %v, want nil", err)
			}
			if got != tc.want {
				t.Fatalf("PostgresEndpoint() = %+v, want %+v", got, tc.want)
			}
		})
	}
}

func TestLoadMetadataStateAcceptsBdMainPostgresDSNShape(t *testing.T) {
	fs := fsys.OSFS{}
	path, _ := copyMetadataFixture(t, fs, "valid_postgres_dsn.json")
	got, ok, err := LoadMetadataState(fs, path)
	if err != nil {
		t.Fatalf("LoadMetadataState(valid_postgres_dsn.json) error = %v, want nil", err)
	}
	if !ok {
		t.Fatalf("LoadMetadataState(valid_postgres_dsn.json) ok = false, want true")
	}
	want := MetadataState{
		Database:       "beads",
		Backend:        "postgres",
		PostgresDSN:    "postgres://gc_city@pg.example.test:6543/gascity_infra",
		PostgresSchema: "city_x",
	}
	if got != want {
		t.Fatalf("LoadMetadataState(valid_postgres_dsn.json) = %+v, want %+v", got, want)
	}
	endpoint, err := got.PostgresEndpoint()
	if err != nil {
		t.Fatalf("PostgresEndpoint() error = %v, want nil (LoadMetadataState guarantees derivable states)", err)
	}
	wantEndpoint := PostgresEndpoint{Host: "pg.example.test", Port: "6543", User: "gc_city", Database: "gascity_infra"}
	if endpoint != wantEndpoint {
		t.Fatalf("PostgresEndpoint() = %+v, want %+v", endpoint, wantEndpoint)
	}
}

func TestLoadMetadataStateRejectsDoltWithPostgresDSN(t *testing.T) {
	fs := fsys.OSFS{}
	path, _ := copyMetadataFixture(t, fs, "reject_dolt_with_postgres_dsn.json")
	_, ok, err := LoadMetadataState(fs, path)
	if err == nil || ok {
		t.Fatalf("LoadMetadataState(reject_dolt_with_postgres_dsn.json) = ok=%v err=%v, want mixed-backend rejection", ok, err)
	}
	var parseErr *MetadataParseError
	if !errors.As(err, &parseErr) {
		t.Fatalf("error %T = %v, want *MetadataParseError", err, err)
	}
	want := "cannot mix dolt and postgres fields in a single scope (backend=dolt but postgres_dsn is also set)"
	if !strings.Contains(parseErr.Reason, want) {
		t.Fatalf("Reason = %q, want substring %q", parseErr.Reason, want)
	}
}

// TestLoadMetadataStateRejectsUnparseablePostgresDSN is the load-boundary half
// of the libpq limitation: a scope gc cannot derive an endpoint for is refused
// outright, so no consumer ever sees a half-populated MetadataState.
func TestLoadMetadataStateRejectsUnparseablePostgresDSN(t *testing.T) {
	fs := fsys.OSFS{}
	path, _ := copyMetadataFixture(t, fs, "reject_pg_dsn_libpq_form.json")
	_, ok, err := LoadMetadataState(fs, path)
	if err == nil || ok {
		t.Fatalf("LoadMetadataState(reject_pg_dsn_libpq_form.json) = ok=%v err=%v, want rejection", ok, err)
	}
	var parseErr *MetadataParseError
	if !errors.As(err, &parseErr) {
		t.Fatalf("error %T = %v, want *MetadataParseError", err, err)
	}
	for _, want := range []string{"postgres_dsn", "keyword/value"} {
		if !strings.Contains(parseErr.Reason, want) {
			t.Errorf("Reason = %q, want substring %q", parseErr.Reason, want)
		}
	}
}

// TestLoadMetadataStateRejectsIncompletePostgresDSN keeps MetadataState's
// documented invariant true across the widening: a postgres state handed to a
// consumer always derives a complete host/port/user/database tuple, so the
// BEADS_POSTGRES_* projection can never go out with empty values.
func TestLoadMetadataStateRejectsIncompletePostgresDSN(t *testing.T) {
	fs := fsys.OSFS{}
	path, _ := copyMetadataFixture(t, fs, "reject_pg_dsn_no_database.json")
	_, ok, err := LoadMetadataState(fs, path)
	if err == nil || ok {
		t.Fatalf("LoadMetadataState(reject_pg_dsn_no_database.json) = ok=%v err=%v, want rejection", ok, err)
	}
	var parseErr *MetadataParseError
	if !errors.As(err, &parseErr) {
		t.Fatalf("error %T = %v, want *MetadataParseError", err, err)
	}
	if !strings.Contains(parseErr.Reason, "postgres_database") {
		t.Fatalf("Reason = %q, want it to name the missing postgres_database", parseErr.Reason)
	}
}

func TestLoadMetadataStateRejectsPostgresDSNPortOutOfRange(t *testing.T) {
	fs := fsys.OSFS{}
	path := writeTempMetadata(t, `{"database":"beads","backend":"postgres","postgres_dsn":"postgres://bd@db.example.test:99999/beads"}`)
	_, ok, err := LoadMetadataState(fs, path)
	if err == nil || ok {
		t.Fatalf("LoadMetadataState = ok=%v err=%v, want port-range rejection", ok, err)
	}
	var parseErr *MetadataParseError
	if !errors.As(err, &parseErr) {
		t.Fatalf("error %T = %v, want *MetadataParseError", err, err)
	}
	if !strings.Contains(parseErr.Reason, "postgres_port must be a TCP port (1..65535), got \"99999\"") {
		t.Fatalf("Reason = %q, want the TCP-port rejection naming the DSN-derived port", parseErr.Reason)
	}
}

func TestEnsureCanonicalMetadataWritesPostgresDSNAndSchema(t *testing.T) {
	fs := fsys.OSFS{}
	path := writeTempMetadata(t, `{"database":"beads","backend":"postgres"}`)
	changed, err := EnsureCanonicalMetadata(fs, path, MetadataState{
		Database:       "beads",
		Backend:        "postgres",
		PostgresDSN:    "postgres://gc_city@pg.example.test:5432/gascity_infra",
		PostgresSchema: "city_x",
	})
	if err != nil {
		t.Fatalf("EnsureCanonicalMetadata: %v", err)
	}
	if !changed {
		t.Fatalf("EnsureCanonicalMetadata changed = false, want true")
	}
	state, ok, err := LoadMetadataState(fs, path)
	if err != nil || !ok {
		t.Fatalf("LoadMetadataState after canonicalise: ok=%v err=%v", ok, err)
	}
	if state.PostgresDSN != "postgres://gc_city@pg.example.test:5432/gascity_infra" {
		t.Errorf("postgres_dsn = %q, want persisted DSN", state.PostgresDSN)
	}
	if state.PostgresSchema != "city_x" {
		t.Errorf("postgres_schema = %q, want city_x", state.PostgresSchema)
	}
}

func TestEnsureCanonicalMetadataScrubsPostgresDSNOnDoltCanonicalise(t *testing.T) {
	fs := fsys.OSFS{}
	path := writeTempMetadata(t, `{"database":"dolt","backend":"dolt","dolt_mode":"server","dolt_database":"hq","postgres_dsn":"postgres://bd@db.example.test:5432/beads","postgres_schema":"city_x"}`)
	changed, err := EnsureCanonicalMetadata(fs, path, MetadataState{
		Database:     "dolt",
		Backend:      "dolt",
		DoltMode:     "server",
		DoltDatabase: "hq",
	})
	if err != nil {
		t.Fatalf("EnsureCanonicalMetadata: %v", err)
	}
	if !changed {
		t.Fatalf("EnsureCanonicalMetadata changed = false, want true (postgres_dsn/postgres_schema must be scrubbed)")
	}
	data, err := fs.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"postgres_dsn", "postgres_schema"} {
		if strings.Contains(string(data), key) {
			t.Errorf("dolt canonicalise left %q in metadata.json: %s", key, data)
		}
	}
}

func TestEnsureCanonicalMetadataByteIdenticalForCanonicalDSNScope(t *testing.T) {
	fs := fsys.OSFS{}
	path := writeTempMetadata(t, `{"backend":"postgres","database":"beads","postgres_dsn":"postgres://gc_city@pg.example.test:5432/gascity_infra","postgres_schema":"city_x"}`)
	state, ok, err := LoadMetadataState(fs, path)
	if err != nil || !ok {
		t.Fatalf("LoadMetadataState: ok=%v err=%v", ok, err)
	}
	before, err := fs.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	changed, err := EnsureCanonicalMetadata(fs, path, state)
	if err != nil {
		t.Fatalf("EnsureCanonicalMetadata: %v", err)
	}
	if changed {
		t.Fatalf("EnsureCanonicalMetadata changed = true, want false (round-trip must be a no-op)")
	}
	after, err := fs.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != string(after) {
		t.Fatalf("metadata.json rewritten on no-op round-trip:\nbefore: %s\nafter:  %s", before, after)
	}
}

func writeTempMetadata(t *testing.T, contents string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "metadata.json")
	if err := (fsys.OSFS{}).WriteFile(path, []byte(contents), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}
