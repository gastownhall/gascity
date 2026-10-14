package contract

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/gastownhall/gascity/internal/fsys"
	beadsbackend "github.com/steveyegge/beads/backend"
)

// registerLibraryBackendForTest registers name with the linked beads library's
// backend registry for the duration of the test, standing in for a build's
// distribution wiring (the enterprise build's bdhttp.Register). A name the
// build's own wiring already registered is left as it is: the library panics on
// a duplicate, and the distribution's registration is the one under test then.
func registerLibraryBackendForTest(t *testing.T, name string) {
	t.Helper()
	if beadsbackend.Registered(name) {
		return
	}
	refuse := func(context.Context, string) (beadsbackend.DoltStorage, error) {
		return nil, errors.New("test backend does not open")
	}
	beadsbackend.Register(name, beadsbackend.Backend{Open: refuse, OpenReadOnly: refuse})
	t.Cleanup(func() { beadsbackend.Deregister(name) })
}

func writeMetadataForTest(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "metadata.json")
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

// Without distribution wiring the linked library registers no extension
// backend, so a converted remote workspace is refused exactly as before.
func TestRecognizeBackendRefusesHTTPWithoutLibraryRegistration(t *testing.T) {
	if beadsbackend.Registered("http") {
		t.Skip("this build's beads library registers http; the OSS refusal does not apply")
	}
	err := RecognizeBackend("http")
	if !errors.Is(err, ErrUnknownBackend) {
		t.Fatalf("RecognizeBackend(http) error = %v, want ErrUnknownBackend", err)
	}
	want := `unsupported backend "http" (supported: dolt, doltlite); ` + BackendNotOpenedGuarantee
	if err.Error() != want {
		t.Fatalf("refusal = %q, want %q", err.Error(), want)
	}
	if IsLibraryExtensionBackend("http") {
		t.Fatal("IsLibraryExtensionBackend(http) = true without library registration")
	}
}

func TestRecognizeBackendAcceptsLibraryRegisteredExtension(t *testing.T) {
	const name = "http"
	if !beadsbackend.Registered(name) {
		if err := RecognizeBackend(name); !errors.Is(err, ErrUnknownBackend) {
			t.Fatalf("RecognizeBackend(%q) before registration error = %v, want ErrUnknownBackend", name, err)
		}
	}
	registerLibraryBackendForTest(t, name)

	if err := RecognizeBackend(name); err != nil {
		t.Fatalf("RecognizeBackend(%q) error = %v, want nil", name, err)
	}
	if !IsLibraryExtensionBackend(name) {
		t.Fatalf("IsLibraryExtensionBackend(%q) = false, want true", name)
	}
	// The enumeration names what gc itself implements; library-served names
	// never join it.
	names, err := RegisteredBackends()
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"dolt", "doltlite"}; !reflect.DeepEqual(names, want) {
		t.Fatalf("RegisteredBackends() = %v, want %v", names, want)
	}

	state, ok, err := LoadMetadataState(fsys.OSFS{}, writeMetadataForTest(t, `{"database":"dolt","backend":"`+name+`","dolt_database":"hq","project_id":"proj-1"}`))
	if err != nil || !ok {
		t.Fatalf("LoadMetadataState() = (%+v, %v, %v), want the workspace accepted", state, ok, err)
	}
	if state.Backend != name {
		t.Fatalf("LoadMetadataState().Backend = %q, want %q", state.Backend, name)
	}
}

// Registration with the library is necessary but not sufficient. A backend the
// library serves only beside gc's own preflight, fallback, and runtime — the
// postgres backend, whose existing workspaces carry storage fields and rely on
// the BdStore verdict — must stay exactly as unrecognized as it was before the
// library learned the name. Only names on the self-sufficient allowlist become
// extensions.
func TestLibraryRegisteredBackendOffTheAllowlistIsNotAnExtension(t *testing.T) {
	for _, name := range []string{"postgres", "gctest-ext"} {
		t.Run(name, func(t *testing.T) {
			registerLibraryBackendForTest(t, name)
			if IsLibraryExtensionBackend(name) {
				t.Fatalf("IsLibraryExtensionBackend(%q) = true for a backend off the self-sufficient allowlist", name)
			}
			if err := RecognizeBackend(name); !errors.Is(err, ErrUnknownBackend) {
				t.Fatalf("RecognizeBackend(%q) error = %v, want ErrUnknownBackend", name, err)
			}
		})
	}
}

// A name gc implements stays gc's even when a library also registers it: the
// compiled bundle wins, so gc keeps projecting its environment and managing
// its runtime.
func TestIsLibraryExtensionBackendExcludesCompiledNames(t *testing.T) {
	registerLibraryBackendForTest(t, "doltlite")
	for _, name := range []string{"", "dolt", "doltlite"} {
		if IsLibraryExtensionBackend(name) {
			t.Fatalf("IsLibraryExtensionBackend(%q) = true, want false for a name gc implements", name)
		}
	}
}

func TestRecognizeBackendStillRefusesUnknownWithLibraryExtensionsRegistered(t *testing.T) {
	registerLibraryBackendForTest(t, "http")
	err := RecognizeBackend("postgress")
	if !errors.Is(err, ErrUnknownBackend) {
		t.Fatalf("RecognizeBackend(postgress) error = %v, want ErrUnknownBackend", err)
	}
	if IsLibraryExtensionBackend("postgress") {
		t.Fatal("IsLibraryExtensionBackend(postgress) = true for an unregistered name")
	}
}
