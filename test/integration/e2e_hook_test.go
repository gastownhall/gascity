//go:build integration

package integration

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestE2E_Hook_NoWork verifies that gc hook exits 1 when the work query
// returns no output.
func TestE2E_Hook_NoWork(t *testing.T) {
	city := e2eCity{
		Agents: []e2eAgent{
			{
				Name:         "hooker",
				StartCommand: e2eSleepScript(),
				WorkQuery:    "exit 1",
			},
		},
	}
	cityDir := setupE2ECity(t, nil, city)

	_, err := gc(cityDir, "hook", "hooker")
	if err == nil {
		t.Error("gc hook should exit non-zero when work query fails")
	}
}

// TestE2E_Hook_WithWork verifies that gc hook exits 0 and outputs the
// work query result when work is available.
func TestE2E_Hook_WithWork(t *testing.T) {
	city := e2eCity{
		Agents: []e2eAgent{
			{
				Name:         "worker",
				StartCommand: e2eSleepScript(),
				WorkQuery:    "echo 'hook test work available'",
			},
		},
	}
	cityDir := setupE2ECity(t, nil, city)

	// gc hook should find work (echo always succeeds).
	out, err := gc(cityDir, "hook", "worker")
	if err != nil {
		t.Fatalf("gc hook should exit 0 with work: %v\noutput: %s", err, out)
	}
	if !strings.Contains(out, "hook test work available") {
		t.Errorf("hook output should contain work query result:\n%s", out)
	}
}

// TestE2E_Hook_Inject verifies that gc hook --inject is silent legacy
// compatibility and does not run the configured work query.
func TestE2E_Hook_Inject(t *testing.T) {
	const markerName = "inject-work-query-ran"
	const armEnv = "GC_E2E_HOOK_INJECT_ARM"
	armValue := uniqueCityName()
	city := e2eCity{
		Agents: []e2eAgent{
			{
				Name:         "injectee",
				StartCommand: e2eSleepScript(),
				WorkQuery:    "if [ \"${" + armEnv + ":-}\" = \"" + armValue + "\" ]; then touch .gc/" + markerName + " && echo 'inject hook work items'; fi",
			},
		},
	}
	cityDir := setupE2ECityNoStart(t, city)
	markerPath := filepath.Join(cityDir, ".gc", markerName)
	if _, err := os.Stat(markerPath); err == nil {
		t.Fatalf("work_query marker exists before gc hook --inject: %s", markerPath)
	} else if !os.IsNotExist(err) {
		t.Fatalf("checking pre-hook work_query marker: %v", err)
	}

	env := commandEnvForDir(cityDir, false)
	env = append(env, armEnv+"="+armValue)
	out, err := runGCWithEnv(env, cityDir, "hook", "--inject", "injectee")
	if err != nil {
		t.Fatalf("gc hook --inject should exit 0: %v\noutput: %s", err, out)
	}
	if strings.TrimSpace(out) != "" {
		t.Errorf("gc hook --inject should be silent, got:\n%s", out)
	}

	if _, err := os.Stat(markerPath); err == nil {
		t.Fatalf("gc hook --inject ran work_query; marker exists at %s", markerPath)
	} else if !os.IsNotExist(err) {
		t.Fatalf("checking work_query marker: %v", err)
	}

	hookEnv := filterEnvMany(commandEnvForDir(cityDir, false),
		"GC_RIG",
		"GC_RIG_ROOT",
		"GC_CITY",
		"GC_CITY_PATH",
		"GC_CITY_ROOT",
		"GC_CITY_RUNTIME_DIR",
	)
	hookEnv = append(hookEnv, armEnv+"="+armValue)
	out, err = runGCWithEnv(hookEnv, cityDir, "hook", "injectee")
	if err != nil {
		t.Fatalf("gc hook should run armed work_query: %v\noutput: %s", err, out)
	}
	if !strings.Contains(out, "inject hook work items") {
		t.Fatalf("gc hook output missing armed work query result:\n%s", out)
	}
	if _, err := os.Stat(markerPath); err != nil {
		t.Fatalf("normal gc hook did not create work_query marker: %v", err)
	}
}
