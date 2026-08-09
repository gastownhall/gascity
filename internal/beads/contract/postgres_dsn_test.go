package contract

import (
	"encoding/json"
	"errors"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/fsys"
)

// These tests pin gc to bd's postgres metadata contract. The Postgres add-on's
// `bd init --backend=postgres --pg-url --pg-schema` persists a password-free
// postgres_dsn plus a per-workspace postgres_schema — see bd-enterprise
// cmd/bd/backend_init_distribution_enterprise.go (runInitPostgres) for the
// write and internal/storage/postgres/fromconfig.go for the read — NOT the
// draft-era discrete postgres_host/port/user/database fields, which no bd has
// ever written. pgdialect.RedactPassword keeps the password off disk
// fail-closed; bd re-supplies it at command time from
// BEADS_PG_PASSWORD_COMMAND or BEADS_PG_PASSWORD. OSS bd retains both keys as
// deprecated round-trip storage and refuses to open backend=postgres, so gc
// must preserve the shape it finds rather than force a hand-conversion.

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
			// The rejection deliberately does NOT name the port: url.Parse's
			// own "invalid port" message quotes the raw DSN, so the reason is
			// the static unsupported-form sentinel instead. See
			// TestParsePostgresDSNEndpointErrorsNeverEchoTheDSN.
			name:    "non-numeric port",
			dsn:     "postgres://bd@db.example.test:notaport/beads",
			wantErr: "not a parseable URL",
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

// TestLoadMetadataStateToleratesDoltScopeWithPostgresResidue pins the one
// asymmetry in the mixed-backend guard. postgres_dsn/postgres_schema are bd's
// own fields: bd retains them across a re-init, so flipping a Postgres
// workspace to Dolt leaves them behind on disk with no operator involvement.
// A declared backend=dolt is unambiguous and the residue is inert, so the
// scope must still load — gc scrubs the keys on the next canonicalise instead
// of bricking every command until a human hand-edits the file. The discrete
// postgres_host/port/user/database fields have no such producer and stay
// fatal (TestLoadMetadataStateRejectsDoltWithPostgresField).
func TestLoadMetadataStateToleratesDoltScopeWithPostgresResidue(t *testing.T) {
	fs := fsys.OSFS{}
	path, _ := copyMetadataFixture(t, fs, "dolt_with_postgres_residue.json")
	got, ok, err := LoadMetadataState(fs, path)
	if err != nil || !ok {
		t.Fatalf("LoadMetadataState(dolt_with_postgres_residue.json) = ok=%v err=%v, want the scope to load", ok, err)
	}
	if got.Backend != "dolt" || got.DoltDatabase != "hq" {
		t.Fatalf("state = %+v, want the dolt binding preserved", got)
	}

	// The residue must survive into the state so canonicalisation can see and
	// scrub it, and EnsureCanonicalMetadata must actually remove it.
	if got.PostgresDSN == "" || got.PostgresSchema == "" {
		t.Fatalf("state = %+v, want the postgres residue carried through for scrubbing", got)
	}
	if _, err := EnsureCanonicalMetadata(fs, path, MetadataState{
		Database:     got.Database,
		Backend:      "dolt",
		DoltMode:     got.DoltMode,
		DoltDatabase: got.DoltDatabase,
	}); err != nil {
		t.Fatalf("EnsureCanonicalMetadata: %v", err)
	}
	data, err := fs.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"postgres_dsn", "postgres_schema"} {
		if strings.Contains(string(data), key) {
			t.Errorf("canonicalise left %q behind: %s", key, data)
		}
	}
	if _, ok, err := LoadMetadataState(fs, path); err != nil || !ok {
		t.Fatalf("LoadMetadataState after canonicalise = ok=%v err=%v, want a clean load", ok, err)
	}
}

