package config

import (
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strconv"
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

// TestParseStorageRemoteWorkspaceBindingRoundTrips pins the authored shape of
// the two endpoint fields: a workspace binding whose backend is remote keeps
// its config_ref as the on-disk anchor and adds the endpoint plus a credential
// reference. Nothing in this build opens the endpoint — the fields are typed
// so the authoring surface can accept them at all, and so a malformed or
// secret-bearing value is refused at load rather than at first use.
func TestParseStorageRemoteWorkspaceBindingRoundTrips(t *testing.T) {
	const input = `
[workspace]
name = "remote-city"

[storage.classes]
work = "work"
graph = "remote"
sessions = "remote"
messaging = "remote"
orders = "remote"
nudges = "remote"

[storage.bindings.remote]
provider = "beads-workspace"
config_ref = "shared"
url = "https://beads.example/workspaces"
auth = "gasworks"
`

	cfg, err := Parse([]byte(input))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	want := StorageConfig{
		Classes: StorageClasses{
			Work:      "work",
			Graph:     "remote",
			Sessions:  "remote",
			Messaging: "remote",
			Orders:    "remote",
			Nudges:    "remote",
		},
		Bindings: map[string]StorageBindingConfig{
			"remote": {
				Provider:  StorageProviderBeadsWorkspace,
				ConfigRef: "shared",
				URL:       "https://beads.example/workspaces",
				Auth:      StorageAuthCredentialProvider,
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

// TestParseStorageAcceptsEveryAuthReferenceForm enumerates the complete set of
// credential references. Anything outside it is rejected by
// TestParseStorageRejectsInvalidAuthoring; the two together are the closed set.
func TestParseStorageAcceptsEveryAuthReferenceForm(t *testing.T) {
	for _, auth := range []string{
		StorageAuthCredentialProvider,
		"env:BEADS_TOKEN",
		"env:_leading_underscore",
		"env:A1",
	} {
		t.Run(auth, func(t *testing.T) {
			cfg, err := Parse([]byte(remoteWorkspaceCity(`url = "https://beads.example"`, "auth = "+strconv.Quote(auth))))
			if err != nil {
				t.Fatalf("Parse: %v", err)
			}
			if got := cfg.Storage.Bindings["remote"].Auth; got != auth {
				t.Fatalf("auth = %q, want %q", got, auth)
			}
		})
	}
}

// TestStorageBindingConfigStaysComparable is the property Equal, Clone, and
// StorageReloadRequiresRestart all rest on: the binding struct is compared with
// != as a value. A field of a non-comparable kind would not fail to compile
// here — it would fail to compile in Equal — so assert the property directly.
func TestStorageBindingConfigStaysComparable(t *testing.T) {
	if !reflect.TypeOf(StorageBindingConfig{}).Comparable() {
		t.Fatal("StorageBindingConfig is no longer comparable; StorageConfig.Equal compares bindings with !=")
	}
}

// TestStorageReloadRequiresRestartTracksRemoteEndpointFields proves the two new
// fields participate in reload equality. A binding whose endpoint or credential
// reference changed is a different binding, and live storage handles are
// immutable.
func TestStorageReloadRequiresRestartTracksRemoteEndpointFields(t *testing.T) {
	classes := StorageClasses{
		Work:      "work",
		Graph:     "remote",
		Sessions:  "remote",
		Messaging: "remote",
		Orders:    "remote",
		Nudges:    "remote",
	}
	base := &City{Storage: &StorageConfig{
		Classes: classes,
		Bindings: map[string]StorageBindingConfig{
			"remote": {
				Provider:  StorageProviderBeadsWorkspace,
				ConfigRef: "shared",
				URL:       "https://beads.example",
				Auth:      StorageAuthCredentialProvider,
			},
		},
	}}

	unchanged := base.Storage.Clone()
	if StorageReloadRequiresRestart(base, &City{Storage: &unchanged}) {
		t.Fatal("an unchanged remote binding should be reload-equivalent")
	}

	for name, mutate := range map[string]func(*StorageBindingConfig){
		"url":  func(b *StorageBindingConfig) { b.URL = "https://other.example" },
		"auth": func(b *StorageBindingConfig) { b.Auth = "env:BEADS_TOKEN" },
	} {
		t.Run(name, func(t *testing.T) {
			next := base.Storage.Clone()
			binding := next.Bindings["remote"]
			mutate(&binding)
			next.Bindings["remote"] = binding
			if !StorageReloadRequiresRestart(base, &City{Storage: &next}) {
				t.Fatalf("a changed %s should require restart", name)
			}
		})
	}
}

// remoteWorkspaceCity renders a complete city whose remote binding carries the
// given endpoint lines, so a rejection case names only what it is testing.
func remoteWorkspaceCity(lines ...string) string {
	return `
[storage.classes]
work = "work"
graph = "remote"
sessions = "remote"
messaging = "remote"
orders = "remote"
nudges = "remote"

[storage.bindings.remote]
provider = "beads-workspace"
config_ref = "shared"
` + strings.Join(lines, "\n") + "\n"
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
		{
			name: "sqlite rejects url",
			input: strings.Replace(completeClasses, `graph = "work"`, `graph = "infra"`, 1) + `
[storage.bindings.infra]
provider = "sqlite-beads"
path = ".gc/store"
url = "https://beads.example"
`,
			wantErr: `url is only supported by provider "beads-workspace"`,
		},
		{
			name: "other provider rejects url",
			input: strings.Replace(completeClasses, `graph = "work"`, `graph = "infra"`, 1) + `
[storage.bindings.infra]
provider = "test-go-provider"
config_ref = "infra"
url = "https://beads.example"
`,
			wantErr: `url is only supported by provider "beads-workspace"`,
		},
		{
			name:    "url scheme",
			input:   remoteWorkspaceCity(`url = "ftp://beads.example"`),
			wantErr: `url scheme must be http or https`,
		},
		{
			name:    "url without host",
			input:   remoteWorkspaceCity(`url = "https:///workspaces"`),
			wantErr: `url has no host`,
		},
		{
			name:    "url with userinfo",
			input:   remoteWorkspaceCity(`url = "https://token@beads.example"`),
			wantErr: `url must not embed credentials`,
		},
		{
			name:    "url with query",
			input:   remoteWorkspaceCity(`url = "https://beads.example?token=abc"`),
			wantErr: `url must not carry a query`,
		},
		{
			name:    "url with fragment",
			input:   remoteWorkspaceCity(`url = "https://beads.example#frag"`),
			wantErr: `url must not carry a fragment`,
		},
		{
			name:    "auth without url",
			input:   remoteWorkspaceCity(`auth = "gasworks"`),
			wantErr: `auth requires url`,
		},
		{
			name:    "unknown auth form",
			input:   remoteWorkspaceCity(`url = "https://beads.example"`, `auth = "bearer"`),
			wantErr: `auth must be "gasworks" or "env:<VARNAME>"`,
		},
		{
			name:    "auth env name",
			input:   remoteWorkspaceCity(`url = "https://beads.example"`, `auth = "env:1BAD"`),
			wantErr: `auth "env:" must be followed by an environment variable name`,
		},
		{
			name:    "auth carries a URL",
			input:   remoteWorkspaceCity(`url = "https://beads.example"`, `auth = "https://beads.example/token"`),
			wantErr: `auth is a credential reference, not credential material`,
		},
		{
			name:    "auth carries whitespace",
			input:   remoteWorkspaceCity(`url = "https://beads.example"`, `auth = "env: TOKEN"`),
			wantErr: `auth is a credential reference, not credential material`,
		},
		{
			name:    "auth carries a pasted token",
			input:   remoteWorkspaceCity(`url = "https://beads.example"`, `auth = "env:`+strings.Repeat("A", 64)+`"`),
			wantErr: `auth is longer than 64 bytes`,
		},
		{
			// The rule this pins fires BEFORE the generic "config_ref is
			// required for provider" rule, which would otherwise shadow it and
			// report the wrong reason: the binding is not missing config_ref
			// because its provider needs one, it is missing the on-disk anchor
			// the endpoint is relative to.
			name: "url without config ref",
			input: strings.Replace(completeClasses, `graph = "work"`, `graph = "remote"`, 1) + `
[storage.bindings.remote]
provider = "beads-workspace"
url = "https://beads.example"
`,
			wantErr: `storage.bindings.remote: url requires config_ref`,
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
