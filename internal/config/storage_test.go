package config

import (
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/coordclass"
)

func TestParseStorageAbsentSynthesizesReservedWorkBinding(t *testing.T) {
	cfg, err := Parse([]byte(`
[workspace]
name = "existing-city"
`))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if cfg.Storage != nil {
		t.Fatalf("Storage = %#v, want nil for an existing city with no [storage]", cfg.Storage)
	}

	effective := cfg.EffectiveStorage()
	for _, class := range storageConfigClassOrder() {
		if got := effective.Classes.BindingFor(class); got != StorageWorkBinding {
			t.Errorf("BindingFor(%s) = %q, want %q", class, got, StorageWorkBinding)
		}
	}
	if len(effective.Bindings) != 0 {
		t.Fatalf("effective Bindings = %#v, want no explicit definitions", effective.Bindings)
	}

	data, err := cfg.Marshal()
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if strings.Contains(string(data), "[storage") {
		t.Fatalf("Marshal introduced [storage] into an existing city:\n%s", data)
	}
}

func TestParseStorageSQLiteSplitRoundTrips(t *testing.T) {
	const input = `
[workspace]
name = "sqlite-city"

[storage.classes]
work = "work"
graph = "infra"
sessions = "infra"
messaging = "infra"
orders = "infra"
nudges = "infra"

[storage.bindings.infra]
provider = "sqlite-beads"
path = ".gc/store"
`

	cfg, err := Parse([]byte(input))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if cfg.Storage == nil {
		t.Fatal("Storage = nil, want authored storage config")
	}
	want := StorageConfig{
		Classes: StorageClasses{
			Work:      "work",
			Graph:     "infra",
			Sessions:  "infra",
			Messaging: "infra",
			Orders:    "infra",
			Nudges:    "infra",
		},
		Bindings: map[string]StorageBindingConfig{
			"infra": {
				Provider: StorageProviderSQLiteBeads,
				Path:     DefaultSQLiteStoragePath,
			},
		},
	}
	if got := cfg.EffectiveStorage(); !got.Equal(want) {
		t.Fatalf("EffectiveStorage() = %#v, want %#v", got, want)
	}

	data, err := cfg.Marshal()
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	roundTripped, err := Parse(data)
	if err != nil {
		t.Fatalf("Parse(Marshal): %v\n%s", err, data)
	}
	if got := roundTripped.EffectiveStorage(); !got.Equal(want) {
		t.Fatalf("round-tripped EffectiveStorage() = %#v, want %#v", got, want)
	}
}

func TestEffectiveStorageNormalizesSQLiteDefaultPathAndClonesBindings(t *testing.T) {
	cfg, err := Parse([]byte(`
[storage.classes]
work = "work"
graph = "infra"
sessions = "infra"
messaging = "infra"
orders = "infra"
nudges = "infra"

[storage.bindings.infra]
provider = "sqlite-beads"
`))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}

	effective := cfg.EffectiveStorage()
	if got := effective.Bindings["infra"].Path; got != DefaultSQLiteStoragePath {
		t.Fatalf("effective sqlite path = %q, want %q", got, DefaultSQLiteStoragePath)
	}
	effective.Bindings["infra"] = StorageBindingConfig{Provider: "changed", ConfigRef: "changed"}
	if got := cfg.Storage.Bindings["infra"].Provider; got != StorageProviderSQLiteBeads {
		t.Fatalf("mutating effective config changed authored config provider to %q", got)
	}

	clone := cfg.Storage.Clone()
	clone.Bindings["infra"] = StorageBindingConfig{Provider: "changed", ConfigRef: "changed"}
	if got := cfg.Storage.Bindings["infra"].Provider; got != StorageProviderSQLiteBeads {
		t.Fatalf("mutating Clone changed source config provider to %q", got)
	}
}

