package main

import (
	"bytes"
	"os"
	"path/filepath"
	"reflect"
	goruntime "runtime"
	"slices"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/processenv"
	"github.com/gastownhall/gascity/internal/runtime/tmux"
)

// isolateSupervisorRoutingEnv gives a test an empty HOME and GC_HOME, a fixed
// PATH, no GC_SUPERVISOR_ENV or omit flag, stubbed service managers, and no
// provider-prefixed name from the test process's own environment (the shell
// scan skips empty values).
func isolateSupervisorRoutingEnv(t *testing.T) {
	t.Helper()
	homeDir := t.TempDir()
	t.Setenv("HOME", homeDir)
	t.Setenv("GC_HOME", filepath.Join(homeDir, ".gc"))
	t.Setenv("PATH", "/usr/local/bin:/usr/bin:/bin")
	t.Setenv("GC_SUPERVISOR_ENV", "")
	t.Setenv(supervisorOmitProviderCredsEnv, "")
	for _, entry := range os.Environ() {
		key, _, ok := strings.Cut(entry, "=")
		if ok && processenv.IsProviderCredentialEnv(key) {
			t.Setenv(key, "")
		}
	}
	stubServiceManagers(t, "", nil, "")
}

// incidentRoutingNames are the shell-only ANTHROPIC_ routing names from the
// #9257 incident, sorted.
var incidentRoutingNames = []string{
	"ANTHROPIC_BASE_URL",
	"ANTHROPIC_DEFAULT_OPUS_MODEL",
	"ANTHROPIC_MODEL",
	"ANTHROPIC_SMALL_FAST_MODEL",
}

// setIncidentEnv reproduces the #9257 host: secrets.env holds three
// ANTHROPIC_ tokens and the calling shell exports four ANTHROPIC_ routing
// names and no ANTHROPIC_ credential.
func setIncidentEnv(t *testing.T) {
	t.Helper()
	writeSupervisorSecretsEnvFile(t, "ANTHROPIC_AUTH_TOKEN=tok-file-1\nANTHROPIC_AUTH_TOKEN2=tok-file-2\nANTHROPIC_AUTH_TOKEN_ZAI=tok-file-zai\n")
	for _, name := range incidentRoutingNames {
		t.Setenv(name, "shell-"+strings.ToLower(name))
	}
}

// TestSupervisorServiceEnvRejectsShellOnlyRoutingBesideFileCredential
// reproduces issue #9257: the provider credential comes only from
// ${GC_HOME}/secrets.env while ANTHROPIC_BASE_URL is exported only in the
// calling shell. The routing name must not be persisted without an explicit
// GC_SUPERVISOR_ENV opt-in, so the service env never pairs a file-tier
// credential with a shell-tier endpoint.
func TestSupervisorServiceEnvRejectsShellOnlyRoutingBesideFileCredential(t *testing.T) {
	isolateSupervisorRoutingEnv(t)
	t.Setenv("ANTHROPIC_BASE_URL", "https://other-provider.example.test")
	writeSupervisorSecretsEnvFile(t, "ANTHROPIC_AUTH_TOKEN=sk-from-file\n")

	data, err := buildSupervisorServiceData()
	if err != nil {
		t.Fatalf("buildSupervisorServiceData: %v", err)
	}
	got := supervisorServiceEnvMap(data.ExtraEnv)
	if got["ANTHROPIC_AUTH_TOKEN"] != "sk-from-file" {
		t.Fatalf("ExtraEnv[ANTHROPIC_AUTH_TOKEN] = %q, want file credential", got["ANTHROPIC_AUTH_TOKEN"])
	}
	if v, ok := got["ANTHROPIC_BASE_URL"]; ok {
		t.Fatalf("ExtraEnv persisted shell-only routing ANTHROPIC_BASE_URL=%q beside a secrets.env-sourced credential (mixed-tier credential/routing chimera)", v)
	}
}

