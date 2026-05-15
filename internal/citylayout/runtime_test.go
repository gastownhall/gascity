package citylayout

import (
	"path/filepath"
	"testing"
)

func TestCityRuntimeEnv(t *testing.T) {
	cityRoot := "/city"

	got := CityRuntimeEnv(cityRoot)
	want := map[string]string{
		"GC_CITY":                             cityRoot,
		"GC_CITY_PATH":                        cityRoot,
		"GC_CITY_RUNTIME_DIR":                 "/city/.gc/runtime",
		"GC_CONTROL_DISPATCHER_TRACE_DEFAULT": "/city/.gc/runtime/control-dispatcher-trace.log",
	}

	lookup := make(map[string]string, len(got))
	for _, entry := range got {
		for i := 0; i < len(entry); i++ {
			if entry[i] == '=' {
				lookup[entry[:i]] = entry[i+1:]
				break
			}
		}
	}

	for key, expected := range want {
		if lookup[key] != expected {
			t.Fatalf("%s = %q, want %q", key, lookup[key], expected)
		}
	}
}

func TestPackRuntimeEnv(t *testing.T) {
	cityRoot := "/city"

	got := PackRuntimeEnv(cityRoot, "rlm")
	want := map[string]string{
		"GC_CITY":                             cityRoot,
		"GC_CITY_PATH":                        cityRoot,
		"GC_CITY_RUNTIME_DIR":                 "/city/.gc/runtime",
		"GC_CONTROL_DISPATCHER_TRACE_DEFAULT": "/city/.gc/runtime/control-dispatcher-trace.log",
		"GC_PACK_STATE_DIR":                   "/city/.gc/runtime/packs/rlm",
	}

	lookup := make(map[string]string, len(got))
	for _, entry := range got {
		for i := 0; i < len(entry); i++ {
			if entry[i] == '=' {
				lookup[entry[:i]] = entry[i+1:]
				break
			}
		}
	}

	for key, expected := range want {
		if lookup[key] != expected {
			t.Fatalf("%s = %q, want %q", key, lookup[key], expected)
		}
	}
}

func TestPackRuntimeEnvMapWithoutPackName(t *testing.T) {
	got := PackRuntimeEnvMap("/city", "")
	if got["GC_CITY_RUNTIME_DIR"] != "/city/.gc/runtime" {
		t.Fatalf("GC_CITY_RUNTIME_DIR = %q, want %q", got["GC_CITY_RUNTIME_DIR"], "/city/.gc/runtime")
	}
	if got["GC_CONTROL_DISPATCHER_TRACE_DEFAULT"] != "/city/.gc/runtime/control-dispatcher-trace.log" {
		t.Fatalf("GC_CONTROL_DISPATCHER_TRACE_DEFAULT = %q, want %q", got["GC_CONTROL_DISPATCHER_TRACE_DEFAULT"], "/city/.gc/runtime/control-dispatcher-trace.log")
	}
	if _, ok := got["GC_PACK_STATE_DIR"]; ok {
		t.Fatal("GC_PACK_STATE_DIR should be omitted when pack name is empty")
	}
}

func TestCityRuntimeEnvForRuntimeDir(t *testing.T) {
	t.Run("preserves external runtime root", func(t *testing.T) {
		cityRoot := "/city"
		runtimeDir := "/var/tmp/gascity-runtime"
		got := CityRuntimeEnvMapForRuntimeDir(cityRoot, runtimeDir)
		if got["GC_CITY_RUNTIME_DIR"] != runtimeDir {
			t.Fatalf("GC_CITY_RUNTIME_DIR = %q, want %q", got["GC_CITY_RUNTIME_DIR"], runtimeDir)
		}
		wantTrace := filepath.Join(runtimeDir, "control-dispatcher-trace.log")
		if got["GC_CONTROL_DISPATCHER_TRACE_DEFAULT"] != wantTrace {
			t.Fatalf("GC_CONTROL_DISPATCHER_TRACE_DEFAULT = %q, want %q", got["GC_CONTROL_DISPATCHER_TRACE_DEFAULT"], wantTrace)
		}
	})

	t.Run("coerces watcher-visible in-city root", func(t *testing.T) {
		cityRoot := "/city"
		runtimeDir := "/city/rigs/alpha"
		got := CityRuntimeEnvMapForRuntimeDir(cityRoot, runtimeDir)
		if got["GC_CITY_RUNTIME_DIR"] != runtimeDir {
			t.Fatalf("GC_CITY_RUNTIME_DIR = %q, want %q", got["GC_CITY_RUNTIME_DIR"], runtimeDir)
		}
		if got["GC_CONTROL_DISPATCHER_TRACE_DEFAULT"] != "/city/.gc/runtime/control-dispatcher-trace.log" {
			t.Fatalf("GC_CONTROL_DISPATCHER_TRACE_DEFAULT = %q, want %q", got["GC_CONTROL_DISPATCHER_TRACE_DEFAULT"], "/city/.gc/runtime/control-dispatcher-trace.log")
		}
	})
}