func TestStorageReloadRequiresRestartComparesEffectiveConfiguration(t *testing.T) {
	explicitAllWork := &City{Storage: &StorageConfig{Classes: defaultStorageConfig().Classes}}
	if StorageReloadRequiresRestart(&City{}, explicitAllWork) {
		t.Fatal("omitted storage and explicit all-work assignments should be reload-equivalent")
	}

	implicitSQLitePath := &City{Storage: &StorageConfig{
		Classes: StorageClasses{
			Work:      "work",
			Graph:     "infra",
			Sessions:  "infra",
			Messaging: "infra",
			Orders:    "infra",
			Nudges:    "infra",
		},
		Bindings: map[string]StorageBindingConfig{
			"infra": {Provider: StorageProviderSQLiteBeads},
		},
	}}
	explicitSQLitePath := &City{Storage: &StorageConfig{
		Classes: implicitSQLitePath.Storage.Classes,
		Bindings: map[string]StorageBindingConfig{
			"infra": {Provider: StorageProviderSQLiteBeads, Path: DefaultSQLiteStoragePath},
		},
	}}
	if StorageReloadRequiresRestart(implicitSQLitePath, explicitSQLitePath) {
		t.Fatal("omitted and explicit default SQLite paths should be reload-equivalent")
	}

	changed := explicitSQLitePath.Storage.Clone()
	changed.Bindings["infra"] = StorageBindingConfig{Provider: StorageProviderSQLiteBeads, Path: ".gc/other"}
	if !StorageReloadRequiresRestart(explicitSQLitePath, &City{Storage: &changed}) {
		t.Fatal("changed SQLite provider configuration should require restart")
	}
}

func TestValidateStorageBindingsSupportsCompiledGoRustAndMixedAssignments(t *testing.T) {
	const input = `
[storage.classes]
work = "tasks"
graph = "infra-go"
sessions = "infra-rust"
messaging = "infra-rust"
orders = "work"
nudges = "infra-go"

[storage.bindings.tasks]
provider = "test-go-provider"
config_ref = "city-work"

[storage.bindings.infra-go]
provider = "test-go-provider"
config_ref = "city-graph"

[storage.bindings.infra-rust]
provider = "test-rust-provider"
config_ref = "city-sessions"
`
	cfg, err := Parse([]byte(input))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}

	seen := make(map[string][]StorageClass)
	err = ValidateStorageBindings(cfg, func(name string, binding StorageBindingConfig, assigned []StorageClass) error {
		switch binding.Provider {
		case "test-go-provider", "test-rust-provider":
		default:
			return fmt.Errorf("unknown storage provider: %s", binding.Provider)
		}
		seen[name] = append([]StorageClass(nil), assigned...)
		return nil
	})
	if err != nil {
		t.Fatalf("ValidateStorageBindings: %v", err)
	}

	wantClasses := map[string][]StorageClass{
		"tasks":      {StorageClassWork},
		"infra-go":   {StorageClassGraph, StorageClassNudges},
		"infra-rust": {StorageClassSessions, StorageClassMessaging},
	}
	if len(seen) != len(wantClasses) {
		t.Fatalf("validated bindings = %#v, want %d", seen, len(wantClasses))
	}
	for binding, classes := range wantClasses {
		if got := seen[binding]; !reflect.DeepEqual(got, classes) {
			t.Errorf("classes for %q = %v, want %v", binding, got, classes)
		}
	}
}

func TestValidateStorageBindingsRejectsProviderAndCapabilityFailures(t *testing.T) {
	const input = `
[storage.classes]
work = "tasks"
graph = "tasks"
sessions = "work"
messaging = "work"
orders = "work"
nudges = "work"

[storage.bindings.tasks]
provider = "test-go-provider"
config_ref = "city-work"
`
	cfg, err := Parse([]byte(input))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}

	tests := []struct {
		name     string
		validate func(string, StorageBindingConfig, []StorageClass) error
	}{
		{
			name: "unknown compiled provider",
			validate: func(_ string, binding StorageBindingConfig, _ []StorageClass) error {
				return fmt.Errorf("unknown compiled provider %q", binding.Provider)
			},
		},
		{
			name: "provider owned configuration",
			validate: func(_ string, binding StorageBindingConfig, _ []StorageClass) error {
				return fmt.Errorf("provider rejected config_ref %q", binding.ConfigRef)
			},
		},
		{
			name: "unsupported class capability",
			validate: func(_ string, _ StorageBindingConfig, assigned []StorageClass) error {
				return fmt.Errorf("provider does not support assigned classes %v", assigned)
			},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			testErr := errors.New("provider validation failed")
			err := ValidateStorageBindings(cfg, func(name string, binding StorageBindingConfig, assigned []StorageClass) error {
				providerErr := tc.validate(name, binding, assigned)
				if providerErr == nil {
					return nil
				}
				return fmt.Errorf("%w: %w", testErr, providerErr)
			})
			if !errors.Is(err, testErr) {
				t.Fatalf("ValidateStorageBindings error = %v, want wrapped provider error", err)
			}
			if !strings.Contains(err.Error(), `storage binding "tasks"`) {
				t.Fatalf("ValidateStorageBindings error = %v, want binding context", err)
			}
		})
	}
}

