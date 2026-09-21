package proxyendpoint

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/gastownhall/gascity/internal/beads/contract"
	"github.com/gastownhall/gascity/internal/fsys"
)

// writeRecord writes rec into root the way bd does.
func writeRecord(t *testing.T, root string, rec Record) {
	t.Helper()
	data, err := json.Marshal(rec)
	if err != nil {
		t.Fatalf("marshal record: %v", err)
	}
	if err := os.WriteFile(PIDPath(root), data, 0o644); err != nil {
		t.Fatalf("write %s: %v", PIDPath(root), err)
	}
}

// validRecord is a record that passes every field check for root.
func validRecord(t *testing.T, root string) Record {
	t.Helper()
	id, err := RootID(root)
	if err != nil {
		t.Fatalf("RootID(%s): %v", root, err)
	}
	return Record{
		PID:         4242,
		Port:        45123,
		UpstreamID:  "abc",
		Schema:      SchemaV2,
		Kind:        RecordKind,
		Birth:       BirthToken("boot-1", "99887766"),
		RootID:      id,
		ControlPort: 45124,
	}
}

func TestReadDecodesEveryFieldBdWrites(t *testing.T) {
	root := t.TempDir()
	want := validRecord(t, root)
	writeRecord(t, root, want)

	got, err := Read(root)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if got != want {
		t.Fatalf("Read() = %+v, want %+v — a field this package drops is a field gc cannot use to identify a generation", got, want)
	}
}

func TestReadAbsentAndMalformed(t *testing.T) {
	t.Run("absent is ErrNoProxy", func(t *testing.T) {
		if _, err := Read(t.TempDir()); !errors.Is(err, ErrNoProxy) {
			t.Fatalf("Read on an empty root = %v, want ErrNoProxy — bd removes the record on an orderly exit, which is not a fault", err)
		}
	})
	t.Run("malformed is ErrMalformed", func(t *testing.T) {
		root := t.TempDir()
		if err := os.WriteFile(PIDPath(root), []byte("{not json"), 0o644); err != nil {
			t.Fatal(err)
		}
		if _, err := Read(root); !errors.Is(err, ErrMalformed) {
			t.Fatalf("Read on a truncated record = %v, want ErrMalformed", err)
		}
	})
}

func TestValidateFieldTable(t *testing.T) {
	root := t.TempDir()
	base := validRecord(t, root)

	cases := []struct {
		name   string
		mutate func(*Record)
		want   error
		field  string
	}{
		{name: "valid", mutate: func(*Record) {}},
		{
			name:   "schema 1 is a legacy proxy",
			mutate: func(r *Record) { r.Schema = 1 },
			want:   ErrLegacyProxy,
			field:  "schema",
		},
		{
			name:   "schema 0 is a legacy proxy",
			mutate: func(r *Record) { r.Schema = 0 },
			want:   ErrLegacyProxy,
			field:  "schema",
		},
		{
			name:   "the dolt-backend record is not the proxy's",
			mutate: func(r *Record) { r.Kind = "dolt-backend" },
			want:   ErrNotOurs,
			field:  "kind",
		},
		{
			name:   "pid 0",
			mutate: func(r *Record) { r.PID = 0 },
			want:   ErrNotOurs,
			field:  "pid",
		},
		{
			name:   "negative pid",
			mutate: func(r *Record) { r.PID = -1 },
			want:   ErrNotOurs,
			field:  "pid",
		},
		{
			name:   "port 0",
			mutate: func(r *Record) { r.Port = 0 },
			want:   ErrNotOurs,
			field:  "port",
		},
		{
			name:   "port 70000",
			mutate: func(r *Record) { r.Port = 70000 },
			want:   ErrNotOurs,
			field:  "port",
		},
		{
			name:   "control port out of range",
			mutate: func(r *Record) { r.ControlPort = 70000 },
			want:   ErrNotOurs,
			field:  "control_port",
		},
		{
			// bd's own struct tags control_port omitempty, so a record written
			// before it existed carries none. That is not a foreign record.
			name:   "absent control port is allowed",
			mutate: func(r *Record) { r.ControlPort = 0 },
		},
		{
			name:   "empty birth",
			mutate: func(r *Record) { r.Birth = "" },
			want:   ErrNotOurs,
			field:  "birth",
		},
		{
			name:   "another root's id",
			mutate: func(r *Record) { r.RootID = "00ff" },
			want:   ErrNotOurs,
			field:  "root_id",
		},
		{
			// "absent" is not "mine". gc has no IDENT exchange to fall back on,
			// so a record with no root identity has no proof behind it.
			name:   "absent root id",
			mutate: func(r *Record) { r.RootID = "" },
			want:   ErrNotOurs,
			field:  "root_id",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := base
			tc.mutate(&rec)
			err := Validate(rec, root)
			if tc.want == nil {
				if err != nil {
					t.Fatalf("Validate() = %v, want nil", err)
				}
				return
			}
			if !errors.Is(err, tc.want) {
				t.Fatalf("Validate() = %v, want %v", err, tc.want)
			}
			var fieldErr *FieldError
			if !errors.As(err, &fieldErr) {
				t.Fatalf("Validate() = %v, want a *FieldError naming the field that disagreed", err)
			}
			if fieldErr.Field != tc.field {
				t.Fatalf("Validate() named field %q, want %q", fieldErr.Field, tc.field)
			}
		})
	}
}