// TestLoadMetadataStateRejectsDoltWithPostgresField keeps the mixed-backend
// guard fatal for the discrete fields, which are gc's own and have no
// legitimate producer on a dolt scope.
func TestLoadMetadataStateRejectsDoltWithPostgresField(t *testing.T) {
	fs := fsys.OSFS{}
	path, _ := copyMetadataFixture(t, fs, "reject_dolt_with_postgres_field.json")
	_, ok, err := LoadMetadataState(fs, path)
	if err == nil || ok {
		t.Fatalf("LoadMetadataState(reject_dolt_with_postgres_field.json) = ok=%v err=%v, want mixed-backend rejection", ok, err)
	}
	var parseErr *MetadataParseError
	if !errors.As(err, &parseErr) {
		t.Fatalf("error %T = %v, want *MetadataParseError", err, err)
	}
	if !strings.Contains(parseErr.Reason, "cannot mix dolt and postgres fields") {
		t.Fatalf("Reason = %q, want the mixed-backend rejection", parseErr.Reason)
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

// leakyDSNCases are the malformed DSNs whose parse failure is most likely to
// carry a credential: url.Parse fails on exactly the bytes ('%', '/', control
// characters, brackets) that generated passwords contain, so the failure
// correlates with the secret being present. secret is a fragment that must not
// survive into any error text — checking a fragment rather than the whole
// password catches the partial echoes ("invalid URL escape \"%or\"",
// "invalid port \":sword\"") that unwrapping to url.Error.Err would leave.
var leakyDSNCases = []struct {
	name   string
	dsn    string
	secret string
}{
	{
		name:   "percent in password",
		dsn:    "postgres://operator:sw%ordfish@db.example.com:5432/gascity",
		secret: "ordfish",
	},
	{
		name:   "slash in password",
		dsn:    "postgres://operator:sword/fish@db.example.com:5432/gascity",
		secret: "sword",
	},
	{
		name:   "control character in password",
		dsn:    "postgres://operator:sword\x7ffish@db.example.com:5432/gascity",
		secret: "sword",
	},
	{
		name:   "unbalanced bracket in host",
		dsn:    "postgres://operator:swordfish@[db.example.com:5432/gascity",
		secret: "swordfish",
	},
	{
		name:   "non-numeric port",
		dsn:    "postgres://operator:swordfish@db.example.com:notaport/gascity",
		secret: "swordfish",
	},
}

// TestParsePostgresDSNEndpointErrorsNeverEchoTheDSN pins the invariant at the
// point of failure: no error ParsePostgresDSNEndpoint returns may quote the
// DSN. net/url's *url.Error renders as `%s %q: %s` with the raw input, so
// wrapping it with %w publishes any hand-written password verbatim.
func TestParsePostgresDSNEndpointErrorsNeverEchoTheDSN(t *testing.T) {
	for _, tc := range leakyDSNCases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ParsePostgresDSNEndpoint(tc.dsn)
			if err == nil {
				t.Fatalf("ParsePostgresDSNEndpoint(%q) error = nil, want a rejection", tc.dsn)
			}
			assertDSNFree(t, err.Error(), tc.dsn, tc.secret)
		})
	}
}

// TestLoadMetadataStateErrorNeverEchoesPostgresDSN is the load-path twin of
// TestPreflightFailsContractShapeOnUnderivablePostgresDSN's leak assertion.
// The preflight summary is a diagnostic surface; this is the one every gc
// command traverses to build a bd subprocess environment, and its error text
// reaches stderr, `gc --json` failure payloads, and captured CI logs.
func TestLoadMetadataStateErrorNeverEchoesPostgresDSN(t *testing.T) {
	for _, tc := range leakyDSNCases {
		t.Run(tc.name, func(t *testing.T) {
			raw, err := json.Marshal(map[string]string{
				"database":     "beads",
				"backend":      "postgres",
				"postgres_dsn": tc.dsn,
			})
			if err != nil {
				t.Fatal(err)
			}
			path := writeTempMetadata(t, string(raw))
			_, ok, err := LoadMetadataState(fsys.OSFS{}, path)
			if err == nil || ok {
				t.Fatalf("LoadMetadataState = ok=%v err=%v, want a rejection", ok, err)
			}
			assertDSNFree(t, err.Error(), tc.dsn, tc.secret)

			var parseErr *MetadataParseError
			if !errors.As(err, &parseErr) {
				t.Fatalf("error %T = %v, want *MetadataParseError", err, err)
			}
			assertDSNFree(t, parseErr.Reason, tc.dsn, tc.secret)
			// The rejection still has to be actionable without the DSN.
			if !strings.Contains(parseErr.Reason, "postgres_dsn") {
				t.Errorf("Reason = %q, want it to name postgres_dsn as the offending field", parseErr.Reason)
			}
		})
	}
}

// dsnWithUserinfoRe matches a postgres URL carrying userinfo. Naming the
// required form ("must be a postgres:// URL") is fine; echoing a concrete URL
// that reached the `user:password@host` stage is not, and catches the leak
// class even for fixtures whose password this test does not know.
var dsnWithUserinfoRe = regexp.MustCompile(`postgres(ql)?://[^\s"]*@`)

