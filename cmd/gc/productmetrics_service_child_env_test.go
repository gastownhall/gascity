package main

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/execenv"
)

func TestProductMetricsServiceChildEnvSupervisorStart(t *testing.T) {
	t.Setenv("GC_HOME", t.TempDir())
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())
	t.Setenv(supervisorSystemdUnitEnv, "")
	t.Setenv(supervisorSystemdScopeEnv, "")

	previousAlive := supervisorAliveHook
	supervisorAliveHook = func() int { return 4242 }
	t.Cleanup(func() { supervisorAliveHook = previousAlive })

	entries := captureProductMetricsDirectChildEnv(t, func() error {
		var stdout, stderr bytes.Buffer
		if code := doSupervisorStartJSON(&stdout, &stderr, true); code != 0 {
			return fmt.Errorf("gc supervisor start code %d: stdout=%q stderr=%q", code, stdout.String(), stderr.String())
		}
		if stderr.Len() != 0 {
			t.Errorf("gc supervisor start stderr = %q, want empty", stderr.String())
		}
		payload := decodeLifecycleJSONLine(t, stdout.String())
		if payload["ok"] != true || payload["command"] != "supervisor start" || payload["action"] != "start" {
			t.Errorf("payload = %v, want ok=true command=%q action=%q", payload, "supervisor start", "start")
		}
		if pid, _ := payload["supervisor_pid"].(float64); int(pid) != 4242 {
			t.Errorf("payload supervisor_pid = %v, want 4242", payload["supervisor_pid"])
		}
		return nil
	})
	assertProductMetricsDirectChildEnv(t, entries)
}

func TestProductMetricsServiceChildEnvDriftRestart(t *testing.T) {
	t.Setenv("GC_HOME", t.TempDir())
	entries := captureProductMetricsDirectChildEnv(t, func() error {
		binary, err := os.Executable()
		if err != nil {
			return err
		}
		return spawnDetachedSupervisor(binary, "supervisor", "run")
	})
	assertProductMetricsDirectChildEnv(t, entries)
}

func TestProductMetricsServiceChildEnvAgentScriptMailSend(t *testing.T) {
	installProductMetricsDirectChildSpyCommand(t, "gc")
	entries := captureProductMetricsDirectChildEnv(t, func() error {
		return runAgentScriptCommand(io.Discard, io.Discard, "gc", "mail", "send", "worker", "subject")
	})
	assertProductMetricsDirectChildEnv(t, entries)
}

func TestProductMetricsServiceChildEnvAgentScriptExplicitStoreEnv(t *testing.T) {
	installProductMetricsDirectChildSpyCommand(t, "gc")
	dir := t.TempDir()
	entries := captureProductMetricsDirectChildEnv(t, func() error {
		env := append([]string(nil), os.Environ()...)
		env = append(env, execenv.UsageMetricsDisableEnv+"=hostile-explicit-late-value")
		return runAgentScriptCommandInStore(io.Discard, io.Discard, dir, env, "gc", "mail", "send", "worker", "subject")
	})
	assertProductMetricsDirectChildEnv(t, entries)
	if got := valuesForProductMetricsDirectChildKey(entries, "PWD"); !slices.Equal(got, []string{dir}) {
		t.Fatalf("agent-script explicit-store child PWD values = %#v, want [%q]", got, dir)
	}
}

func TestProductMetricsServiceChildEnvAgentScriptBDIsUnaffected(t *testing.T) {
	installProductMetricsDirectChildSpyCommand(t, "bd")
	entries := captureProductMetricsDirectChildEnv(t, func() error {
		return runAgentScriptCommand(io.Discard, io.Discard, "bd", "show", "gc-test")
	})
	if got := valuesForProductMetricsDirectChildKey(entries, execenv.UsageMetricsDisableEnv); !slices.Equal(got, []string{"0"}) {
		t.Fatalf("agent-script bd child %s values = %#v, want inherited [0]", execenv.UsageMetricsDisableEnv, got)
	}
	assertProductMetricsDirectChildUnrelatedEnv(t, entries)
}