// TestSupervisorServiceEnvWithholdsIncidentShellRouting pins AC-1: with every
// ANTHROPIC_ credential from secrets.env and the incident's four routing names
// only in the shell, the tokens persist from the file, none of the routing
// names persists, and one ANTHROPIC_ group records them withheld.
func TestSupervisorServiceEnvWithholdsIncidentShellRouting(t *testing.T) {
	isolateSupervisorRoutingEnv(t)
	setIncidentEnv(t)

	data, err := buildSupervisorServiceData()
	if err != nil {
		t.Fatalf("buildSupervisorServiceData: %v", err)
	}
	got := supervisorServiceEnvMap(data.ExtraEnv)
	for name, want := range map[string]string{
		"ANTHROPIC_AUTH_TOKEN":     "tok-file-1",
		"ANTHROPIC_AUTH_TOKEN2":    "tok-file-2",
		"ANTHROPIC_AUTH_TOKEN_ZAI": "tok-file-zai",
	} {
		if got[name] != want {
			t.Errorf("ExtraEnv[%s] = %q, want %q", name, got[name], want)
		}
	}
	for _, name := range incidentRoutingNames {
		if v, ok := got[name]; ok {
			t.Errorf("ExtraEnv persisted shell-only routing %s=%q beside secrets.env credentials", name, v)
		}
	}
	want := []supervisorWithheldEnv{{
		Prefix:            "ANTHROPIC_",
		Dropped:           incidentRoutingNames,
		CredentialSources: []supervisorEnvSource{supervisorEnvSourceSecretsFile},
	}}
	if !reflect.DeepEqual(data.WithheldEnv, want) {
		t.Fatalf("WithheldEnv = %+v, want %+v", data.WithheldEnv, want)
	}
}

// TestSupervisorServiceEnvWithholdsShellRoutingPerPrefix pins that the gate
// is per provider prefix, not Anthropic-specific.
func TestSupervisorServiceEnvWithholdsShellRoutingPerPrefix(t *testing.T) {
	isolateSupervisorRoutingEnv(t)
	t.Setenv("OPENAI_BASE_URL", "https://shell.example.test/v1")
	writeSupervisorSecretsEnvFile(t, "OPENAI_API_KEY=sk-openai-file\n")

	data, err := buildSupervisorServiceData()
	if err != nil {
		t.Fatalf("buildSupervisorServiceData: %v", err)
	}
	got := supervisorServiceEnvMap(data.ExtraEnv)
	if got["OPENAI_API_KEY"] != "sk-openai-file" {
		t.Errorf("ExtraEnv[OPENAI_API_KEY] = %q, want file credential", got["OPENAI_API_KEY"])
	}
	if v, ok := got["OPENAI_BASE_URL"]; ok {
		t.Errorf("ExtraEnv persisted shell-only routing OPENAI_BASE_URL=%q beside a secrets.env credential", v)
	}
	want := []supervisorWithheldEnv{{
		Prefix:            "OPENAI_",
		Dropped:           []string{"OPENAI_BASE_URL"},
		CredentialSources: []supervisorEnvSource{supervisorEnvSourceSecretsFile},
	}}
	if !reflect.DeepEqual(data.WithheldEnv, want) {
		t.Fatalf("WithheldEnv = %+v, want %+v", data.WithheldEnv, want)
	}
}

// TestSupervisorServiceEnvWithholdsShellRoutingBesideLaunchctlCredential
// pins launchctl as a durable credential tier: a GC_SUPERVISOR_ENV-listed
// credential that only launchctl supplies withholds the shell's routing as a
// secrets.env credential does, and the warning names both durable tiers when
// each supplies one.
func TestSupervisorServiceEnvWithholdsShellRoutingBesideLaunchctlCredential(t *testing.T) {
	isolateSupervisorRoutingEnv(t)
	t.Setenv("ANTHROPIC_BASE_URL", "https://shell.example.test")
	t.Setenv("GC_SUPERVISOR_ENV", "ANTHROPIC_API_KEY")
	writeSupervisorSecretsEnvFile(t, "ANTHROPIC_AUTH_TOKEN=tok-file\n")
	oldLaunchctl := supervisorLaunchctlGetenv
	t.Cleanup(func() { supervisorLaunchctlGetenv = oldLaunchctl })
	supervisorLaunchctlGetenv = func(key string) string {
		if key == "ANTHROPIC_API_KEY" {
			return "sk-launchctl"
		}
		return ""
	}

	data, err := buildSupervisorServiceData()
	if err != nil {
		t.Fatalf("buildSupervisorServiceData: %v", err)
	}
	got := supervisorServiceEnvByName(data.ExtraEnv)
	want := supervisorServiceEnvVar{Name: "ANTHROPIC_API_KEY", Value: "sk-launchctl", Source: supervisorEnvSourceLaunchctl}
	if got["ANTHROPIC_API_KEY"] != want {
		t.Errorf("ExtraEnv[ANTHROPIC_API_KEY] = %+v, want %+v", got["ANTHROPIC_API_KEY"], want)
	}
	wantWithheld := []supervisorWithheldEnv{{
		Prefix:            "ANTHROPIC_",
		Dropped:           []string{"ANTHROPIC_BASE_URL"},
		CredentialSources: []supervisorEnvSource{supervisorEnvSourceSecretsFile, supervisorEnvSourceLaunchctl},
	}}
	if !reflect.DeepEqual(data.WithheldEnv, wantWithheld) {
		t.Fatalf("WithheldEnv = %+v, want %+v", data.WithheldEnv, wantWithheld)
	}
	var stderr bytes.Buffer
	warnSupervisorWithheldEnv(&stderr, data.WithheldEnv)
	if want := "ANTHROPIC_ credentials come only from secrets.env and launchctl"; !strings.Contains(stderr.String(), want) {
		t.Errorf("warning %q does not say %q", stderr.String(), want)
	}
}

