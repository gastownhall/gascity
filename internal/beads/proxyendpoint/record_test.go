package proxyendpoint

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
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
