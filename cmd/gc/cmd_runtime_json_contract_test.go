package main

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/runtime/runtimecontract"
	"github.com/santhosh-tekuri/jsonschema/v6"
	"github.com/spf13/cobra"
)

// runRuntimeConformanceStrict runs `gc runtime conformance` through the root
// command — and so through the CLI's JSON contract gate — with the contract
// enforced, the way a runtime pack's CI invokes it.
func runRuntimeConformanceStrict(t *testing.T, args ...string) (int, []byte) {
	t.Helper()
	clearGCEnv(t)
	t.Setenv("GC_JSON_CONTRACT_STRICT", "1")
	var stdout, stderr bytes.Buffer
	code := run(append([]string{"runtime", "conformance"}, args...), &stdout, &stderr)
	if strings.Contains(stdout.String(), "json_unsupported") || strings.Contains(stderr.String(), "does not declare JSON support") {
		t.Fatalf("gc runtime conformance --json refused by the JSON contract:\nstdout=%s\nstderr=%s", stdout.String(), stderr.String())
	}
	return code, stdout.Bytes()
}

func TestRuntimeConformanceJSONStrictContract_ConformantPasses(t *testing.T) {
	script := conformantConformanceScript(t)
	code, out := runRuntimeConformanceStrict(t, "--json", script)
	if code != 0 {
		t.Fatalf("exit = %d, want 0; stdout=%s", code, out)
	}
	validateJSONAgainstResultSchema(t, []string{"runtime", "conformance"}, out)

	var report runtimecontract.Report
	if err := json.Unmarshal(out, &report); err != nil {
		t.Fatalf("stdout is not a JSON Report: %v\n%s", err, out)
	}
	if len(report.Results) != len(runtimecontract.Catalog()) {
		t.Errorf("report has %d results, want %d", len(report.Results), len(runtimecontract.Catalog()))
	}
}

func TestRuntimeConformanceJSONStrictContract_BrokenReportsFailureEnvelope(t *testing.T) {
	script := writeRPPScript(t, `
case "$1" in
  protocol)   echo '{"version":0,"capabilities":[]}' ;;
  start)      cat >/dev/null ;;
  stop)       ;;
  is-running) echo false ;;
  *) exit 2 ;;
esac
`)
	code, out := runRuntimeConformanceStrict(t, "--json", script)
	if code == 0 {
		t.Fatalf("broken executable must exit non-zero; stdout=%s", out)
	}
	validateJSONAgainstRoleSchema(t, []string{"runtime", "conformance"}, jsonSchemaFailureRole, out)

	var payload struct {
		OK    bool                  `json:"ok"`
		Error jsonSchemaErrorDetail `json:"error"`
		runtimecontract.Report
	}
	if err := json.Unmarshal(out, &payload); err != nil {
		t.Fatalf("stdout is not one JSON document: %v\n%s", err, out)
	}
	if payload.OK || payload.Error.Code != runtimeConformanceFailedCode || payload.Error.ExitCode != code {
		t.Fatalf("failure envelope = ok:%v error:%+v (exit %d), want ok:false code %q", payload.OK, payload.Error, code, runtimeConformanceFailedCode)
	}
	var sawFail bool
	for _, res := range payload.Results {
		if res.Code == runtimecontract.ReqLifecycleStartRunning && res.Status == runtimecontract.StatusFail {
			sawFail = true
		}
	}
	if !sawFail {
		t.Fatalf("failure envelope should carry the report naming the failed requirement:\n%s", out)
	}
}

// --env adds the capability report to the same JSON document; the output
// must stay one document so CI can parse it and the schema can describe it.
func TestRuntimeConformanceJSONStrictContract_EnvIsOneDocument(t *testing.T) {
	script := conformantConformanceScript(t)
	code, out := runRuntimeConformanceStrict(t, "--json", "--env", script)
	if code != 0 {
		t.Fatalf("exit = %d, want 0; stdout=%s", code, out)
	}
	validateJSONAgainstResultSchema(t, []string{"runtime", "conformance"}, out)
	var payload struct {
		Environment *struct {
			Executable string `json:"executable"`
		} `json:"environment"`
	}
	if err := json.Unmarshal(out, &payload); err != nil {
		t.Fatalf("stdout is not one JSON document: %v\n%s", err, out)
	}
	if payload.Environment == nil || payload.Environment.Executable == "" {
		t.Fatalf("--env --json should carry the capability report under environment:\n%s", out)
	}
}

// Every runtime subcommand that offers --json must declare JSON support, or
// the root command's contract gate refuses it under GC_JSON_CONTRACT_STRICT.
func TestRuntimeSubcommandsWithJSONFlagDeclareJSONSupport(t *testing.T) {
	root := newRootCmd(&bytes.Buffer{}, &bytes.Buffer{})
	runtimeCmd, _, err := root.Find([]string{"runtime"})
	if err != nil {
		t.Fatalf("find runtime command: %v", err)
	}
	var checked int
	var walk func(*cobra.Command)
	walk = func(cmd *cobra.Command) {
		for _, sub := range cmd.Commands() {
			walk(sub)
		}
		if cmd.Flags().Lookup("json") == nil {
			return
		}
		checked++
		path := commandPathWords(cmd)
		if _, err := readCommandSchema(cmd, path, jsonSchemaResultRole); err != nil {
			t.Errorf("gc %s has --json but declares no result schema: %v", strings.Join(path, " "), err)
		}
	}
	walk(runtimeCmd)
	if checked == 0 {
		t.Fatal("found no runtime subcommands with --json")
	}
}

func validateJSONAgainstRoleSchema(t *testing.T, command []string, role string, data []byte) {
	t.Helper()
	rawSchema, err := schemaForRole(nil, command, role)
	if err != nil {
		t.Fatalf("read %s schema for %v: %v", role, command, err)
	}
	schemaDoc, err := jsonschema.UnmarshalJSON(bytes.NewReader(rawSchema))
	if err != nil {
		t.Fatalf("parse %s schema for %v: %v", role, command, err)
	}
	instance, err := jsonschema.UnmarshalJSON(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("parse payload for %v: %v\n%s", command, err, data)
	}
	compiler := jsonschema.NewCompiler()
	schemaURL := strings.Join(command, "/") + "/" + role + ".schema.json"
	if err := compiler.AddResource(schemaURL, schemaDoc); err != nil {
		t.Fatalf("add schema resource: %v", err)
	}
	compiled, err := compiler.Compile(schemaURL)
	if err != nil {
		t.Fatalf("compile schema: %v", err)
	}
	if err := compiled.Validate(instance); err != nil {
		t.Fatalf("payload for %v does not validate against %s schema: %v\n%s", command, role, err, data)
	}
}