func TestProductMetricsServiceChildEnvGeneratedSupervisorFiles(t *testing.T) {
	const (
		hostileAmbientExplicitValue = "hostile-ambient-explicit-value"
		hostileSecretsValue         = "hostile-secrets-value"
		hostileLaunchctlValue       = "hostile-launchctl-value"
	)
	hostileValues := []string{
		hostileAmbientExplicitValue,
		hostileSecretsValue,
		hostileLaunchctlValue,
	}
	tests := []struct {
		name             string
		inheritedGCValue string
		explicitEnvKeys  []string
		hostileSources   bool
	}{
		{
			name:             "hostile explicit opt-in is replaced",
			inheritedGCValue: hostileAmbientExplicitValue,
			explicitEnvKeys:  []string{execenv.UsageMetricsDisableEnv, "BD_DISABLE_METRICS", "OTEL_SERVICE_NAME"},
			hostileSources:   true,
		},
		{
			name:            "unset opt-out is added",
			explicitEnvKeys: []string{"BD_DISABLE_METRICS", "OTEL_SERVICE_NAME"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("GC_HOME", t.TempDir())
			t.Setenv("XDG_RUNTIME_DIR", t.TempDir())
			t.Setenv(supervisorOmitProviderCredsEnv, "1")
			t.Setenv("GC_SUPERVISOR_ENV", strings.Join(tc.explicitEnvKeys, " "))
			t.Setenv(execenv.UsageMetricsDisableEnv, tc.inheritedGCValue)
			if tc.inheritedGCValue == "" {
				if err := os.Unsetenv(execenv.UsageMetricsDisableEnv); err != nil {
					t.Fatalf("unset %s: %v", execenv.UsageMetricsDisableEnv, err)
				}
			}
			t.Setenv("BD_DISABLE_METRICS", "keep-beads-setting")
			t.Setenv("OTEL_SERVICE_NAME", "keep-otel-setting")
			if tc.hostileSources {
				writeSupervisorSecretsEnvFile(t, execenv.UsageMetricsDisableEnv+"="+hostileSecretsValue+"\n")
			}

			launchctlGCProbes := 0
			previousLaunchctlGetenv := supervisorLaunchctlGetenv
			supervisorLaunchctlGetenv = func(key string) string {
				if key == execenv.UsageMetricsDisableEnv {
					launchctlGCProbes++
					if tc.hostileSources {
						return hostileLaunchctlValue
					}
				}
				return ""
			}
			t.Cleanup(func() { supervisorLaunchctlGetenv = previousLaunchctlGetenv })

			if explicitKeys := supervisorServiceExplicitEnvKeys(os.Getenv("GC_SUPERVISOR_ENV")); slices.Contains(explicitKeys, execenv.UsageMetricsDisableEnv) {
				t.Fatalf("fixed service key %s accepted via GC_SUPERVISOR_ENV: %#v", execenv.UsageMetricsDisableEnv, explicitKeys)
			}

			data, err := buildSupervisorServiceData()
			if err != nil {
				t.Fatalf("buildSupervisorServiceData: %v", err)
			}
			if launchctlGCProbes != 0 {
				t.Fatalf("launchctl getenv probes for fixed service key %s = %d, want 0", execenv.UsageMetricsDisableEnv, launchctlGCProbes)
			}
			counts := make(map[string]int)
			values := make(map[string]string)
			for _, item := range data.ExtraEnv {
				counts[item.Name]++
				values[item.Name] = item.Value
			}
			for key, want := range map[string]string{
				"BD_DISABLE_METRICS": "keep-beads-setting",
				"OTEL_SERVICE_NAME":  "keep-otel-setting",
			} {
				if counts[key] != 1 || values[key] != want {
					t.Fatalf("supervisor ExtraEnv %s = count %d value %q, want count 1 value %q", key, counts[key], values[key], want)
				}
			}
			for _, hostileValue := range hostileValues {
				for _, item := range data.ExtraEnv {
					if strings.Contains(item.Value, hostileValue) {
						t.Fatalf("supervisor ExtraEnv retained hostile value %q in %s=%q", hostileValue, item.Name, item.Value)
					}
				}
			}

			systemdContent, err := renderSupervisorTemplate(supervisorSystemdTemplate, data)
			if err != nil {
				t.Fatalf("render systemd supervisor service: %v", err)
			}
			assertGeneratedSupervisorEnvironment(t, "systemd", systemdContent, map[string]string{
				execenv.UsageMetricsDisableEnv: execenv.UsageMetricsDisableValue,
				"BD_DISABLE_METRICS":           "keep-beads-setting",
				"OTEL_SERVICE_NAME":            "keep-otel-setting",
			}, func(key, value string) string {
				return "Environment=" + systemdEnv(key, value)
			})
			wantExecStart := "ExecStart=" + supervisorSystemdQuotePath(data.GCPath) + " supervisor run"
			if !strings.Contains(systemdContent, wantExecStart) {
				t.Fatalf("systemd service missing unchanged %q:\n%s", wantExecStart, systemdContent)
			}
			for _, hostileValue := range hostileValues {
				if strings.Contains(systemdContent, hostileValue) {
					t.Fatalf("systemd service retained hostile value %q:\n%s", hostileValue, systemdContent)
				}
			}

			launchdContent, err := renderSupervisorTemplate(supervisorLaunchdTemplate, data)
			if err != nil {
				t.Fatalf("render launchd supervisor service: %v", err)
			}
			assertGeneratedSupervisorEnvironment(t, "launchd", launchdContent, map[string]string{
				execenv.UsageMetricsDisableEnv: execenv.UsageMetricsDisableValue,
				"BD_DISABLE_METRICS":           "keep-beads-setting",
				"OTEL_SERVICE_NAME":            "keep-otel-setting",
			}, func(key, value string) string {
				return "<key>" + xmlEscape(key) + "</key>\n        <string>" + xmlEscape(value) + "</string>"
			})
			for _, argument := range []string{data.GCPath, "supervisor", "run"} {
				if !strings.Contains(launchdContent, "<string>"+xmlEscape(argument)+"</string>") {
					t.Fatalf("launchd service missing unchanged program argument %q:\n%s", argument, launchdContent)
				}
			}
			for _, hostileValue := range hostileValues {
				if strings.Contains(launchdContent, hostileValue) {
					t.Fatalf("launchd service retained hostile value %q:\n%s", hostileValue, launchdContent)
				}
			}
		})
	}
}

func assertGeneratedSupervisorEnvironment(
	t *testing.T,
	manager string,
	content string,
	want map[string]string,
	assignment func(string, string) string,
) {
	t.Helper()
	for key, value := range want {
		var keyEntry string
		switch manager {
		case "systemd":
			keyEntry = "Environment=" + key + "="
		case "launchd":
			keyEntry = "<key>" + xmlEscape(key) + "</key>"
		default:
			t.Fatalf("unknown supervisor service manager %q", manager)
		}
		if count := strings.Count(content, keyEntry); count != 1 {
			t.Fatalf("%s service key %q count = %d, want 1:\n%s", manager, key, count, content)
		}
		entry := assignment(key, value)
		if count := strings.Count(content, entry); count != 1 {
			t.Fatalf("%s service assignment %q count = %d, want 1:\n%s", manager, entry, count, content)
		}
	}
}