// assertDSNFree fails when a message quotes the DSN or any fragment of its
// password.
func assertDSNFree(t *testing.T, msg, dsn, secret string) {
	t.Helper()
	if strings.Contains(msg, dsn) {
		t.Errorf("message echoes the raw postgres_dsn: %q", msg)
	}
	if strings.Contains(msg, secret) {
		t.Errorf("message leaks password fragment %q: %q", secret, msg)
	}
	if dsnWithUserinfoRe.MatchString(msg) {
		t.Errorf("message quotes a postgres URL carrying userinfo: %q", msg)
	}
}

// TestParsePostgresDSNEndpointRejectsAmbiguousHosts covers the DSN shapes
// url.Parse accepts but splits wrongly. libpq's multi-host form and an
// unbracketed IPv6 literal both yield a host that is not a host, and every
// downstream gate passes it: it is projected as BEADS_POSTGRES_HOST and keyed
// into credential lookups. Refusing beats connecting somewhere unintended.
func TestParsePostgresDSNEndpointRejectsAmbiguousHosts(t *testing.T) {
	cases := []struct {
		name string
		dsn  string
	}{
		{name: "multi-host with ports", dsn: "postgres://bd@host1.example.test:5432,host2.example.test:5432/beads"},
		{name: "multi-host without ports", dsn: "postgres://bd@host1.example.test,host2.example.test/beads"},
		{name: "unbracketed ipv6 with port", dsn: "postgres://bd@2001:db8::1:5432/beads"},
		{name: "unbracketed ipv6 loopback", dsn: "postgres://bd@::1/beads"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ParsePostgresDSNEndpoint(tc.dsn)
			if err == nil {
				t.Fatalf("ParsePostgresDSNEndpoint(%q) = %+v, want a rejection (host is ambiguous)", tc.dsn, got)
			}
			if !errors.Is(err, ErrPostgresDSNUnsupportedForm) {
				t.Fatalf("error %v, want errors.Is(err, ErrPostgresDSNUnsupportedForm)", err)
			}
			if got != (PostgresEndpoint{}) {
				t.Fatalf("ParsePostgresDSNEndpoint(%q) = %+v, want the zero endpoint", tc.dsn, got)
			}
		})
	}
}

// TestPreflightContractShapeAgreesWithLoadMetadataState pins preflight to the
// load gate. contract_shape reporting PASS for a DSN LoadMetadataState refuses
// is worse than no check at all: preflight exists to tell an operator whether
// gc can open the scope, so a PASS that precedes a hard load failure is a lie.
func TestPreflightContractShapeAgreesWithLoadMetadataState(t *testing.T) {
	cases := []struct {
		name string
		dsn  string
		want PreflightCheckState
	}{
		{name: "derivable", dsn: "postgres://gc_city@pg.example.test:6543/gascity_infra", want: PreflightCheckPass},
		{name: "no database", dsn: "postgres://bd@db.example.test:5432", want: PreflightCheckFail},
		{name: "no user", dsn: "postgres://db.example.test:5432/beads", want: PreflightCheckFail},
		{name: "port out of range", dsn: "postgres://bd@db.example.test:99999/beads", want: PreflightCheckFail},
		{name: "libpq keyword/value", dsn: "host=db.example.test port=5432 user=bd dbname=beads", want: PreflightCheckFail},
		{name: "multi-host", dsn: "postgres://bd@host1.example.test:5432,host2.example.test:5432/beads", want: PreflightCheckFail},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := PreflightChecker{}.checkContractShape(preflightMetadata{
				Backend:     "postgres",
				PostgresDSN: tc.dsn,
			})
			if got.State != tc.want {
				t.Errorf("checkContractShape state = %v, want %v (summary %q)", got.State, tc.want, got.Summary)
			}

			raw, err := json.Marshal(map[string]string{
				"database":     "beads",
				"backend":      "postgres",
				"postgres_dsn": tc.dsn,
			})
			if err != nil {
				t.Fatal(err)
			}
			path := writeTempMetadata(t, string(raw))
			_, ok, loadErr := LoadMetadataState(fsys.OSFS{}, path)
			loads := ok && loadErr == nil
			if loads != (tc.want == PreflightCheckPass) {
				t.Errorf("LoadMetadataState loads = %v but contract_shape wants %v — preflight and the load gate disagree (load err %v)", loads, tc.want, loadErr)
			}
		})
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