// TestRootIDIsSpellingIndependent pins the property the pool key rests on: two
// spellings of one directory are one root, and two directories are never one
// root however similarly they are named.
func TestRootIDIsSpellingIndependent(t *testing.T) {
	base := t.TempDir()
	resolved := filepath.Join(base, "resolved")
	if err := os.MkdirAll(resolved, 0o755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(base, "link")
	if err := os.Symlink(resolved, link); err != nil {
		t.Fatal(err)
	}

	direct, err := RootID(resolved)
	if err != nil {
		t.Fatalf("RootID(resolved): %v", err)
	}
	viaLink, err := RootID(link)
	if err != nil {
		t.Fatalf("RootID(link): %v", err)
	}
	viaDots, err := RootID(filepath.Join(resolved, "..", "resolved"))
	if err != nil {
		t.Fatalf("RootID(resolved/../resolved): %v", err)
	}
	if direct != viaLink || direct != viaDots {
		t.Fatalf("RootID disagreed across spellings of one root: %s / %s / %s", direct, viaLink, viaDots)
	}

	sibling := filepath.Join(base, "sibling")
	if err := os.MkdirAll(sibling, 0o755); err != nil {
		t.Fatal(err)
	}
	siblingID, err := RootID(sibling)
	if err != nil {
		t.Fatalf("RootID(sibling): %v", err)
	}
	if siblingID == direct {
		t.Fatal("two directories produced one root id")
	}
}

// TestValidateRefusesACopiedRecord is the foreign-root case stated directly: the
// record is bd's, valid, and describes a live proxy — of somebody else's root.
func TestValidateRefusesACopiedRecord(t *testing.T) {
	base := t.TempDir()
	rootA := filepath.Join(base, "a")
	rootB := filepath.Join(base, "b")
	for _, dir := range []string{rootA, rootB} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	recA := validRecord(t, rootA)
	writeRecord(t, rootA, recA)
	writeRecord(t, rootB, recA)

	if err := Validate(recA, rootA); err != nil {
		t.Fatalf("Validate against its own root = %v, want nil", err)
	}
	err := Validate(recA, rootB)
	if !errors.Is(err, ErrNotOurs) {
		t.Fatalf("Validate of A's record against root B = %v, want ErrNotOurs", err)
	}
	var fieldErr *FieldError
	if errors.As(err, &fieldErr) && fieldErr.Field != "root_id" {
		t.Fatalf("copied record refused on field %q, want root_id", fieldErr.Field)
	}
}

// TestValidateAcceptsASymlinkedRootSpelling pins that the root-identity check
// does not turn a symlinked workspace into a foreign one: bd resolves symlinks
// when it computes the id, so a caller who reached the root through a link must
// still recognize its own proxy.
func TestValidateAcceptsASymlinkedRootSpelling(t *testing.T) {
	base := t.TempDir()
	resolved := filepath.Join(base, "resolved")
	if err := os.MkdirAll(resolved, 0o755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(base, "link")
	if err := os.Symlink(resolved, link); err != nil {
		t.Fatal(err)
	}
	rec := validRecord(t, resolved)
	writeRecord(t, resolved, rec)

	if err := Validate(rec, link); err != nil {
		t.Fatalf("Validate through a symlinked spelling = %v, want nil", err)
	}
}

// TestReadOwnershipToleratesFieldsItDoesNotRead pins the protect-side read
// against the shapes a later bd could publish.
//
// Every row here is a document the strict reader refuses, and the point of each
// is that refusing it would unprotect a live proxy over a field the ownership
// question never consults. encoding/json fails the whole decode on a type
// mismatch in any tagged field, so this is not a hypothetical: bd documents its
// birth token as platform-specific, and promoting it to an object is an ordinary
// thing for a minor release to do.
func TestReadOwnershipToleratesFieldsItDoesNotRead(t *testing.T) {
	cases := []struct {
		name   string
		body   string
		wantID int
		// strictRefuses says the strict reader must reject this same document,
		// which is what makes the two questions genuinely different.
		strictRefuses bool
	}{
		{
			name:   "the record bd 1.3.0 writes",
			body:   `{"pid":7701,"port":35425,"upstream_id":"u","schema":2,"kind":"db-proxy","birth":"linux-v1:boot:1","root_id":"abc","control_port":46445}`,
			wantID: 7701,
		},
		{
			name:          "birth promoted to an object",
			body:          `{"pid":7701,"port":35425,"schema":3,"kind":"db-proxy","birth":{"boot_id":"b","starttime":1}}`,
			wantID:        7701,
			strictRefuses: true,
		},
		{
			name:          "schema written as a string",
			body:          `{"pid":7701,"port":35425,"schema":"2","kind":"db-proxy"}`,
			wantID:        7701,
			strictRefuses: true,
		},
		{
			name:          "control_port written as a string",
			body:          `{"pid":7701,"port":35425,"schema":2,"kind":"db-proxy","control_port":"46445"}`,
			wantID:        7701,
			strictRefuses: true,
		},
		{
			name:          "upstream_id written as a number",
			body:          `{"pid":7701,"port":35425,"schema":2,"kind":"db-proxy","upstream_id":42}`,
			wantID:        7701,
			strictRefuses: true,
		},
		{
			name:   "fields this version has never heard of",
			body:   `{"pid":7701,"schema":4,"kind":"db-proxy","lease":{"epoch":3},"tags":["a","b"]}`,
			wantID: 7701,
		},
		{
			name:          "a pid written as a string still names a process",
			body:          `{"pid":"7701","schema":2,"kind":"db-proxy"}`,
			wantID:        7701,
			strictRefuses: true,
		},
		{
			// No pid is no process to check an argv against, so there is
			// nothing to protect and nothing to guess.
			name:          "a pid gc cannot read at all",
			body:          `{"pid":{"value":7701},"schema":2,"kind":"db-proxy"}`,
			strictRefuses: true,
		},
		{
			// Without a readable kind gc cannot tell the proxy's own record
			// from its Dolt child's.
			name:          "a kind gc cannot read at all",
			body:          `{"pid":7701,"schema":2,"kind":{"name":"db-proxy"}}`,
			strictRefuses: true,
		},
		{
			name:          "not JSON at all",
			body:          "not json",
			strictRefuses: true,
		},
		{
			name:          "a JSON document that is not an object",
			body:          `[{"pid":7701,"kind":"db-proxy"}]`,
			strictRefuses: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			if err := os.WriteFile(PIDPath(dir), []byte(tc.body), 0o600); err != nil {
				t.Fatal(err)
			}
			own, err := ReadOwnership(dir)
			switch {
			case tc.wantID == 0 && err == nil:
				t.Fatalf("ReadOwnership accepted %s", tc.body)
			case tc.wantID == 0:
				// Nothing more to assert: the reaper protects nothing here.
			case err != nil:
				t.Fatalf("ReadOwnership(%s) = %v; a field the ownership question never reads must not unprotect a live proxy", tc.body, err)
			case own.PID != tc.wantID || own.Kind != RecordKind:
				t.Fatalf("ReadOwnership = %+v, want pid %d kind %s", own, tc.wantID, RecordKind)
			}
			_, strictErr := Read(dir)
			if tc.strictRefuses && strictErr == nil {
				t.Fatalf("the strict reader accepted %s; this row's premise is gone", tc.body)
			}
			if !tc.strictRefuses && strictErr != nil {
				t.Fatalf("the strict reader refused %s: %v", tc.body, strictErr)
			}
		})
	}

	t.Run("an absent record is still ErrNoProxy", func(t *testing.T) {
		_, err := ReadOwnership(t.TempDir())
		if !errors.Is(err, ErrNoProxy) {
			t.Fatalf("ReadOwnership with no record = %v, want ErrNoProxy", err)
		}
	})
}

// TestProviderRootMirrorsBdDoltDirResolution states the whole resolution as one
// table, because every arm of it is a directory gc would look for proxy.pid in.
//
// The rows that matter most are the metadata ones. `<scope>/.beads/dolt` is not
// merely a default with exotic overrides: bd resolves a scope's Dolt data
// directory through BEADS_DOLT_DATA_DIR and metadata.json's dolt_data_dir first
// (internal/doltserver/physical_root.go), and gc's own `gc beads city migrate
// proxied` WRITES dolt_data_dir on every rig so the rig's proxy roots at the
// city's data dir. A reader that hardcoded the default answered a directory bd
// never publishes into for exactly the scopes gc created.
func TestProviderRootMirrorsBdDoltDirResolution(t *testing.T) {
	cases := []struct {
		name string
		// metadata and sidecar are written into the scope's .beads directory
		// when non-empty.
		metadata string
		sidecar  string
		// env is applied for the duration of the case.
		env map[string]string
		// want is the root, given the scope and its .beads directory.
		want func(scope, beadsDir string) string
	}{
		{
			name: "nothing overrides: the scope's own dolt directory",
			want: func(_, beadsDir string) string { return filepath.Join(beadsDir, DefaultRootDirName) },
		},
		{
			name:     "metadata with no dolt_data_dir is still the default",
			metadata: `{"database":"beads.db","dolt_mode":"proxied-server"}`,
			want:     func(_, beadsDir string) string { return filepath.Join(beadsDir, DefaultRootDirName) },
		},
		{
			name:     "malformed metadata is not a root claim",
			metadata: "not json",
			want:     func(_, beadsDir string) string { return filepath.Join(beadsDir, DefaultRootDirName) },
		},
		{
			name:     "a relative dolt_data_dir joins .beads, as bd joins it",
			metadata: `{"dolt_mode":"proxied-server","dolt_data_dir":"elsewhere/dolt"}`,
			want:     func(_, beadsDir string) string { return filepath.Join(beadsDir, "elsewhere", "dolt") },
		},
		{
			name:     "an absolute dolt_data_dir stands as written",
			metadata: `{"dolt_mode":"proxied-server","dolt_data_dir":"/srv/dolt-data"}`,
			want:     func(_, _ string) string { return filepath.FromSlash("/srv/dolt-data") },
		},
		{
			name:     "BEADS_DOLT_DATA_DIR wins over metadata",
			metadata: `{"dolt_mode":"proxied-server","dolt_data_dir":"elsewhere/dolt"}`,
			env:      map[string]string{DoltDataDirEnv: "env-data"},
			want:     func(_, beadsDir string) string { return filepath.Join(beadsDir, "env-data") },
		},
		{
			name:     "the sidecar's root_path wins over metadata",
			metadata: `{"dolt_mode":"proxied-server","dolt_data_dir":"elsewhere/dolt"}`,
			sidecar:  `{"root_path":"sidecar/dolt"}`,
			want:     func(_, beadsDir string) string { return filepath.Join(beadsDir, "sidecar", "dolt") },
		},
		{
			// R1-F3: bd joins a relative BEADS_PROXIED_SERVER_ROOT_PATH to the
			// scope's .beads directory. Resolving it against the reader's
			// working directory would name a path that depends on where gc was
			// invoked from.
			name:    "a relative BEADS_PROXIED_SERVER_ROOT_PATH joins .beads, not the working directory",
			sidecar: `{"root_path":"sidecar/dolt"}`,
			env:     map[string]string{RootPathEnv: "proxyroot"},
			want:    func(_, beadsDir string) string { return filepath.Join(beadsDir, "proxyroot") },
		},
		{
			name:     "shared-server mode roots at the shared dolt directory",
			metadata: `{"dolt_mode":"proxied-server","dolt_data_dir":"elsewhere/dolt"}`,
			env:      map[string]string{SharedServerModeEnv: "1", SharedServerDirEnv: "/srv/shared-server"},
			want:     func(_, _ string) string { return filepath.FromSlash("/srv/shared-server/dolt") },
		},
		{
			name:    "an explicit sidecar root_path still wins over shared-server mode",
			sidecar: `{"root_path":"/srv/pinned/dolt"}`,
			env:     map[string]string{SharedServerModeEnv: "true", SharedServerDirEnv: "/srv/shared-server"},
			want:    func(_, _ string) string { return filepath.FromSlash("/srv/pinned/dolt") },
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			scope := t.TempDir()
			beadsDir := filepath.Join(scope, ".beads")
			if err := os.MkdirAll(beadsDir, 0o755); err != nil {
				t.Fatal(err)
			}
			if tc.metadata != "" {
				if err := os.WriteFile(filepath.Join(beadsDir, MetadataFileName), []byte(tc.metadata), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			if tc.sidecar != "" {
				writeSidecar(t, beadsDir, tc.sidecar)
			}
			for name, value := range tc.env {
				t.Setenv(name, value)
			}
			got, err := ProviderRoot(scope)
			if err != nil {
				t.Fatalf("ProviderRoot: %v", err)
			}
			if want := tc.want(scope, beadsDir); got != want {
				t.Fatalf("ProviderRoot = %q, want %q", got, want)
			}
		})
	}
}

// TestProviderRootFollowsARigOntoItsCitysRoot is the shape gc's own migration
// creates, spelled out end to end.
//
// `gc beads city migrate proxied` records a RELATIVE dolt_data_dir on each rig
// pointing at the city's Dolt root, because beads drops an absolute one on save.
// bd then roots that rig's proxy in the city's directory, where exactly one
// proxy.pid lives for both scopes, and the pool key distinguishes them by
// database alone. Resolving the rig to its own .beads/dolt would report
// no_record for every rig in a migrated city.
func TestProviderRootFollowsARigOntoItsCitysRoot(t *testing.T) {
	base := t.TempDir()
	cityBeads := filepath.Join(base, "city", ".beads")
	rig := filepath.Join(base, "city", "rigs", "alpha")
	rigBeads := filepath.Join(rig, ".beads")
	cityRoot := filepath.Join(cityBeads, DefaultRootDirName)
	if err := os.MkdirAll(cityRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(rigBeads, 0o755); err != nil {
		t.Fatal(err)
	}
	relative, err := filepath.Rel(rigBeads, cityRoot)
	if err != nil {
		t.Fatal(err)
	}
	if err := contract.SetMetadataDoltDataDir(fsys.OSFS{}, filepath.Join(rigBeads, MetadataFileName), relative); err == nil {
		t.Fatal("SetMetadataDoltDataDir wrote into a scope with no metadata.json")
	}
	if err := os.WriteFile(filepath.Join(rigBeads, MetadataFileName), []byte(`{"database":"beads.db","dolt_mode":"proxied-server"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := contract.SetMetadataDoltDataDir(fsys.OSFS{}, filepath.Join(rigBeads, MetadataFileName), relative); err != nil {
		t.Fatalf("record the shared root on the rig: %v", err)
	}

	got, err := ProviderRoot(rig)
	if err != nil {
		t.Fatalf("ProviderRoot: %v", err)
	}
	if got != cityRoot {
		t.Fatalf("ProviderRoot(rig) = %q, want the city's root %q — a shared-root rig's proxy.pid lives in the city's directory", got, cityRoot)
	}
}

func TestProviderRootPrecedence(t *testing.T) {
	scope := t.TempDir()
	beadsDir := filepath.Join(scope, ".beads")
	if err := os.MkdirAll(beadsDir, 0o755); err != nil {
		t.Fatal(err)
	}

	t.Run("default when nothing overrides", func(t *testing.T) {
		got, err := ProviderRoot(scope)
		if err != nil {
			t.Fatalf("ProviderRoot: %v", err)
		}
		want := filepath.Join(beadsDir, DefaultRootDirName)
		if got != want {
			t.Fatalf("ProviderRoot = %q, want %q", got, want)
		}
	})

	t.Run("sidecar root_path wins over the default", func(t *testing.T) {
		elsewhere := filepath.Join(t.TempDir(), "proxyroot")
		writeSidecar(t, beadsDir, `{"root_path":`+quote(elsewhere)+`}`)
		got, err := ProviderRoot(scope)
		if err != nil {
			t.Fatalf("ProviderRoot: %v", err)
		}
		if got != elsewhere {
			t.Fatalf("ProviderRoot = %q, want the sidecar's %q", got, elsewhere)
		}
	})

	t.Run("a relative sidecar root_path resolves against .beads", func(t *testing.T) {
		writeSidecar(t, beadsDir, `{"root_path":"custom/dolt"}`)
		got, err := ProviderRoot(scope)
		if err != nil {
			t.Fatalf("ProviderRoot: %v", err)
		}
		want := filepath.Join(beadsDir, "custom", "dolt")
		if got != want {
			t.Fatalf("ProviderRoot = %q, want %q", got, want)
		}
	})

	t.Run("the environment wins over the sidecar", func(t *testing.T) {
		writeSidecar(t, beadsDir, `{"root_path":"custom/dolt"}`)
		override := filepath.Join(t.TempDir(), "env-root")
		t.Setenv(RootPathEnv, override)
		got, err := ProviderRoot(scope)
		if err != nil {
			t.Fatalf("ProviderRoot: %v", err)
		}
		if got != override {
			t.Fatalf("ProviderRoot = %q, want the environment's %q — bd reads it first, so a root gc resolved differently is a root nothing writes", got, override)
		}
	})
}

// quote renders a path as a JSON string.
func quote(s string) string {
	out, err := json.Marshal(s)
	if err != nil {
		panic(err)
	}
	return string(out)
}
