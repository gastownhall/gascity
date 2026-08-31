package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/fsys"
	"github.com/gastownhall/gascity/internal/packman"
)

func importStatusLockFixture(t *testing.T, dir string) string {
	t.Helper()
	fetched := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	if err := packman.WriteLockfile(fsys.OSFS{}, dir, &packman.Lockfile{
		Schema: packman.LockfileSchema,
		Packs: map[string]packman.LockedPack{
			"https://example.com/tools.git":  {Version: "1.4.2", Commit: "aaaa", Fetched: fetched},
			"https://example.com/base.git":   {Version: "2.0.0", Commit: "bbbb", Fetched: fetched},
			"https://example.com/worker.git": {Version: "3.1.0", Commit: "cccc", Fetched: fetched},
		},
	}); err != nil {
		t.Fatalf("WriteLockfile: %v", err)
	}
	data, err := os.ReadFile(filepath.Join(dir, packman.LockfileName))
	if err != nil {
		t.Fatalf("ReadFile(packs.lock): %v", err)
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// TestDoImportStatusJSONGolden pins the exact machine-readable document
// emitted by "gc import status --json": the declared import set with
// source paths and lock pins, plus the packs.lock closure and its
// content hash (ga-qcnpu1). Drift checkers consume this instead of
// parsing "gc import list" text.
func TestDoImportStatusJSONGolden(t *testing.T) {
	clearGCEnv(t)
	dir := t.TempDir()
	writeCityToml(t, dir, "[workspace]\nname = \"demo\"\n")
	writePackToml(t, dir, `[pack]
name = "demo"
schema = 1

[imports.tools]
source = "https://example.com/tools.git"
version = "^1.4"

[imports.local]
source = "./packs/local"

[defaults.rig.imports.worker]
source = "https://example.com/worker.git"
version = "^3.0"
`)
	lockSHA := importStatusLockFixture(t, dir)

	var stdout, stderr bytes.Buffer
	code := doImportStatus(dir, true, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("code = %d, stderr = %s", code, stderr.String())
	}

	want := fmt.Sprintf(`{
  "schema_version": "1",
  "ok": true,
  "root": %[1]q,
  "packs_lock_path": %[2]q,
  "packs_lock_sha256": %[3]q,
  "imports": [
    {
      "name": "default-rig:worker",
      "source": "https://example.com/worker.git",
      "constraint": "^3.0",
      "kind": "remote",
      "pin": {
        "version": "3.1.0",
        "commit": "cccc",
        "fetched": "2026-01-02T03:04:05Z"
      }
    },
    {
      "name": "pack:local",
      "source": "./packs/local",
      "kind": "path",
      "path": %[4]q
    },
    {
      "name": "pack:tools",
      "source": "https://example.com/tools.git",
      "constraint": "^1.4",
      "kind": "remote",
      "pin": {
        "version": "1.4.2",
        "commit": "aaaa",
        "fetched": "2026-01-02T03:04:05Z"
      }
    }
  ],
  "locked_packs": [
    {
      "source": "https://example.com/base.git",
      "version": "2.0.0",
      "commit": "bbbb",
      "fetched": "2026-01-02T03:04:05Z"
    },
    {
      "source": "https://example.com/tools.git",
      "version": "1.4.2",
      "commit": "aaaa",
      "fetched": "2026-01-02T03:04:05Z"
    },
    {
      "source": "https://example.com/worker.git",
      "version": "3.1.0",
      "commit": "cccc",
      "fetched": "2026-01-02T03:04:05Z"
    }
  ]
}
`, dir, filepath.Join(dir, packman.LockfileName), lockSHA, filepath.Join(dir, "packs", "local"))

	if got := stdout.String(); got != want {
		t.Fatalf("gc import status --json output mismatch\ngot:\n%s\nwant:\n%s", got, want)
	}
}

// TestDoImportStatusJSONIncludesRigScopedImports asserts the status
// document covers rig-scoped [rigs.imports.*] bindings — the drift
// surface that "gc import list" only shows with an explicit --rig flag.
func TestDoImportStatusJSONIncludesRigScopedImports(t *testing.T) {
	clearGCEnv(t)
	dir := t.TempDir()
	writeCityToml(t, dir, `
[workspace]
name = "demo"

[[rigs]]
name = "myrig"

[rigs.imports.extra]
source = "/opt/packs/extra"
`)
	writePackToml(t, dir, "[pack]\nname = \"demo\"\nschema = 1\n")

	var stdout, stderr bytes.Buffer
	code := doImportStatus(dir, true, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("code = %d, stderr = %s", code, stderr.String())
	}
	out := stdout.String()
	if !strings.Contains(out, `"name": "rig:myrig:extra"`) {
		t.Fatalf("missing rig-scoped import entry:\n%s", out)
	}
	if !strings.Contains(out, `"source": "/opt/packs/extra"`) {
		t.Fatalf("missing rig-scoped import source:\n%s", out)
	}
	if !strings.Contains(out, `"kind": "path"`) {
		t.Fatalf("missing path kind for rig-scoped import:\n%s", out)
	}
}

// TestDoImportStatusJSONOmitsLockHashWhenMissing confirms a city
// without packs.lock emits no packs_lock_sha256 and an empty (not
// null) locked_packs array.
func TestDoImportStatusJSONOmitsLockHashWhenMissing(t *testing.T) {
	clearGCEnv(t)
	dir := t.TempDir()
	writeCityToml(t, dir, "[workspace]\nname = \"demo\"\n")
	writePackToml(t, dir, "[pack]\nname = \"demo\"\nschema = 1\n")

	var stdout, stderr bytes.Buffer
	code := doImportStatus(dir, true, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("code = %d, stderr = %s", code, stderr.String())
	}
	out := stdout.String()
	if strings.Contains(out, "packs_lock_sha256") {
		t.Fatalf("packs_lock_sha256 present despite missing packs.lock:\n%s", out)
	}
	if !strings.Contains(out, `"locked_packs": []`) {
		t.Fatalf("locked_packs should be an empty array:\n%s", out)
	}
	if !strings.Contains(out, `"imports": []`) {
		t.Fatalf("imports should be an empty array:\n%s", out)
	}
}

// TestDoImportStatusTextShowsPins covers the human-readable default:
// one line per import with its lock pin, prefixed by the lock hash.
func TestDoImportStatusTextShowsPins(t *testing.T) {
	clearGCEnv(t)
	dir := t.TempDir()
	writeCityToml(t, dir, "[workspace]\nname = \"demo\"\n")
	writePackToml(t, dir, `[pack]
name = "demo"
schema = 1

[imports.tools]
source = "https://example.com/tools.git"
version = "^1.4"
`)
	lockSHA := importStatusLockFixture(t, dir)

	var stdout, stderr bytes.Buffer
	code := doImportStatus(dir, false, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("code = %d, stderr = %s", code, stderr.String())
	}
	out := stdout.String()
	if !strings.Contains(out, "packs.lock sha256: "+lockSHA) {
		t.Fatalf("missing lock hash line:\n%s", out)
	}
	if !strings.Contains(out, "pack:tools\thttps://example.com/tools.git\t^1.4\tremote\t1.4.2\taaaa") {
		t.Fatalf("missing pinned import line:\n%s", out)
	}
}

// TestImportStatusJSONProductionRun exercises "gc import status --json"
// through the full production CLI path — including the JSON contract
// gate in run(), which rejects --json with json_unsupported unless the
// command declares an embedded result schema — and validates the
// emitted document against that schema. doImportStatus-level tests
// bypass this gate, so only this test proves the machine-readable
// drift surface is reachable in production. The fixture declares a
// pinned remote, an unlocked remote, and a path import so the schema
// validation covers every entry shape the command can emit.
func TestImportStatusJSONProductionRun(t *testing.T) {
	clearGCEnv(t)
	dir := t.TempDir()
	writeCityToml(t, dir, "[workspace]\nname = \"demo\"\n")
	writePackToml(t, dir, `[pack]
name = "demo"
schema = 1

[imports.tools]
source = "https://example.com/tools.git"
version = "^1.4"

[imports.local]
source = "./packs/local"

[imports.unlocked]
source = "https://example.com/unlocked.git"
version = "^9.9"
`)
	importStatusLockFixture(t, dir)

	var stdout, stderr bytes.Buffer
	code := run([]string{"--city", dir, "import", "status", "--json"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("run(import status --json) = %d, stderr=%q stdout=%q", code, stderr.String(), stdout.String())
	}
	if strings.Contains(stdout.String(), "json_unsupported") {
		t.Fatalf("production path rejected --json:\n%s", stdout.String())
	}

	var doc struct {
		Imports []struct {
			Kind string          `json:"kind"`
			Pin  json.RawMessage `json:"pin"`
		} `json:"imports"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &doc); err != nil {
		t.Fatalf("production document is not JSON: %v\n%s", err, stdout.String())
	}
	shapes := map[string]bool{}
	for _, imp := range doc.Imports {
		switch {
		case imp.Kind == "path":
			shapes["path"] = true
		case len(imp.Pin) > 0:
			shapes["pinned remote"] = true
		default:
			shapes["unlocked remote"] = true
		}
	}
	for _, shape := range []string{"pinned remote", "unlocked remote", "path"} {
		if !shapes[shape] {
			t.Fatalf("fixture emitted no %s import entry, so the schema validation no longer covers that shape:\n%s", shape, stdout.String())
		}
	}

	assertTopLevelOKTrue(t, stdout.Bytes())
	validateJSONAgainstResultSchema(t, []string{"import", "status"}, stdout.Bytes())
}

// TestImportStatusJSONSchemaManifest asserts the --json-schema manifest
// reports JSON support for "gc import status" with a valid embedded
// result schema, so drift checkers can discover the contract.
func TestImportStatusJSONSchemaManifest(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := run([]string{"import", "status", "--json-schema"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("run(import status --json-schema) = %d, stderr=%q stdout=%q", code, stderr.String(), stdout.String())
	}
	var manifest struct {
		Command       []string                   `json:"command"`
		JSONSupported bool                       `json:"json_supported"`
		Schemas       map[string]json.RawMessage `json:"schemas"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &manifest); err != nil {
		t.Fatalf("manifest is not JSON: %v\n%s", err, stdout.String())
	}
	if got := strings.Join(manifest.Command, " "); got != "import status" {
		t.Fatalf("manifest command = %q, want \"import status\"", got)
	}
	if !manifest.JSONSupported {
		t.Fatalf("manifest json_supported = false, want true:\n%s", stdout.String())
	}
	if !json.Valid(manifest.Schemas["result"]) {
		t.Fatalf("result schema missing or invalid: %s", manifest.Schemas["result"])
	}
}

// TestImportStatusCommandRegistered asserts "gc import status" is wired
// into the import command tree with a --json flag.
func TestImportStatusCommandRegistered(t *testing.T) {
	var stdout, stderr bytes.Buffer
	cmd := newImportCmd(&stdout, &stderr)
	for _, sub := range cmd.Commands() {
		if sub.Name() == "status" {
			if sub.Flags().Lookup("json") == nil {
				t.Fatal("gc import status is missing the --json flag")
			}
			return
		}
	}
	t.Fatal("gc import status subcommand not registered")
}

// upstreamStatusFixtureCity writes the three-scope import fixture the
// freshness tests share: a pinned remote, an unlocked remote, and a path
// import.
func upstreamStatusFixtureCity(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	writeCityToml(t, dir, "[workspace]\nname = \"demo\"\n")
	writePackToml(t, dir, `[pack]
name = "demo"
schema = 1

[imports.tools]
source = "https://example.com/tools.git"
version = "^1.4"

[imports.local]
source = "./packs/local"

[imports.unlocked]
source = "https://example.com/unlocked.git"
version = "^9.9"
`)
	importStatusLockFixture(t, dir)
	return dir
}

// stubCheckUpstreamImports swaps the freshness seam for a fixed report and
// returns a pointer to the call count, so a test can assert the seam was not
// reached at all.
func stubCheckUpstreamImports(t *testing.T, statuses ...packman.UpstreamStatus) *int {
	t.Helper()
	calls := 0
	prev := checkUpstreamImports
	t.Cleanup(func() { checkUpstreamImports = prev })
	checkUpstreamImports = func(_ string, _ map[string]config.Import, _ *packman.Lockfile) (*packman.UpstreamReport, error) {
		calls++
		return &packman.UpstreamReport{Checked: len(statuses), Statuses: statuses}, nil
	}
	return &calls
}

func upstreamStatusEntry(t *testing.T, doc *ImportStatusJSON, name string) ImportStatusEntry {
	t.Helper()
	for _, entry := range doc.Imports {
		if entry.Name == name {
			return entry
		}
	}
	t.Fatalf("no import entry %q in %#v", name, doc.Imports)
	return ImportStatusEntry{}
}

func decodeImportStatusDoc(t *testing.T, data []byte) *ImportStatusJSON {
	t.Helper()
	var doc ImportStatusJSON
	if err := json.Unmarshal(data, &doc); err != nil {
		t.Fatalf("decoding status document: %v\n%s", err, data)
	}
	return &doc
}

// TestImportStatusWithoutCheckUpstreamStaysOffline is REQ-003 at the command
// layer: the default invocation must not resolve anything over the network,
// and must not emit either new field. The seam is stubbed to fail the test if
// it is reached.
func TestImportStatusWithoutCheckUpstreamStaysOffline(t *testing.T) {
	clearGCEnv(t)
	dir := upstreamStatusFixtureCity(t)

	prev := checkUpstreamImports
	t.Cleanup(func() { checkUpstreamImports = prev })
	checkUpstreamImports = func(_ string, _ map[string]config.Import, _ *packman.Lockfile) (*packman.UpstreamReport, error) {
		t.Fatal("gc import status resolved upstream freshness without --check-upstream")
		return nil, nil
	}

	var stdout, stderr bytes.Buffer
	if code := doImportStatus(dir, true, &stdout, &stderr); code != 0 {
		t.Fatalf("code = %d, stderr = %s", code, stderr.String())
	}
	if strings.Contains(stdout.String(), "upstream") {
		t.Fatalf("default document mentions upstream:\n%s", stdout.String())
	}
	doc := decodeImportStatusDoc(t, stdout.Bytes())
	if doc.Upstream != nil {
		t.Fatalf("Upstream = %#v, want nil", doc.Upstream)
	}
	for _, entry := range doc.Imports {
		if entry.Upstream != nil {
			t.Fatalf("entry %q carries upstream %#v, want nil", entry.Name, entry.Upstream)
		}
	}
}

// TestImportStatusCheckUpstreamJSONGolden is AC-06: the document under
// --check-upstream carries, for each import, the fields a drift checker needs
// to act -- name, source, constraint, the pin it holds, and what upstream
// resolved to -- and validates against the schema whose verdict is an enum.
func TestImportStatusCheckUpstreamJSONGolden(t *testing.T) {
	clearGCEnv(t)
	dir := upstreamStatusFixtureCity(t)
	stubCheckUpstreamImports(t,
		packman.UpstreamStatus{
			Name: "pack:local", Source: "./packs/local", Verdict: packman.UpstreamNotApplicable,
		},
		packman.UpstreamStatus{
			Name: "pack:tools", Source: "https://example.com/tools.git", Constraint: "^1.4",
			LockedCommit: "aaaa", ResolvedRef: "1.9.0", ResolvedCommit: "dddd",
			Verdict: packman.UpstreamBehind,
		},
		packman.UpstreamStatus{
			Name: "pack:unlocked", Source: "https://example.com/unlocked.git", Constraint: "^9.9",
			Verdict: packman.UpstreamNotApplicable,
			Err:     fmt.Errorf("no packs.lock entry for %q; run \"gc import install\"", "https://example.com/unlocked.git"),
		},
	)

	var stdout, stderr bytes.Buffer
	code := run([]string{"--city", dir, "import", "status", "--json", "--check-upstream"}, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("code = %d, want 1 for a behind import; stderr=%s", code, stderr.String())
	}

	doc := decodeImportStatusDoc(t, stdout.Bytes())
	tools := upstreamStatusEntry(t, doc, "pack:tools")
	if tools.Source != "https://example.com/tools.git" || tools.Constraint != "^1.4" {
		t.Fatalf("tools entry = %#v", tools)
	}
	if tools.Pin == nil || tools.Pin.Commit != "aaaa" || tools.Pin.Fetched != "2026-01-02T03:04:05Z" {
		t.Fatalf("tools pin = %#v, want commit aaaa with a fetched timestamp", tools.Pin)
	}
	if tools.Upstream == nil {
		t.Fatal("tools entry carries no upstream verdict")
	}
	if tools.Upstream.Verdict != "behind" || tools.Upstream.ResolvedCommit != "dddd" || tools.Upstream.ResolvedRef != "1.9.0" {
		t.Fatalf("tools upstream = %#v", tools.Upstream)
	}
	unlocked := upstreamStatusEntry(t, doc, "pack:unlocked")
	if unlocked.Pin != nil {
		t.Fatalf("unlocked pin = %#v, want nil", unlocked.Pin)
	}
	if unlocked.Upstream == nil || unlocked.Upstream.Verdict != "not_applicable" ||
		!strings.Contains(unlocked.Upstream.Error, "gc import install") {
		t.Fatalf("unlocked upstream = %#v", unlocked.Upstream)
	}
	if local := upstreamStatusEntry(t, doc, "pack:local"); local.Upstream == nil || local.Upstream.Verdict != "not_applicable" {
		t.Fatalf("local upstream = %#v", local.Upstream)
	}
	if doc.Upstream == nil {
		t.Fatal("document carries no upstream summary")
	}
	want := ImportStatusUpstreamSummary{Passed: false, Checked: 3, Current: 0, Behind: 1, Unreachable: 0, NotApplicable: 2}
	if *doc.Upstream != want {
		t.Fatalf("summary = %#v, want %#v", *doc.Upstream, want)
	}

	assertTopLevelOKTrue(t, stdout.Bytes())
	validateJSONAgainstResultSchema(t, []string{"import", "status"}, stdout.Bytes())
}

// TestImportStatusCheckUpstreamExitCodeAndStderr is AC-07. A behind import
// exits 1, an all-current run exits 0, "passed" mirrors the exit code either
// way, and every stale import is named on stderr with both commits -- a CI log
// that captures only stderr still says which pin is stale and where it should
// move to.
func TestImportStatusCheckUpstreamExitCodeAndStderr(t *testing.T) {
	t.Run("behind exits 1 and names the import on stderr", func(t *testing.T) {
		clearGCEnv(t)
		dir := upstreamStatusFixtureCity(t)
		stubCheckUpstreamImports(t, packman.UpstreamStatus{
			Name: "pack:tools", Source: "https://example.com/tools.git", Constraint: "^1.4",
			LockedCommit: "aaaa", ResolvedRef: "refs/heads/main", ResolvedCommit: "dddd",
			Verdict: packman.UpstreamBehind,
		})

		var stdout, stderr bytes.Buffer
		code := run([]string{"--city", dir, "import", "status", "--json", "--check-upstream"}, &stdout, &stderr)
		if code != 1 {
			t.Fatalf("code = %d, want 1", code)
		}
		for _, want := range []string{`"pack:tools"`, "aaaa", "dddd", "behind"} {
			if !strings.Contains(stderr.String(), want) {
				t.Fatalf("stderr missing %q:\n%s", want, stderr.String())
			}
		}
		if doc := decodeImportStatusDoc(t, stdout.Bytes()); doc.Upstream.Passed {
			t.Fatalf("passed = true, want false to mirror exit 1")
		}
	})

	t.Run("all current exits 0", func(t *testing.T) {
		clearGCEnv(t)
		dir := upstreamStatusFixtureCity(t)
		stubCheckUpstreamImports(t, packman.UpstreamStatus{
			Name: "pack:tools", Source: "https://example.com/tools.git", Constraint: "^1.4",
			LockedCommit: "aaaa", ResolvedRef: "refs/heads/main", ResolvedCommit: "aaaa",
			Verdict: packman.UpstreamCurrent,
		})

		var stdout, stderr bytes.Buffer
		code := run([]string{"--city", dir, "import", "status", "--json", "--check-upstream"}, &stdout, &stderr)
		if code != 0 {
			t.Fatalf("code = %d, want 0; stderr=%s", code, stderr.String())
		}
		if strings.Contains(stderr.String(), "behind") {
			t.Fatalf("stderr names a stale import with none present:\n%s", stderr.String())
		}
		if doc := decodeImportStatusDoc(t, stdout.Bytes()); !doc.Upstream.Passed {
			t.Fatalf("passed = false, want true to mirror exit 0")
		}
	})
}

// TestImportStatusCheckUpstreamUnreachable is AC-08. A source that will not
// resolve is reported unreachable with its error surfaced -- never current,
// which would be a false all-clear, and never behind, which would name a
// commit nothing resolved. --fail-on-unreachable changes only the exit code
// and "passed", not a single verdict.
func TestImportStatusCheckUpstreamUnreachable(t *testing.T) {
	unreachable := packman.UpstreamStatus{
		Name: "pack:tools", Source: "https://example.com/tools.git", Constraint: "^1.4",
		LockedCommit: "aaaa", Verdict: packman.UpstreamUnreachable,
		Err: fmt.Errorf("resolving head for %q: dial tcp: no route to host", "https://example.com/tools.git"),
	}

	clearGCEnv(t)
	dir := upstreamStatusFixtureCity(t)
	stubCheckUpstreamImports(t, unreachable)

	var stdout, stderr bytes.Buffer
	code := run([]string{"--city", dir, "import", "status", "--json", "--check-upstream"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("code = %d, want 0 without --fail-on-unreachable; stderr=%s", code, stderr.String())
	}
	doc := decodeImportStatusDoc(t, stdout.Bytes())
	tools := upstreamStatusEntry(t, doc, "pack:tools")
	if tools.Upstream.Verdict != "unreachable" {
		t.Fatalf("verdict = %q, want unreachable", tools.Upstream.Verdict)
	}
	if !strings.Contains(tools.Upstream.Error, "no route to host") {
		t.Fatalf("error = %q, want the resolution failure surfaced", tools.Upstream.Error)
	}
	if tools.Upstream.ResolvedCommit != "" {
		t.Fatalf("resolved_commit = %q, want empty", tools.Upstream.ResolvedCommit)
	}
	if !doc.Upstream.Passed || doc.Upstream.Unreachable != 1 {
		t.Fatalf("summary = %#v, want passed with 1 unreachable", *doc.Upstream)
	}
	if !strings.Contains(stderr.String(), "unreachable") {
		t.Fatalf("stderr does not name the unresolved import:\n%s", stderr.String())
	}
	validateJSONAgainstResultSchema(t, []string{"import", "status"}, stdout.Bytes())

	// Same fixture, same verdicts, one more flag.
	stubCheckUpstreamImports(t, unreachable)
	var strictOut, strictErr bytes.Buffer
	strictCode := run([]string{"--city", dir, "import", "status", "--json", "--check-upstream", "--fail-on-unreachable"}, &strictOut, &strictErr)
	if strictCode != 1 {
		t.Fatalf("code = %d, want 1 with --fail-on-unreachable; stderr=%s", strictCode, strictErr.String())
	}
	strictDoc := decodeImportStatusDoc(t, strictOut.Bytes())
	if strictDoc.Upstream.Passed {
		t.Fatal("passed = true, want false to mirror exit 1")
	}
	if got := upstreamStatusEntry(t, strictDoc, "pack:tools").Upstream.Verdict; got != "unreachable" {
		t.Fatalf("verdict = %q, want unreachable: the flag changes the exit code, not the verdict", got)
	}
	strictDoc.Upstream.Passed = true
	if *strictDoc.Upstream != *doc.Upstream {
		t.Fatalf("summary changed beyond passed: %#v vs %#v", *strictDoc.Upstream, *doc.Upstream)
	}
}

// TestImportStatusFailOnUnreachableRequiresCheckUpstream: a flag that is
// silently ignored reads as a passing gate, which is the failure mode this
// command exists to remove.
func TestImportStatusFailOnUnreachableRequiresCheckUpstream(t *testing.T) {
	clearGCEnv(t)
	dir := upstreamStatusFixtureCity(t)

	var stdout, stderr bytes.Buffer
	code := run([]string{"--city", dir, "import", "status", "--fail-on-unreachable"}, &stdout, &stderr)
	if code == 0 {
		t.Fatalf("code = 0, want non-zero; stdout=%s", stdout.String())
	}
	if !strings.Contains(stderr.String(), "--fail-on-unreachable requires --check-upstream") {
		t.Fatalf("stderr = %q", stderr.String())
	}
}

// TestImportStatusCheckUpstreamTextOutput pins the human-readable block: a
// verdict per import, an aggregate line, and the per-repository note that
// keeps a subpath import's "behind" from being read as "this pack changed".
func TestImportStatusCheckUpstreamTextOutput(t *testing.T) {
	clearGCEnv(t)
	dir := t.TempDir()
	writeCityToml(t, dir, "[workspace]\nname = \"demo\"\n")
	writePackToml(t, dir, `[pack]
name = "demo"
schema = 1

[imports.bd]
source = "https://example.com/mono.git//examples/bd"
version = "sha:aaaa"
`)
	if err := packman.WriteLockfile(fsys.OSFS{}, dir, &packman.Lockfile{
		Schema: packman.LockfileSchema,
		Packs: map[string]packman.LockedPack{
			"https://example.com/mono.git//examples/bd": {Version: "sha:aaaa", Commit: "aaaa", Fetched: time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)},
		},
	}); err != nil {
		t.Fatalf("WriteLockfile: %v", err)
	}
	stubCheckUpstreamImports(t, packman.UpstreamStatus{
		Name: "pack:bd", Source: "https://example.com/mono.git//examples/bd", Constraint: "sha:aaaa",
		LockedCommit: "aaaa", ResolvedRef: "refs/heads/main", ResolvedCommit: "bbbbbbbbbbbb",
		Verdict: packman.UpstreamBehind,
	})

	var stdout, stderr bytes.Buffer
	code := run([]string{"--city", dir, "import", "status", "--check-upstream"}, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("code = %d, want 1; stderr=%s", code, stderr.String())
	}
	for _, want := range []string{
		"upstream freshness:",
		"pack:bd",
		"behind",
		"refs/heads/main",
		"1 checked: 0 current, 1 behind, 0 unreachable, 0 not applicable",
		"freshness is measured per repository",
	} {
		if !strings.Contains(stdout.String(), want) {
			t.Fatalf("stdout missing %q:\n%s", want, stdout.String())
		}
	}
}

// TestImportStatusJSONProductionRunWithCheckUpstream runs the real command
// with --check-upstream and validates the emitted document against the
// schema, so a struct field added without a schema property is a red test
// rather than a silent pass. The fixture keeps all three entry shapes -- a
// pinned remote, an unlocked remote, and a path import -- so the shape
// coverage guard stays meaningful on the freshness path too.
func TestImportStatusJSONProductionRunWithCheckUpstream(t *testing.T) {
	clearGCEnv(t)
	dir := upstreamStatusFixtureCity(t)
	stubCheckUpstreamImports(t,
		packman.UpstreamStatus{
			Name: "pack:local", Source: "./packs/local", Verdict: packman.UpstreamNotApplicable,
		},
		packman.UpstreamStatus{
			Name: "pack:tools", Source: "https://example.com/tools.git", Constraint: "^1.4",
			LockedCommit: "aaaa", ResolvedRef: "1.4.2", ResolvedCommit: "aaaa",
			Verdict: packman.UpstreamCurrent,
		},
		packman.UpstreamStatus{
			Name: "pack:unlocked", Source: "https://example.com/unlocked.git", Constraint: "^9.9",
			Verdict: packman.UpstreamNotApplicable,
			Err:     fmt.Errorf("no packs.lock entry; run \"gc import install\""),
		},
	)

	var stdout, stderr bytes.Buffer
	code := run([]string{"--city", dir, "import", "status", "--json", "--check-upstream"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("code = %d, stderr=%q stdout=%q", code, stderr.String(), stdout.String())
	}

	doc := decodeImportStatusDoc(t, stdout.Bytes())
	shapes := map[string]bool{}
	for _, imp := range doc.Imports {
		if imp.Upstream == nil {
			t.Fatalf("entry %q carries no upstream verdict under --check-upstream", imp.Name)
		}
		switch {
		case imp.Kind == "path":
			shapes["path"] = true
		case imp.Pin != nil:
			shapes["pinned remote"] = true
		default:
			shapes["unlocked remote"] = true
		}
	}
	for _, shape := range []string{"pinned remote", "unlocked remote", "path"} {
		if !shapes[shape] {
			t.Fatalf("fixture emitted no %s import entry, so the schema validation no longer covers that shape:\n%s", shape, stdout.String())
		}
	}

	assertTopLevelOKTrue(t, stdout.Bytes())
	validateJSONAgainstResultSchema(t, []string{"import", "status"}, stdout.Bytes())
}
