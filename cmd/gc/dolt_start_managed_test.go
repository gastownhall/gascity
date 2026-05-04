package main

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	bdpack "github.com/gastownhall/gascity/examples/bd"
)

func TestDoltServerEnv_AppendsDefaultWhenMissing(t *testing.T) {
	parent := []string{"PATH=/usr/bin", "HOME=/home/test"}
	out := doltServerEnv(parent)

	want := "DOLT_GC_SCHEDULER=NONE"
	found := false
	for _, kv := range out {
		if kv == want {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("expected %q in env, got %v", want, out)
	}
	// Original entries preserved.
	for _, kv := range parent {
		var hit bool
		for _, got := range out {
			if got == kv {
				hit = true
				break
			}
		}
		if !hit {
			t.Fatalf("parent entry %q missing from output env %v", kv, out)
		}
	}
}

func TestDoltServerEnv_RespectsUserOverride(t *testing.T) {
	parent := []string{"PATH=/usr/bin", "DOLT_GC_SCHEDULER=LOADAVG", "HOME=/home/test"}
	out := doltServerEnv(parent)

	// User-provided value must be preserved exactly.
	count := 0
	for _, kv := range out {
		if kv == "DOLT_GC_SCHEDULER=LOADAVG" {
			count++
		}
		if kv == "DOLT_GC_SCHEDULER=NONE" {
			t.Fatalf("user override clobbered by default: %v", out)
		}
	}
	if count != 1 {
		t.Fatalf("expected exactly one DOLT_GC_SCHEDULER=LOADAVG entry, got %d in %v", count, out)
	}
}

func TestDoltServerEnv_RespectsEmptyUserValue(t *testing.T) {
	// An explicit empty value (DOLT_GC_SCHEDULER=) is still a user
	// override and we must not replace it.
	parent := []string{"DOLT_GC_SCHEDULER="}
	out := doltServerEnv(parent)
	for _, kv := range out {
		if kv == "DOLT_GC_SCHEDULER=NONE" {
			t.Fatalf("explicit empty-value override clobbered: %v", out)
		}
	}
}

func TestGCBeadsBDScript_RespectsEmptyUserValue(t *testing.T) {
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller(0) failed")
	}
	scriptPath := filepath.Join(filepath.Dir(thisFile), "..", "..", "examples", "bd", "assets", "scripts", "gc-beads-bd.sh")
	data, err := os.ReadFile(scriptPath)
	if err != nil {
		t.Fatalf("read %s: %v", scriptPath, err)
	}
	script := string(data)

	if !strings.Contains(script, `${DOLT_GC_SCHEDULER=NONE}`) {
		t.Fatalf("gc-beads-bd.sh must default DOLT_GC_SCHEDULER only when unset")
	}
	if strings.Contains(script, `${DOLT_GC_SCHEDULER:=NONE}`) {
		t.Fatalf("gc-beads-bd.sh must not clobber an explicitly empty DOLT_GC_SCHEDULER")
	}
}

func TestGCBeadsBDScript_UsesPortableSleepMS(t *testing.T) {
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller(0) failed")
	}
	scriptPath := filepath.Join(filepath.Dir(thisFile), "..", "..", "examples", "bd", "assets", "scripts", "gc-beads-bd.sh")
	data, err := os.ReadFile(scriptPath)
	if err != nil {
		t.Fatalf("read %s: %v", scriptPath, err)
	}
	script := string(data)
	embedded, err := bdpack.PackFS.ReadFile("assets/scripts/gc-beads-bd.sh")
	if err != nil {
		t.Fatalf("read embedded gc-beads-bd.sh: %v", err)
	}
	if string(embedded) != script {
		t.Fatalf("embedded gc-beads-bd.sh differs from source script")
	}

	if !strings.Contains(script, "sleep_ms()") {
		t.Fatalf("gc-beads-bd.sh must define portable sleep_ms helper")
	}
	if strings.Contains(script, `sleep "$(awk`) {
		t.Fatalf("gc-beads-bd.sh must not use awk to calculate sleep durations")
	}
	if got := strings.Count(script, `sleep_ms "$backoff_ms" 2>/dev/null || sleep 1`); got < 3 {
		t.Fatalf("gc-beads-bd.sh must use sleep_ms for retry backoff sleeps; found %d call sites", got)
	}
	if !strings.Contains(script, "for attempt in 1 2 3 4 5 6 7 8; do") {
		t.Fatalf("gc-beads-bd.sh must allow slow bd runtime schema visibility after init")
	}
}