// TestSupervisorServiceEnvKeepsShellRoutingWithoutDurableOnlyCredentials pins
// AC-2: shell routing persists as it always has when no credential under the
// prefix comes from a durable tier, or when the shell itself supplies one.
func TestSupervisorServiceEnvKeepsShellRoutingWithoutDurableOnlyCredentials(t *testing.T) {
	t.Run("no secrets.env", func(t *testing.T) {
		isolateSupervisorRoutingEnv(t)
		t.Setenv("ANTHROPIC_BASE_URL", "https://shell.example.test")
		t.Setenv("OLLAMA_HOST", "http://127.0.0.1:11434")

		data, err := buildSupervisorServiceData()
		if err != nil {
			t.Fatalf("buildSupervisorServiceData: %v", err)
		}
		got := supervisorServiceEnvMap(data.ExtraEnv)
		if got["ANTHROPIC_BASE_URL"] != "https://shell.example.test" {
			t.Errorf("ExtraEnv[ANTHROPIC_BASE_URL] = %q, want shell value", got["ANTHROPIC_BASE_URL"])
		}
		if got["OLLAMA_HOST"] != "http://127.0.0.1:11434" {
			t.Errorf("ExtraEnv[OLLAMA_HOST] = %q, want shell value", got["OLLAMA_HOST"])
		}
		if len(data.WithheldEnv) != 0 {
			t.Errorf("WithheldEnv = %+v, want none", data.WithheldEnv)
		}
	})
	t.Run("shell supplies a credential", func(t *testing.T) {
		isolateSupervisorRoutingEnv(t)
		t.Setenv("ANTHROPIC_AUTH_TOKEN", "tok-shell")
		t.Setenv("ANTHROPIC_BASE_URL", "https://shell.example.test")
		writeSupervisorSecretsEnvFile(t, "ANTHROPIC_AUTH_TOKEN_ZAI=tok-file-zai\n")

		data, err := buildSupervisorServiceData()
		if err != nil {
			t.Fatalf("buildSupervisorServiceData: %v", err)
		}
		got := supervisorServiceEnvMap(data.ExtraEnv)
		for name, want := range map[string]string{
			"ANTHROPIC_AUTH_TOKEN":     "tok-shell",
			"ANTHROPIC_BASE_URL":       "https://shell.example.test",
			"ANTHROPIC_AUTH_TOKEN_ZAI": "tok-file-zai",
		} {
			if got[name] != want {
				t.Errorf("ExtraEnv[%s] = %q, want %q", name, got[name], want)
			}
		}
		if len(data.WithheldEnv) != 0 {
			t.Errorf("WithheldEnv = %+v, want none", data.WithheldEnv)
		}
	})
}

// supervisorServiceEnvByName indexes vars by name, keeping each entry's source.
func supervisorServiceEnvByName(vars []supervisorServiceEnvVar) map[string]supervisorServiceEnvVar {
	m := make(map[string]supervisorServiceEnvVar, len(vars))
	for _, v := range vars {
		m[v.Name] = v
	}
	return m
}