func TestParseStorageRejectsInvalidAuthoring(t *testing.T) {
	completeClasses := `
[storage.classes]
work = "work"
graph = "work"
sessions = "work"
messaging = "work"
orders = "work"
nudges = "work"
`
	tests := []struct {
		name    string
		input   string
		wantErr string
	}{
		{
			name: "unknown class",
			input: `
[storage.classes]
work = "work"
graph = "work"
sessions = "work"
messaging = "work"
orders = "work"
nudges = "work"
artifacts = "work"
`,
			wantErr: `unknown storage class "artifacts"`,
		},
		{
			name: "invalid binding name",
			input: `
[storage.classes]
work = "work"
graph = "in fra"
sessions = "work"
messaging = "work"
orders = "work"
nudges = "work"
`,
			wantErr: `storage.classes.graph: binding name has invalid characters`,
		},
		{
			name: "reserved work definition",
			input: completeClasses + `
[storage.bindings.work]
provider = "sqlite-beads"
`,
			wantErr: `storage.bindings.work is reserved`,
		},
		{
			name: "missing provider",
			input: strings.Replace(completeClasses, `graph = "work"`, `graph = "infra"`, 1) + `
[storage.bindings.infra]
path = ".gc/store"
`,
			wantErr: `provider ID is empty`,
		},
		{
			name: "non builtin requires config ref",
			input: strings.Replace(completeClasses, `work = "work"`, `work = "tasks"`, 1) + `
[storage.bindings.tasks]
provider = "test-go-provider"
`,
			wantErr: `config_ref is required`,
		},
		{
			name: "non builtin rejects path",
			input: strings.Replace(completeClasses, `work = "work"`, `work = "tasks"`, 1) + `
[storage.bindings.tasks]
provider = "test-go-provider"
path = ".gc/not-provider-owned"
`,
			wantErr: `path is only supported by provider "sqlite-beads"`,
		},
		{
			name: "sqlite rejects config ref",
			input: strings.Replace(completeClasses, `graph = "work"`, `graph = "infra"`, 1) + `
[storage.bindings.infra]
provider = "sqlite-beads"
config_ref = "city-infra"
`,
			wantErr: `provider "sqlite-beads" does not accept config_ref`,
		},
		{
			name: "path and config ref",
			input: strings.Replace(completeClasses, `work = "work"`, `work = "tasks"`, 1) + `
[storage.bindings.tasks]
provider = "test-go-provider"
path = ".gc/store"
config_ref = "city-work"
`,
			wantErr: `path and config reference are mutually exclusive`,
		},
		{
			name: "migration key",
			input: completeClasses + `
[storage]
migration = "copy"
`,
			wantErr: `storage migration and mode keys are not supported`,
		},
		{
			name: "binding mode key",
			input: strings.Replace(completeClasses, `graph = "work"`, `graph = "infra"`, 1) + `
[storage.bindings.infra]
provider = "sqlite-beads"
mode = "active"
`,
			wantErr: `storage migration and mode keys are not supported`,
		},
		{
			name: "unknown binding field",
			input: strings.Replace(completeClasses, `graph = "work"`, `graph = "infra"`, 1) + `
[storage.bindings.infra]
provider = "sqlite-beads"
database = ".gc/store"
`,
			wantErr: `unknown storage field "storage.bindings.infra.database"`,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Parse([]byte(tc.input))
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("Parse error = %v, want substring %q", err, tc.wantErr)
			}
		})
	}
}

func TestStorageClassesExposeExactlySixCanonicalKeys(t *testing.T) {
	typ := reflect.TypeOf(StorageClasses{})
	got := make([]string, 0, typ.NumField())
	for i := 0; i < typ.NumField(); i++ {
		got = append(got, strings.Split(typ.Field(i).Tag.Get("toml"), ",")[0])
	}
	want := []string{"work", "graph", "sessions", "messaging", "orders", "nudges"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("StorageClasses TOML keys = %v, want %v", got, want)
	}
}

func TestStorageClassValuesMatchCoordinationClassContract(t *testing.T) {
	got := make([]string, 0, len(storageConfigClassOrder()))
	for _, class := range storageConfigClassOrder() {
		got = append(got, class.String())
	}
	want := make([]string, 0, len(coordclass.Classes()))
	for _, class := range coordclass.Classes() {
		want = append(want, class.String())
	}
	for _, class := range got {
		if !slices.Contains(want, class) {
			t.Errorf("config storage class %q is not a coordination class", class)
		}
	}
	for _, class := range want {
		if !slices.Contains(got, class) {
			t.Errorf("coordination class %q is not exposed by storage config", class)
		}
	}
}
