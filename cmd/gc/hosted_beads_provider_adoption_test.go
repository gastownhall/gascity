package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/clientcontext"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/credentialprovider"
	"github.com/gastownhall/gascity/internal/shellquote"
)

const hostedBeadsProviderTestMarker = "hosted-beads-credential-provider-helper"

func stubHostedBeadsCredentialExecutable(t *testing.T, executable string) string {
	t.Helper()
	original := hostedBeadsCredentialExecutable
	hostedBeadsCredentialExecutable = func() (string, error) { return executable, nil }
	t.Cleanup(func() { hostedBeadsCredentialExecutable = original })
	absolute, err := filepath.Abs(executable)
	if err != nil {
		t.Fatal(err)
	}
	return shellquote.Quote(absolute) + " internal beads-credential"
}

// TestHostedBeadsCredentialProviderProcess is re-executed by the credential
// provider tests below. The subprocess records the exact request and returns a
// protocol-valid credential without placing a bearer in process arguments or
// environment.
func TestHostedBeadsCredentialProviderProcess(_ *testing.T) {
	marker := slices.Index(os.Args, hostedBeadsProviderTestMarker)
	if marker < 0 {
		return
	}
	if len(os.Args) != marker+3 {
		os.Exit(2)
	}
	requestPath, mode := os.Args[marker+1], os.Args[marker+2]
	request, err := io.ReadAll(os.Stdin)
	if err != nil {
		os.Exit(3)
	}
	requestFile, err := os.OpenFile(requestPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		os.Exit(4)
	}
	if _, err := requestFile.Write(append(request, '\n')); err != nil {
		_ = requestFile.Close()
		os.Exit(5)
	}
	if err := requestFile.Close(); err != nil {
		os.Exit(6)
	}
	if mode == "error" {
		if _, err := fmt.Fprintln(os.Stdout, `{"version":"gascity.dev/credential-provider/v1","kind":"Error","code":"access_denied","message":"super-secret-bearer"}`); err != nil {
			os.Exit(8)
		}
		os.Exit(0)
	}
	var decoded struct {
		Audience       string   `json:"audience"`
		RequiredScopes []string `json:"required_scopes"`
		ForceRefresh   bool     `json:"force_refresh"`
	}
	if err := json.Unmarshal(request, &decoded); err != nil {
		os.Exit(7)
	}
	token := "opaque-initial"
	if decoded.ForceRefresh {
		token = "opaque-refreshed"
	}
	_ = json.NewEncoder(os.Stdout).Encode(map[string]any{
		"version":              credentialprovider.ProtocolVersion,
		"kind":                 "Credential",
		"access_token":         token,
		"authorization_scheme": "Bearer",
		"expires_at":           "2099-01-02T03:04:05Z",
		"audience":             decoded.Audience,
		"scopes":               decoded.RequiredScopes,
	})
	os.Exit(0)
}

func hostedBeadsProviderArgv(t *testing.T, requestPath, mode string) string {
	t.Helper()
	encoded, err := json.Marshal([]string{
		os.Args[0],
		"-test.run=^TestHostedBeadsCredentialProviderProcess$",
		"--",
		hostedBeadsProviderTestMarker,
		requestPath,
		mode,
	})
	if err != nil {
		t.Fatal(err)
	}
	return string(encoded)
}

func hostedBeadsCityTOML(url, auth string, partial bool) string {
	sessionsBinding := "infra"
	if partial {
		sessionsBinding = "work"
	}
	endpoint := ""
	if url != "" {
		endpoint += "url = " + fmt.Sprintf("%q", url) + "\n"
	}
	if auth != "" {
		endpoint += "auth = " + fmt.Sprintf("%q", auth) + "\n"
	}
	return fmt.Sprintf(`[workspace]
name = "hosted-provider-test"

[storage.classes]
work = "work"
graph = "infra"
sessions = %q
messaging = "infra"
orders = "infra"
nudges = "infra"

[storage.bindings.infra]
provider = "beads-workspace"
config_ref = "infra"
%s`, sessionsBinding, endpoint)
}