// TestSupervisorServiceEnvPersistsExplicitShellRouting pins AC-3: a routing
// name listed in GC_SUPERVISOR_ENV persists with the shell value and source
// GC_SUPERVISOR_ENV, while the prefix's other shell routing names are still
// withheld.
func TestSupervisorServiceEnvPersistsExplicitShellRouting(t *testing.T) {
	isolateSupervisorRoutingEnv(t)
	setIncidentEnv(t)
	t.Setenv("GC_SUPERVISOR_ENV", "ANTHROPIC_BASE_URL")

	data, err := buildSupervisorServiceData()
	if err != nil {
		t.Fatalf("buildSupervisorServiceData: %v", err)
	}
	got := supervisorServiceEnvByName(data.ExtraEnv)
	want := supervisorServiceEnvVar{Name: "ANTHROPIC_BASE_URL", Value: "shell-anthropic_base_url", Source: supervisorEnvSourceExplicit}
	if got["ANTHROPIC_BASE_URL"] != want {
		t.Errorf("ExtraEnv[ANTHROPIC_BASE_URL] = %+v, want %+v", got["ANTHROPIC_BASE_URL"], want)
	}
	if src := got["ANTHROPIC_AUTH_TOKEN"].Source; src != supervisorEnvSourceSecretsFile {
		t.Errorf("ExtraEnv[ANTHROPIC_AUTH_TOKEN].Source = %v, want %v", src, supervisorEnvSourceSecretsFile)
	}
	wantWithheld := []supervisorWithheldEnv{{
		Prefix:            "ANTHROPIC_",
		Dropped:           []string{"ANTHROPIC_DEFAULT_OPUS_MODEL", "ANTHROPIC_MODEL", "ANTHROPIC_SMALL_FAST_MODEL"},
		CredentialSources: []supervisorEnvSource{supervisorEnvSourceSecretsFile},
	}}
	if !reflect.DeepEqual(data.WithheldEnv, wantWithheld) {
		t.Fatalf("WithheldEnv = %+v, want %+v", data.WithheldEnv, wantWithheld)
	}
	for _, name := range wantWithheld[0].Dropped {
		if v, ok := got[name]; ok {
			t.Errorf("ExtraEnv persisted withheld %s=%q", name, v.Value)
		}
	}
}

// TestSupervisorServiceEnvTakesWithheldRoutingFromSecretsFile pins AC-4: a
// withheld shell routing name that secrets.env also sets persists with the
// file's value and source, beside the file's credential. The install reports
// the name as replaced only when the file's value differs from the shell's.
func TestSupervisorServiceEnvTakesWithheldRoutingFromSecretsFile(t *testing.T) {
	for _, tt := range []struct {
		name         string
		fileValue    string
		wantWithheld []supervisorWithheldEnv
	}{
		{
			name:      "file value differs",
			fileValue: "https://file.example.test",
			wantWithheld: []supervisorWithheldEnv{{
				Prefix:            "ANTHROPIC_",
				Replaced:          []string{"ANTHROPIC_BASE_URL"},
				CredentialSources: []supervisorEnvSource{supervisorEnvSourceSecretsFile},
			}},
		},
		{
			name:      "file value matches",
			fileValue: "https://shell.example.test",
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			isolateSupervisorRoutingEnv(t)
			t.Setenv("ANTHROPIC_BASE_URL", "https://shell.example.test")
			writeSupervisorSecretsEnvFile(t, "ANTHROPIC_AUTH_TOKEN=tok-file\nANTHROPIC_BASE_URL="+tt.fileValue+"\n")

			data, err := buildSupervisorServiceData()
			if err != nil {
				t.Fatalf("buildSupervisorServiceData: %v", err)
			}
			got := supervisorServiceEnvByName(data.ExtraEnv)
			want := supervisorServiceEnvVar{Name: "ANTHROPIC_BASE_URL", Value: tt.fileValue, Source: supervisorEnvSourceSecretsFile}
			if got["ANTHROPIC_BASE_URL"] != want {
				t.Fatalf("ExtraEnv[ANTHROPIC_BASE_URL] = %+v, want %+v", got["ANTHROPIC_BASE_URL"], want)
			}
			if !reflect.DeepEqual(data.WithheldEnv, tt.wantWithheld) {
				t.Fatalf("WithheldEnv = %+v, want %+v", data.WithheldEnv, tt.wantWithheld)
			}
		})
	}
}