func TestGCBeadsBDScript_QuarantinesRetiredReplacementDatabases(t *testing.T) {
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller(0) failed")
	}
	scriptPath := filepath.Join(filepath.Dir(thisFile), "..", "..", "examples", "bd", "assets", "scripts", "gc-beads-bd.sh")
	data, err := os.ReadFile(scriptPath)
	if err != nil {
		t.Fatalf("read %s: %v", scriptPath, err)
	}
	script := string(data)

	required := []string{
		"retired_replacement_db_name()",
		"?*.replaced-[0-9][0-9][0-9][0-9][0-9][0-9][0-9][0-9]T[0-9][0-9][0-9][0-9][0-9][0-9]Z)",
		`reason="retired replacement"`,
		`quarantining unservable database`,
		`mv -f "$dir" "$quarantine_dir"`,
	}
	for _, want := range required {
		if !strings.Contains(script, want) {
			t.Fatalf("gc-beads-bd.sh missing retired replacement fallback fragment %q", want)
		}
	}
	if strings.Contains(script, "quarantining phantom database") {
		t.Fatal("gc-beads-bd.sh still logs the broader fallback as phantom-only")
	}
}

func TestManagedDoltStartFields(t *testing.T) {
	report := managedDoltStartReport{
		Ready:        true,
		PID:          4321,
		Port:         3312,
		AddressInUse: false,
		Attempts:     2,
	}
	fields := managedDoltStartFields(report)
	want := []string{
		"ready\ttrue",
		"pid\t4321",
		"port\t3312",
		"address_in_use\tfalse",
		"attempts\t2",
	}
	if len(fields) != len(want) {
		t.Fatalf("got %d fields, want %d", len(fields), len(want))
	}
	for i, w := range want {
		if fields[i] != w {
			t.Errorf("fields[%d] = %q, want %q", i, fields[i], w)
		}
	}
}

func TestManagedDoltLogSize(t *testing.T) {
	t.Run("existing file", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "dolt.log")
		if err := os.WriteFile(path, []byte("hello world\n"), 0o644); err != nil {
			t.Fatalf("WriteFile: %v", err)
		}
		got, err := managedDoltLogSize(path)
		if err != nil {
			t.Fatalf("managedDoltLogSize: %v", err)
		}
		if got != 12 {
			t.Errorf("managedDoltLogSize = %d, want 12", got)
		}
	})

	t.Run("missing file returns zero", func(t *testing.T) {
		got, err := managedDoltLogSize(filepath.Join(t.TempDir(), "no-such.log"))
		if err != nil {
			t.Fatalf("managedDoltLogSize: %v", err)
		}
		if got != 0 {
			t.Errorf("managedDoltLogSize = %d, want 0", got)
		}
	})
}

func TestManagedDoltLogSuffix(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "dolt.log")
	content := "line one\nline two\nline three\n"
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	t.Run("from offset", func(t *testing.T) {
		got, err := managedDoltLogSuffix(path, 9)
		if err != nil {
			t.Fatalf("managedDoltLogSuffix: %v", err)
		}
		if got != "line two\nline three\n" {
			t.Errorf("got %q, want %q", got, "line two\nline three\n")
		}
	})

	t.Run("offset past end returns empty", func(t *testing.T) {
		got, err := managedDoltLogSuffix(path, int64(len(content)+10))
		if err != nil {
			t.Fatalf("managedDoltLogSuffix: %v", err)
		}
		if got != "" {
			t.Errorf("got %q, want empty", got)
		}
	})

	t.Run("negative offset treated as zero", func(t *testing.T) {
		got, err := managedDoltLogSuffix(path, -5)
		if err != nil {
			t.Fatalf("managedDoltLogSuffix: %v", err)
		}
		if got != content {
			t.Errorf("got %q, want %q", got, content)
		}
	})

	t.Run("missing file returns empty", func(t *testing.T) {
		got, err := managedDoltLogSuffix(filepath.Join(dir, "no-such.log"), 0)
		if err != nil {
			t.Fatalf("managedDoltLogSuffix: %v", err)
		}
		if got != "" {
			t.Errorf("got %q, want empty", got)
		}
	})
}

func TestResolveDoltArchiveLevel(t *testing.T) {
	tests := []struct {
		name     string
		explicit int
		envVal   string
		want     int
	}{
		{name: "explicit zero", explicit: 0, want: 0},
		{name: "explicit positive", explicit: 1, want: 1},
		{name: "explicit large", explicit: 42, want: 42},
		{name: "negative defaults to zero", explicit: -1, want: 0},
		{name: "negative with valid env", explicit: -1, envVal: "1", want: 1},
		{name: "negative with env zero", explicit: -1, envVal: "0", want: 0},
		{name: "negative with non-numeric env falls back", explicit: -1, envVal: "abc", want: 0},
		{name: "negative with empty env", explicit: -1, envVal: "", want: 0},
		{name: "explicit overrides env", explicit: 2, envVal: "5", want: 2},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("GC_DOLT_ARCHIVE_LEVEL", tt.envVal)
			if got := resolveDoltArchiveLevel(tt.explicit); got != tt.want {
				t.Errorf("resolveDoltArchiveLevel(%d) = %d, want %d", tt.explicit, got, tt.want)
			}
		})
	}
}
