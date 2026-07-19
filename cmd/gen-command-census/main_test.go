package main

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/commandcensus"
)

func TestRunGeneratorCheckDetectsDriftAndWriteConverges(t *testing.T) {
	root := t.TempDir()
	writeGeneratorFixture(t, root)
	options := generatorOptions{Root: root}
	if err := runGenerator(options); err != nil {
		t.Fatal(err)
	}
	if err := runGenerator(generatorOptions{Root: root, Check: true}); err != nil {
		t.Fatalf("fresh check: %v", err)
	}

	runtimePath := filepath.Join(root, "cmd/gc/metrics_census_gen.go")
	if err := os.WriteFile(runtimePath, []byte("stale\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := runGenerator(generatorOptions{Root: root, Check: true}); err == nil || !strings.Contains(err.Error(), "metrics_census_gen.go") {
		t.Fatalf("stale check error = %v", err)
	}
}

func TestCommittedCommandCensusArtifactsAreFresh(t *testing.T) {
	_, source, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller could not locate repository")
	}
	root := filepath.Clean(filepath.Join(filepath.Dir(source), "../.."))
	if err := runGenerator(generatorOptions{Root: root, Check: true}); err != nil {
		t.Fatal(err)
	}
}

func writeGeneratorFixture(t *testing.T, root string) {
	t.Helper()
	for _, dir := range []string{"cmd/gc", "internal/productmetrics", "schemas/metrics/example"} {
		if err := os.MkdirAll(filepath.Join(root, dir), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	manifest := strings.Replace(validGeneratorManifest, "ROOT", "gc", 1)
	if err := os.WriteFile(filepath.Join(root, "cmd/gc/productmetrics_command_census.json"), []byte(manifest), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "schemas/metrics/example/result.schema.json"), []byte(generatorTestSchema), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "internal/productmetrics/command_ids_gen.go"), []byte(commandcensus.S1BootstrapCatalog), 0o644); err != nil {
		t.Fatal(err)
	}
}

const validGeneratorManifest = `{
  "schema_version":1,"next_id":5,
  "permanent_ids":[{"name":"help","id":1,"wire":"help"},{"name":"version","id":2,"wire":"version"},{"name":"unknown","id":3,"wire":"unknown"},{"name":"pack-command","id":4,"wire":"pack-command"}],
  "global_conditional_modes":["generic-machine-output","managed-context","provider-hook"],
  "commands":[{"path":"ROOT","aliases":[],"conditional_modes":[],"hidden":false,"effective_hidden":false,"disable_flag_parsing":false,"shape":"runnable-group","classification":"help","canonical_target":"@help","mode":"standard","notice_policy":"eligible","recording_policy":"recordable","owner":"deferred","resolver":"root-dispatch","deferred_default":"help","id":1}],
  "synthetic":[{"path":"gc <unknown>","aliases":[],"conditional_modes":[],"hidden":false,"effective_hidden":false,"disable_flag_parsing":false,"shape":"runnable","classification":"unknown","mode":"standard","notice_policy":"eligible","recording_policy":"recordable","owner":"deferred","resolver":"root-dispatch","id":3},{"path":"gc <pack-command>","aliases":[],"conditional_modes":[],"hidden":false,"effective_hidden":false,"disable_flag_parsing":false,"shape":"runnable","classification":"pack-command","mode":"pack-command","notice_policy":"ineligible","recording_policy":"recordable","owner":"deferred","resolver":"pack-dispatch","id":4},{"path":"gc __complete","aliases":["__completeNoDesc"],"conditional_modes":[],"hidden":true,"effective_hidden":true,"disable_flag_parsing":true,"shape":"runnable","classification":"excluded","mode":"private-completion","notice_policy":"ineligible","recording_policy":"excluded","owner":"excluded","exclusion":"private-completion"}],
  "tombstones":[]
}`

const generatorTestSchema = `{"properties":{"events":{"items":{"properties":{"command_id":{"enum":["help","version","unknown","pack-command"]}}}}}}`