// TestSupervisorServiceEnvIgnoresNeutralProviderSettings pins that a provider
// setting that neither carries a credential nor routes a call (OPENAI_ORG_ID,
// OLLAMA_MODELS) persists from its own tier and never decides which tier a
// provider's credentials come from: a shell setting does not keep the shell's
// routing beside a credential only secrets.env supplies, and a secrets.env
// setting does not withhold the shell's routing where no credential exists.
func TestSupervisorServiceEnvIgnoresNeutralProviderSettings(t *testing.T) {
	t.Run("shell setting beside a file-only credential", func(t *testing.T) {
		isolateSupervisorRoutingEnv(t)
		t.Setenv("OPENAI_BASE_URL", "https://shell.example.test/v1")
		t.Setenv("OPENAI_ORG_ID", "org-shell")
		writeSupervisorSecretsEnvFile(t, "OPENAI_API_KEY=sk-openai-file\n")

		data, err := buildSupervisorServiceData()
		if err != nil {
			t.Fatalf("buildSupervisorServiceData: %v", err)
		}
		got := supervisorServiceEnvByName(data.ExtraEnv)
		if v, ok := got["OPENAI_BASE_URL"]; ok {
			t.Errorf("ExtraEnv persisted shell routing OPENAI_BASE_URL=%q beside a secrets.env-only credential; a shell OPENAI_ORG_ID carries no credential", v.Value)
		}
		want := supervisorServiceEnvVar{Name: "OPENAI_ORG_ID", Value: "org-shell", Source: supervisorEnvSourceShell}
		if got["OPENAI_ORG_ID"] != want {
			t.Errorf("ExtraEnv[OPENAI_ORG_ID] = %+v, want %+v", got["OPENAI_ORG_ID"], want)
		}
		wantWithheld := []supervisorWithheldEnv{{
			Prefix:            "OPENAI_",
			Dropped:           []string{"OPENAI_BASE_URL"},
			CredentialSources: []supervisorEnvSource{supervisorEnvSourceSecretsFile},
		}}
		if !reflect.DeepEqual(data.WithheldEnv, wantWithheld) {
			t.Fatalf("WithheldEnv = %+v, want %+v", data.WithheldEnv, wantWithheld)
		}
	})
	t.Run("file setting without a credential", func(t *testing.T) {
		isolateSupervisorRoutingEnv(t)
		t.Setenv("OLLAMA_HOST", "http://remote-host:11434")
		writeSupervisorSecretsEnvFile(t, "OLLAMA_MODELS=/srv/ollama/models\n")

		data, err := buildSupervisorServiceData()
		if err != nil {
			t.Fatalf("buildSupervisorServiceData: %v", err)
		}
		got := supervisorServiceEnvByName(data.ExtraEnv)
		for _, want := range []supervisorServiceEnvVar{
			{Name: "OLLAMA_HOST", Value: "http://remote-host:11434", Source: supervisorEnvSourceShell},
			{Name: "OLLAMA_MODELS", Value: "/srv/ollama/models", Source: supervisorEnvSourceSecretsFile},
		} {
			if got[want.Name] != want {
				t.Errorf("ExtraEnv[%s] = %+v, want %+v", want.Name, got[want.Name], want)
			}
		}
		if len(data.WithheldEnv) != 0 {
			t.Errorf("WithheldEnv = %+v, want none: OLLAMA_MODELS carries no credential", data.WithheldEnv)
		}
	})
}

// installSupervisorServiceForTest runs installSupervisorServiceReporting for
// goos with systemd and the running-supervisor probe stubbed: no supervisor
// answers, and every systemctl call succeeds.
func installSupervisorServiceForTest(t *testing.T, goos string, data *supervisorServiceData) (int, string, string) {
	t.Helper()
	stubRunningSupervisor(t, false, "", nil)
	oldRun, oldActive, oldLinger := supervisorSystemctlRun, supervisorSystemctlActive, supervisorLingerEnabled
	t.Cleanup(func() {
		supervisorSystemctlRun, supervisorSystemctlActive, supervisorLingerEnabled = oldRun, oldActive, oldLinger
	})
	supervisorSystemctlRun = func(...string) error { return nil }
	supervisorSystemctlActive = func(string) bool { return false }
	supervisorLingerEnabled = func(string) bool { return true }
	stubSupervisorSystemctlUserAvailable(t, true)
	stubSupervisorRunningPreserveSignalReady(t, true)

	var stdout, stderr bytes.Buffer
	code := installSupervisorServiceReporting(goos, data, &stdout, &stderr)
	return code, stdout.String(), stderr.String()
}