func writeHostedBeadsCity(t *testing.T, url, auth string, partial bool) string {
	t.Helper()
	cityPath := t.TempDir()
	if err := os.WriteFile(filepath.Join(cityPath, "city.toml"), []byte(hostedBeadsCityTOML(url, auth, partial)), 0o600); err != nil {
		t.Fatal(err)
	}
	return cityPath
}

func writeCompleteStorageBinding(t *testing.T, scopeRoot string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(scopeRoot, ".beads"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(scopeMetadataJSONPath(scopeRoot), []byte(`{"backend":"postgres","storage_endpoint":"https://beads.example","storage_database":"infra"}`), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestHostedBeadsCredentialSelectorIsExact(t *testing.T) {
	for _, key := range []string{"GC_DOLT_CRED_CMD", "BEADS_DOLT_CREDENTIAL_COMMAND"} {
		t.Setenv(key, "")
	}
	wantBridge := stubHostedBeadsCredentialExecutable(t, "/opt/current gc/bin/gc")
	providerArgv := `["/opt/gasworks","credential-provider"]`
	t.Setenv(registryCredentialProviderEnv, providerArgv)
	tests := []struct {
		name    string
		city    func(*testing.T) string
		want    bool
		wantErr bool
	}{
		{name: "exact hosted selector", city: func(t *testing.T) string {
			return writeHostedBeadsCity(t, "https://beads.example/workspaces/infra", "gasworks", false)
		}, want: true},
		{name: "local workspace", city: func(t *testing.T) string {
			return writeHostedBeadsCity(t, "", "", false)
		}},
		{name: "environment auth", city: func(t *testing.T) string {
			return writeHostedBeadsCity(t, "https://beads.example", "env:BEADS_TOKEN", false)
		}},
		{name: "missing auth", city: func(t *testing.T) string {
			return writeHostedBeadsCity(t, "https://beads.example", "", false)
		}},
		{name: "partial topology", city: func(t *testing.T) string {
			return writeHostedBeadsCity(t, "https://beads.example", "gasworks", true)
		}},
		{name: "no storage", city: func(t *testing.T) string {
			cityPath := t.TempDir()
			if err := os.WriteFile(filepath.Join(cityPath, "city.toml"), []byte("[workspace]\nname = \"local\"\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			return cityPath
		}},
		{name: "no city file", city: func(t *testing.T) string { return t.TempDir() }},
		{name: "invalid city config fails closed", city: func(t *testing.T) string {
			cityPath := t.TempDir()
			if err := os.WriteFile(filepath.Join(cityPath, "city.toml"), []byte("[storage"), 0o600); err != nil {
				t.Fatal(err)
			}
			return cityPath
		}, wantErr: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			env := map[string]string{}
			err := applyHostedBeadsCredentialEnv(env, test.city(t))
			if (err != nil) != test.wantErr {
				t.Fatalf("applyHostedBeadsCredentialEnv() error = %v, wantErr %v", err, test.wantErr)
			}
			if got := env["BEADS_DOLT_CREDENTIAL_COMMAND"]; (got == wantBridge) != test.want {
				t.Fatalf("credential command = %q, selected=%v, want selected=%v", got, got == wantBridge, test.want)
			}
			if got, present := env[registryCredentialProviderEnv]; present != test.want || (present && got != providerArgv) {
				t.Fatalf("provider argv = %q, present=%v; want exact projection=%v", got, present, test.want)
			}
		})
	}
}

func TestHostedBeadsCredentialSelectorUsesComposedConfig(t *testing.T) {
	t.Setenv("GC_DOLT_CRED_CMD", "")
	t.Setenv("BEADS_DOLT_CREDENTIAL_COMMAND", "")
	wantBridge := stubHostedBeadsCredentialExecutable(t, "/opt/current gc/bin/gc")
	cityPath := t.TempDir()
	if err := os.WriteFile(filepath.Join(cityPath, "city.toml"), []byte("include = [\"storage.toml\"]\n[workspace]\nname = \"composed\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cityPath, "storage.toml"), []byte(strings.TrimPrefix(hostedBeadsCityTOML("https://beads.example", "gasworks", false), "[workspace]\nname = \"hosted-provider-test\"\n\n")), 0o600); err != nil {
		t.Fatal(err)
	}
	env := map[string]string{}
	if err := applyHostedBeadsCredentialEnv(env, cityPath); err != nil {
		t.Fatal(err)
	}
	if got := env["BEADS_DOLT_CREDENTIAL_COMMAND"]; got != wantBridge {
		t.Fatalf("credential command = %q, want %q", got, wantBridge)
	}
}

func TestHostedBeadsCredentialSelectorRequiresProviderAndSharedBinding(t *testing.T) {
	base := &config.City{Storage: &config.StorageConfig{
		Classes: config.StorageClasses{
			Work: "work", Graph: "infra", Sessions: "infra", Messaging: "infra", Orders: "infra", Nudges: "infra",
		},
		Bindings: map[string]config.StorageBindingConfig{
			"infra": {Provider: config.StorageProviderBeadsWorkspace, ConfigRef: "infra", URL: "https://beads.example", Auth: config.StorageAuthCredentialProvider},
		},
	}}
	if !configSelectsHostedBeadsCredentialProvider(base) {
		t.Fatal("exact selector was not recognized")
	}

	wrongProvider := *base
	wrongProvider.Storage = &config.StorageConfig{
		Classes: base.Storage.Classes,
		Bindings: map[string]config.StorageBindingConfig{
			"infra": {Provider: "other-provider", ConfigRef: "infra", URL: "https://beads.example", Auth: config.StorageAuthCredentialProvider},
		},
	}
	if configSelectsHostedBeadsCredentialProvider(&wrongProvider) {
		t.Fatal("selector accepted a non-beads-workspace provider")
	}

	mixed := *base
	mixed.Storage = &config.StorageConfig{
		Classes: config.StorageClasses{
			Work: "work", Graph: "infra", Sessions: "other", Messaging: "infra", Orders: "infra", Nudges: "infra",
		},
		Bindings: base.Storage.Bindings,
	}
	if configSelectsHostedBeadsCredentialProvider(&mixed) {
		t.Fatal("selector accepted infrastructure classes split across bindings")
	}
}

func TestHostedBeadsCredentialLegacyHelperPrecedence(t *testing.T) {
	cityPath := writeHostedBeadsCity(t, "https://beads.example", "gasworks", false)
	wantBridge := stubHostedBeadsCredentialExecutable(t, "/opt/current gc/bin/gc")
	tests := []struct {
		name       string
		env        map[string]string
		ambientGC  string
		ambientBD  string
		wantHelper string
	}{
		{name: "map gc helper", env: map[string]string{"GC_DOLT_CRED_CMD": "/map/gc-helper"}, ambientBD: "/ambient/bd-helper", wantHelper: "/map/gc-helper"},
		{name: "map bd helper", env: map[string]string{"BEADS_DOLT_CREDENTIAL_COMMAND": "/map/bd-helper"}, ambientGC: "/ambient/gc-helper", wantHelper: "/map/bd-helper"},
		{name: "ambient gc helper", env: map[string]string{}, ambientGC: "/ambient/gc-helper", ambientBD: "/ambient/bd-helper", wantHelper: "/ambient/gc-helper"},
		{name: "ambient bd helper is withheld", env: map[string]string{}, ambientBD: "/ambient/bd-helper", wantHelper: wantBridge},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Setenv("GC_DOLT_CRED_CMD", test.ambientGC)
			t.Setenv("BEADS_DOLT_CREDENTIAL_COMMAND", test.ambientBD)
			if err := applyHostedBeadsCredentialEnv(test.env, cityPath); err != nil {
				t.Fatal(err)
			}
			if got := test.env["BEADS_DOLT_CREDENTIAL_COMMAND"]; got != test.wantHelper {
				t.Fatalf("credential command = %q, want %q", got, test.wantHelper)
			}
		})
	}
}

func TestHostedBeadsProviderArgvProjection(t *testing.T) {
	stubHostedBeadsCredentialExecutable(t, "/opt/current gc/bin/gc")
	for _, raw := range []string{"", `{`, `["/opt/gasworks","credential-provider"]`} {
		t.Run(raw, func(t *testing.T) {
			t.Setenv(registryCredentialProviderEnv, raw)
			env := map[string]string{}
			cityPath := writeHostedBeadsCity(t, "https://beads.example", "gasworks", false)
			if err := applyHostedBeadsCredentialEnv(env, cityPath); err != nil {
				t.Fatal(err)
			}
			got, present := env[registryCredentialProviderEnv]
			if !present || got != raw {
				t.Fatalf("%s = %q, present=%v; want exact configured value %q", registryCredentialProviderEnv, got, present, raw)
			}
			merged := hostedEnvEntriesToMap(mergeRuntimeEnv([]string{registryCredentialProviderEnv + "=" + raw}, nil))
			if got, present := merged[registryCredentialProviderEnv]; !present || got != raw {
				t.Fatalf("merged %s = %q, present=%v; want exact value %q", registryCredentialProviderEnv, got, present, raw)
			}
		})
	}
}

func TestHostedBeadsProviderCityRigAndExecProjection(t *testing.T) {
	t.Setenv("GC_BEADS", "bd")
	t.Setenv("GC_DOLT_CRED_CMD", "")
	t.Setenv("BEADS_DOLT_CREDENTIAL_COMMAND", "")
	wantBridge := stubHostedBeadsCredentialExecutable(t, filepath.Join("relative current gc dir with ' quote", "gc"))
	providerArgv := `["/opt/gasworks","credential-provider","--profile","prod"]`
	t.Setenv(registryCredentialProviderEnv, providerArgv)
	cityPath := writeHostedBeadsCity(t, "https://beads.example", "gasworks", false)
	rigPath := filepath.Join(cityPath, "rigs", "frontend")
	if err := os.MkdirAll(rigPath, 0o700); err != nil {
		t.Fatal(err)
	}
	writeCompleteStorageBinding(t, cityPath)
	writeCompleteStorageBinding(t, rigPath)

	cityEnv, err := bdRuntimeEnvWithError(cityPath)
	if err != nil {
		t.Fatal(err)
	}
	rigEnv, err := bdRuntimeEnvForRigWithError(cityPath, nil, rigPath)
	if err != nil {
		t.Fatal(err)
	}
	for name, env := range map[string]map[string]string{"city": cityEnv, "rig": rigEnv} {
		if got := env["BEADS_DOLT_CREDENTIAL_COMMAND"]; got != wantBridge {
			t.Errorf("%s credential command = %q, want absolute quoted bridge %q", name, got, wantBridge)
		}
		if got := env[registryCredentialProviderEnv]; got != providerArgv {
			t.Errorf("%s %s = %q, want %q", name, registryCredentialProviderEnv, got, providerArgv)
		}
	}
	destination := map[string]string{}
	copyExecProjectedBackendEnv(destination, rigEnv)
	if got := destination["BEADS_DOLT_CREDENTIAL_COMMAND"]; got != wantBridge {
		t.Errorf("exec credential command = %q, want absolute quoted bridge %q", got, wantBridge)
	}
	if got := destination[registryCredentialProviderEnv]; got != providerArgv {
		t.Errorf("exec %s = %q, want %q", registryCredentialProviderEnv, got, providerArgv)
	}
}

func TestHostedBeadsAmbientNamespaceIsWithheldFromSessionsAndCommands(t *testing.T) {
	t.Setenv("GC_DOLT_CRED_CMD", "")
	t.Setenv(registryCredentialProviderEnv, `["/opt/gasworks","credential-provider"]`)
	ambient := map[string]string{
		"BEADS_DB":                      "ambient-database",
		"BEADS_DOLT_SERVER_HOST":        "ambient.example",
		"BEADS_DOLT_CREDENTIAL_COMMAND": "/ambient/helper",
		"BEADS_FUTURE_AUTHORITY":        "ambient-future-value",
	}
	for key, value := range ambient {
		t.Setenv(key, value)
	}
	wantBridge := stubHostedBeadsCredentialExecutable(t, "/opt/current gc/bin/gc")
	cityPath := writeHostedBeadsCity(t, "https://beads.example", "gasworks", false)
	writeCompleteStorageBinding(t, cityPath)

	sessionEnv, err := sessionBackendEnvWithError(cityPath, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := sessionEnv["BEADS_DOLT_CREDENTIAL_COMMAND"]; got != wantBridge {
		t.Fatalf("session credential command = %q, want selected bridge %q", got, wantBridge)
	}
	for key := range ambient {
		if key == "BEADS_DOLT_CREDENTIAL_COMMAND" {
			continue
		}
		if got, present := sessionEnv[key]; !present || got != "" {
			t.Errorf("session %s = %q, present=%v; want an explicit withholding", key, got, present)
		}
	}

	runner, err := beadsCommandRunnerForHostedCity(cityPath, sessionEnv)
	if err != nil {
		t.Fatal(err)
	}
	out, err := runner(t.TempDir(), "sh", "-c", "env | sort")
	if err != nil {
		t.Fatal(err)
	}
	childEnv := string(out)
	for key, value := range ambient {
		if strings.Contains(childEnv, key+"="+value+"\n") {
			t.Errorf("hosted command inherited %s", key)
		}
	}
	if !strings.Contains(childEnv, "BEADS_DOLT_CREDENTIAL_COMMAND="+wantBridge+"\n") {
		t.Errorf("hosted command env does not contain selected bridge: %q", childEnv)
	}

	processEnv, err := cityRuntimeProcessEnvWithError(cityPath)
	if err != nil {
		t.Fatal(err)
	}
	processValues := hostedEnvEntriesToMap(processEnv)
	if got := processValues["BEADS_DOLT_CREDENTIAL_COMMAND"]; got != wantBridge {
		t.Fatalf("city process credential command = %q, want selected bridge %q", got, wantBridge)
	}
	for key, ambientValue := range ambient {
		if got := processValues[key]; got == ambientValue {
			t.Errorf("city process inherited ambient %s=%q", key, got)
		}
	}
}

// The exact request includes both Beads scopes because bd's credential-command
// protocol carries no operation intent from which to select only read or write.
func TestInternalBeadsCredentialUsesExactNoninteractiveRequest(t *testing.T) {
	requestPath := filepath.Join(t.TempDir(), "requests.jsonl")
	t.Setenv(registryCredentialProviderEnv, hostedBeadsProviderArgv(t, requestPath, "credential"))
	previousCache := hostedBeadsCredentialCache
	hostedBeadsCredentialCache = credentialprovider.NewCache()
	t.Cleanup(func() { hostedBeadsCredentialCache = previousCache })

	var stdout, stderr bytes.Buffer
	cmd := newInternalBeadsCredentialCmd(&stdout, &stderr)
	cmd.SetArgs(nil)
	if err := cmd.ExecuteContext(context.Background()); err != nil {
		t.Fatalf("command error = %v, stderr=%q", err, stderr.String())
	}
	if got, want := strings.TrimSpace(stdout.String()), `{"token":"opaque-initial","expirationTimestamp":"2099-01-02T03:04:05Z"}`; got != want {
		t.Fatalf("stdout = %s, want %s", got, want)
	}
	requests, err := os.ReadFile(requestPath)
	if err != nil {
		t.Fatal(err)
	}
	wantRequest := `{"version":"gascity.dev/credential-provider/v1","audience":"beads","required_scopes":["beads:read","beads:write"],"org":"","force_refresh":false,"interactive":false}` + "\n"
	if got := string(requests); got != wantRequest {
		t.Fatalf("provider request = %s, want %s", got, wantRequest)
	}
}

func TestInternalBeadsCredentialDoesNotLeakProviderMessage(t *testing.T) {
	requestPath := filepath.Join(t.TempDir(), "requests.jsonl")
	t.Setenv(registryCredentialProviderEnv, hostedBeadsProviderArgv(t, requestPath, "error"))
	previousCache := hostedBeadsCredentialCache
	hostedBeadsCredentialCache = credentialprovider.NewCache()
	t.Cleanup(func() { hostedBeadsCredentialCache = previousCache })

	var stdout, stderr bytes.Buffer
	cmd := newInternalBeadsCredentialCmd(&stdout, &stderr)
	cmd.SetArgs(nil)
	if err := cmd.ExecuteContext(context.Background()); err == nil {
		t.Fatal("command unexpectedly succeeded")
	}
	if strings.Contains(stdout.String(), "super-secret-bearer") || strings.Contains(stderr.String(), "super-secret-bearer") {
		t.Fatalf("provider message leaked: stdout=%q stderr=%q", stdout.String(), stderr.String())
	}
	if !strings.Contains(stderr.String(), "credential provider failed: access_denied") {
		t.Fatalf("stderr = %q, want stable provider code", stderr.String())
	}
}

func TestRemoteContextProviderCachesAndForceRefreshes(t *testing.T) {
	requestPath := filepath.Join(t.TempDir(), "requests.jsonl")
	t.Setenv(registryCredentialProviderEnv, hostedBeadsProviderArgv(t, requestPath, "credential"))
	previousCache := remoteCredentialCache
	remoteCredentialCache = credentialprovider.NewCache()
	t.Cleanup(func() { remoteCredentialCache = previousCache })

	opts, err := remoteClientOptions(&remoteTarget{
		BaseURL:  "https://city.example",
		CityName: "prod",
		Ctx: &clientcontext.Context{
			Name:                     "prod",
			URL:                      "https://city.example",
			CredentialAudience:       "gascity-control",
			CredentialRequiredScopes: []string{"gascity:write", "gascity:read"},
			CredentialOrg:            "org-acme",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	first, err := opts.Token()
	if err != nil {
		t.Fatal(err)
	}
	cached, err := opts.Token()
	if err != nil {
		t.Fatal(err)
	}
	refreshed, err := opts.RefreshToken(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if first != "opaque-initial" || cached != first || refreshed != "opaque-refreshed" {
		t.Fatalf("tokens = first %q cached %q refreshed %q", first, cached, refreshed)
	}
	requestBytes, err := os.ReadFile(requestPath)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(requestBytes)), "\n")
	if len(lines) != 2 {
		t.Fatalf("provider calls = %d, want 2; requests=%s", len(lines), requestBytes)
	}
	for i, line := range lines {
		var request struct {
			Audience       string   `json:"audience"`
			RequiredScopes []string `json:"required_scopes"`
			Org            string   `json:"org"`
			ForceRefresh   bool     `json:"force_refresh"`
			Interactive    bool     `json:"interactive"`
		}
		if err := json.Unmarshal([]byte(line), &request); err != nil {
			t.Fatal(err)
		}
		if request.Audience != "gascity-control" || request.Org != "org-acme" || request.Interactive ||
			!slices.Equal(request.RequiredScopes, []string{"gascity:read", "gascity:write"}) || request.ForceRefresh != (i == 1) {
			t.Fatalf("request %d = %+v", i, request)
		}
	}
}