func TestControlDispatcherTraceLogFileName(t *testing.T) {
	cases := []struct {
		name          string
		qualifiedName string
		want          string
	}{
		{
			name:          "city dispatcher (no rig)",
			qualifiedName: "control-dispatcher",
			want:          "control-dispatcher-trace.log",
		},
		{
			name:          "rig dispatcher uses double-dash",
			qualifiedName: "app/control-dispatcher",
			want:          "app--control-dispatcher-trace.log",
		},
		{
			name:          "rig name with hyphen preserved",
			qualifiedName: "my-rig/control-dispatcher",
			want:          "my-rig--control-dispatcher-trace.log",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := ControlDispatcherTraceLogFileName(tc.qualifiedName); got != tc.want {
				t.Fatalf("ControlDispatcherTraceLogFileName(%q) = %q, want %q", tc.qualifiedName, got, tc.want)
			}
		})
	}
}

func TestControlDispatcherTraceDefaultPathFor(t *testing.T) {
	cases := []struct {
		name          string
		cityRoot      string
		qualifiedName string
		want          string
	}{
		{
			name:          "city dispatcher",
			cityRoot:      "/city",
			qualifiedName: "control-dispatcher",
			want:          "/city/.gc/runtime/control-dispatcher-trace.log",
		},
		{
			name:          "rig dispatcher",
			cityRoot:      "/city",
			qualifiedName: "app/control-dispatcher",
			want:          "/city/.gc/runtime/app--control-dispatcher-trace.log",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := ControlDispatcherTraceDefaultPathFor(tc.cityRoot, tc.qualifiedName); got != tc.want {
				t.Fatalf("ControlDispatcherTraceDefaultPathFor(%q, %q) = %q, want %q", tc.cityRoot, tc.qualifiedName, got, tc.want)
			}
		})
	}
}

func TestControlDispatcherTraceDefaultPathForRuntimeDirAndName(t *testing.T) {
	t.Run("external runtime root preserved", func(t *testing.T) {
		cityRoot := "/city"
		runtimeDir := "/var/tmp/gascity-runtime"
		got := ControlDispatcherTraceDefaultPathForRuntimeDirAndName(cityRoot, runtimeDir, "app/control-dispatcher")
		want := "/var/tmp/gascity-runtime/app--control-dispatcher-trace.log"
		if got != want {
			t.Fatalf("got %q, want %q", got, want)
		}
	})

	t.Run("watcher-visible in-city root coerced", func(t *testing.T) {
		cityRoot := "/city"
		runtimeDir := "/city/rigs/alpha"
		got := ControlDispatcherTraceDefaultPathForRuntimeDirAndName(cityRoot, runtimeDir, "app/control-dispatcher")
		want := "/city/.gc/runtime/app--control-dispatcher-trace.log"
		if got != want {
			t.Fatalf("got %q, want %q", got, want)
		}
	})
}

func TestTrustedAmbientCityRuntimeDirAcceptsLegacyCityRootAnchor(t *testing.T) {
	cityRoot := t.TempDir()
	runtimeDir := filepath.Join(cityRoot, ".gc", "runtime")

	t.Setenv("GC_CITY", "")
	t.Setenv("GC_CITY_PATH", "")
	t.Setenv("GC_CITY_ROOT", cityRoot)
	t.Setenv("GC_CITY_RUNTIME_DIR", runtimeDir)

	if got := TrustedAmbientCityRuntimeDir(cityRoot); got != runtimeDir {
		t.Fatalf("TrustedAmbientCityRuntimeDir() = %q, want %q", got, runtimeDir)
	}
}

func TestPublishedServicesDir(t *testing.T) {
	if got := PublishedServicesDir("/city"); got != "/city/.gc/services/.published" {
		t.Fatalf("PublishedServicesDir = %q, want %q", got, "/city/.gc/services/.published")
	}
}

func TestSessionNameLocksDir(t *testing.T) {
	if got := SessionNameLocksDir("/city"); got != "/city/.gc/session-name-locks" {
		t.Fatalf("SessionNameLocksDir = %q, want %q", got, "/city/.gc/session-name-locks")
	}
}

func TestPublicServiceMountPath(t *testing.T) {
	tests := []struct {
		name        string
		cityName    string
		serviceName string
		want        string
	}{
		{
			name:        "happy path",
			cityName:    "test-city",
			serviceName: "slack",
			want:        "/v0/city/test-city/svc/slack",
		},
		{
			name:        "city with hyphens",
			cityName:    "demo-app",
			serviceName: "bridge",
			want:        "/v0/city/demo-app/svc/bridge",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := PublicServiceMountPath(tt.cityName, tt.serviceName); got != tt.want {
				t.Errorf("PublicServiceMountPath(%q, %q) = %q, want %q",
					tt.cityName, tt.serviceName, got, tt.want)
			}
		})
	}
}