// parseSupervisorEnvSourcesReport returns the report's names by source label.
func parseSupervisorEnvSourcesReport(t *testing.T, stdout string) map[string][]string {
	t.Helper()
	_, report, ok := strings.Cut(stdout, supervisorEnvSourcesReportHeader+"\n")
	if !ok {
		t.Fatalf("stdout has no %q report:\n%s", supervisorEnvSourcesReportHeader, stdout)
	}
	bySource := make(map[string][]string)
	for _, line := range strings.Split(report, "\n") {
		if !strings.HasPrefix(line, "  ") {
			break
		}
		label, names, ok := strings.Cut(strings.TrimSpace(line), ": ")
		if !ok {
			t.Fatalf("report line %q has no %q", line, ": ")
		}
		bySource[label] = strings.Split(names, ", ")
	}
	return bySource
}

// TestSupervisorInstallReportsEnvSourcesWithoutValues pins AC-5: a successful
// install prints every persisted name once, under the label of the tier that
// supplied it, and prints no value; a failed install prints no report.
func TestSupervisorInstallReportsEnvSourcesWithoutValues(t *testing.T) {
	if goruntime.GOOS != "linux" {
		t.Skip("systemd path only applies on linux")
	}
	isolateSupervisorRoutingEnv(t)
	setIncidentEnv(t)
	t.Setenv("GC_SUPERVISOR_ENV", "ANTHROPIC_BASE_URL")
	t.Setenv("LANG", "en_US.UTF-8")
	t.Setenv("GC_DOLT_PASSWORD", "")
	t.Setenv(tmux.TmpdirEnv, "")
	oldLaunchctl := supervisorLaunchctlGetenv
	t.Cleanup(func() { supervisorLaunchctlGetenv = oldLaunchctl })
	supervisorLaunchctlGetenv = func(key string) string {
		if key == "GC_DOLT_PASSWORD" {
			return "dolt-pass-from-launchctl"
		}
		return ""
	}

	data, err := buildSupervisorServiceData()
	if err != nil {
		t.Fatalf("buildSupervisorServiceData: %v", err)
	}
	code, stdout, stderr := installSupervisorServiceForTest(t, "linux", data)
	if code != 0 {
		t.Fatalf("install code = %d, want 0; stderr=%s", code, stderr)
	}

	bySource := parseSupervisorEnvSourcesReport(t, stdout)
	seen := make(map[string]string)
	for label, names := range bySource {
		if !slices.IsSorted(names) {
			t.Errorf("report %s names %v are not sorted", label, names)
		}
		for _, name := range names {
			if prev, dup := seen[name]; dup {
				t.Errorf("report lists %s under both %s and %s", name, prev, label)
			}
			seen[name] = label
		}
	}
	for _, v := range data.ExtraEnv {
		if got, want := seen[v.Name], v.Source.String(); got != want {
			t.Errorf("report lists %s under %q, want %q", v.Name, got, want)
		}
	}
	if len(seen) != len(data.ExtraEnv) {
		t.Errorf("report lists %d names, want the %d ExtraEnv names: %v", len(seen), len(data.ExtraEnv), bySource)
	}
	for label, want := range map[string]string{
		"GC_SUPERVISOR_ENV": "ANTHROPIC_BASE_URL",
		"secrets.env":       "ANTHROPIC_AUTH_TOKEN",
		"launchctl":         "GC_DOLT_PASSWORD",
		"shell":             "LANG",
		"gc":                tmux.TmpdirEnv,
	} {
		if !slices.Contains(bySource[label], want) {
			t.Errorf("report %s = %v, want it to list %s", label, bySource[label], want)
		}
	}
	for _, secret := range []string{"tok-file-1", "tok-file-2", "tok-file-zai", "shell-anthropic_base_url", "dolt-pass-from-launchctl", "en_US.UTF-8"} {
		if strings.Contains(stdout, secret) || strings.Contains(stderr, secret) {
			t.Errorf("install output carries persisted value %q:\nstdout=%s\nstderr=%s", secret, stdout, stderr)
		}
	}

	code, stdout, _ = installSupervisorServiceForTest(t, "freebsd", data)
	if code == 0 {
		t.Fatalf("install on freebsd code = 0, want nonzero")
	}
	if strings.Contains(stdout, supervisorEnvSourcesReportHeader) {
		t.Fatalf("failed install printed the sources report:\n%s", stdout)
	}
}

// stderrLineNaming returns the first line of stderr that contains name, or "".
func stderrLineNaming(stderr, name string) string {
	for _, line := range strings.Split(stderr, "\n") {
		if strings.Contains(line, name) {
			return line
		}
	}
	return ""
}

// TestSupervisorInstallWarnsOfWithheldShellRouting pins AC-6 and its two
// outcomes. The #9257 install still succeeds. The written unit sets no dropped
// name and sets secrets.env's value for a name the file also has. stderr warns
// of the dropped names with the prefix, the credential source, that running
// agent sessions keep their values until gc restart, the secrets.env path and
// the GC_SUPERVISOR_ENV opt-in, notes a name that took a different
// secrets.env value without that remedy, says nothing of a name whose two
// values match, and prints no value.
func TestSupervisorInstallWarnsOfWithheldShellRouting(t *testing.T) {
	if goruntime.GOOS != "linux" {
		t.Skip("systemd path only applies on linux")
	}
	isolateSupervisorRoutingEnv(t)
	setIncidentEnv(t)
	writeSupervisorSecretsEnvFile(t, "ANTHROPIC_AUTH_TOKEN=tok-file-1\nANTHROPIC_MODEL=file-model\nANTHROPIC_SMALL_FAST_MODEL=shell-anthropic_small_fast_model\n")
	dropped := []string{"ANTHROPIC_BASE_URL", "ANTHROPIC_DEFAULT_OPUS_MODEL"}

	data, err := buildSupervisorServiceData()
	if err != nil {
		t.Fatalf("buildSupervisorServiceData: %v", err)
	}
	code, stdout, stderr := installSupervisorServiceForTest(t, "linux", data)
	if code != 0 {
		t.Fatalf("install code = %d, want 0; stderr=%s", code, stderr)
	}
	unitBytes, err := os.ReadFile(supervisorSystemdServicePath())
	if err != nil {
		t.Fatalf("reading installed unit: %v", err)
	}
	unit := string(unitBytes)
	for _, name := range dropped {
		if strings.Contains(unit, "Environment="+name+"=") {
			t.Errorf("installed unit sets withheld %s:\n%s", name, unit)
		}
	}
	if want := `Environment=ANTHROPIC_MODEL="file-model"`; !strings.Contains(unit, want) {
		t.Errorf("installed unit lacks %s:\n%s", want, unit)
	}

	warning := stderrLineNaming(stderr, "ANTHROPIC_BASE_URL")
	for _, want := range append([]string{"warning", "ANTHROPIC_ credentials", "secrets.env", "running ones keep their values until gc restart", supervisorSecretsEnvFilePath(), "GC_SUPERVISOR_ENV"}, dropped...) {
		if !strings.Contains(warning, want) {
			t.Errorf("warning %q does not name %q; stderr:\n%s", warning, want, stderr)
		}
	}
	note := stderrLineNaming(stderr, "ANTHROPIC_MODEL")
	if note == "" || note == warning {
		t.Errorf("stderr has no line of its own for ANTHROPIC_MODEL, which took a different secrets.env value:\n%s", stderr)
	}
	for _, remedy := range []string{"warning", "GC_SUPERVISOR_ENV", supervisorSecretsEnvFilePath()} {
		if strings.Contains(note, remedy) {
			t.Errorf("note %q carries the remedy text %q; the name already takes secrets.env's value", note, remedy)
		}
	}
	if strings.Contains(stderr, "ANTHROPIC_SMALL_FAST_MODEL") {
		t.Errorf("stderr names ANTHROPIC_SMALL_FAST_MODEL, whose shell and secrets.env values match:\n%s", stderr)
	}
	for _, value := range []string{"shell-anthropic_base_url", "shell-anthropic_default_opus_model", "shell-anthropic_model", "shell-anthropic_small_fast_model", "file-model", "tok-file-1"} {
		if strings.Contains(stdout, value) || strings.Contains(stderr, value) {
			t.Errorf("install output carries value %q", value)
		}
	}
}
