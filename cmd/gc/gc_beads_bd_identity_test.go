package main

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"testing"
)

// TestEnsureDoltIdentityErrorMessages exercises the ensure_dolt_identity
// helper from examples/bd/assets/scripts/gc-beads-bd.sh against stub `dolt`
// and `git` binaries on PATH. The bug being guarded against: when a user
// has set ONLY `dolt config --global --add user.name`, the previous
// implementation reported "git user.name not available" and told the user
// to set user.name (which they already had). The corrected helper reports
// the field that is actually missing — user.email.
func TestEnsureDoltIdentityErrorMessages(t *testing.T) {
	t.Parallel()

	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not available; skipping shell-function test")
	}

	root := repoRootForLint(t)
	scriptPath := filepath.Join(root, "examples", "bd", "assets", "scripts", "gc-beads-bd.sh")
	scriptBytes, err := os.ReadFile(scriptPath)
	if err != nil {
		t.Fatalf("read script: %v", err)
	}
	fnSrc := extractShellFunction(t, string(scriptBytes), "ensure_dolt_identity")

	type fakeStore struct {
		name  string
		email string
	}
	type wantOutcome struct {
		exitOK             bool
		mustContain        []string
		mustNotContain     []string
		expectDoltNameSet  string
		expectDoltEmailSet string
	}
	cases := []struct {
		name string
		dolt fakeStore
		git  fakeStore
		want wantOutcome
	}{
		{
			name: "dolt_has_both_returns_ok",
			dolt: fakeStore{name: "Roger", email: "roger@example.com"},
			want: wantOutcome{exitOK: true},
		},
		{
			name: "dolt_only_name_git_empty_reports_email_missing_not_name",
			dolt: fakeStore{name: "Roger"},
			want: wantOutcome{
				exitOK:         false,
				mustContain:    []string{"user.email"},
				mustNotContain: []string{`add user.name "Your Name"`},
			},
		},
		{
			name: "dolt_only_email_git_empty_reports_name_missing_not_email",
			dolt: fakeStore{email: "roger@example.com"},
			want: wantOutcome{
				exitOK:         false,
				mustContain:    []string{"user.name"},
				mustNotContain: []string{`add user.email "you@example.com"`},
			},
		},
		{
			name: "dolt_empty_git_empty_reports_both_missing",
			want: wantOutcome{
				exitOK:      false,
				mustContain: []string{"user.name", "user.email"},
			},
		},
		{
			name: "dolt_empty_git_has_both_backfills_dolt",
			git:  fakeStore{name: "Roger", email: "roger@example.com"},
			want: wantOutcome{
				exitOK:             true,
				expectDoltNameSet:  "Roger",
				expectDoltEmailSet: "roger@example.com",
			},
		},
		{
			name: "dolt_name_git_email_backfills_only_email",
			dolt: fakeStore{name: "Roger"},
			git:  fakeStore{email: "roger@example.com"},
			want: wantOutcome{
				exitOK:             true,
				expectDoltEmailSet: "roger@example.com",
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			binDir := t.TempDir()
			writeFakeDolt(t, binDir, tc.dolt.name, tc.dolt.email)
			writeFakeGit(t, binDir, tc.git.name, tc.git.email)

			doltLog := filepath.Join(binDir, "dolt-set.log")
			origPath := os.Getenv("PATH")

			script := fnSrc + "\n" +
				"die() { printf '%s\\n' \"$*\" >&2; exit 1; }\n" +
				"ensure_dolt_identity\n"

			_, stderr, runErr := runBashSnippet(t, script,
				"PATH="+binDir+string(os.PathListSeparator)+origPath,
				"FAKE_DOLT_LOG="+doltLog,
			)

			if tc.want.exitOK {
				if runErr != nil {
					t.Fatalf("expected success, got %v\nstderr:\n%s", runErr, stderr)
				}
			} else {
				if runErr == nil {
					t.Fatalf("expected non-zero exit, got success\nstderr:\n%s", stderr)
				}
			}
			out := stderr
			for _, frag := range tc.want.mustContain {
				if !strings.Contains(out, frag) {
					t.Errorf("stderr missing %q:\n%s", frag, out)
				}
			}
			for _, frag := range tc.want.mustNotContain {
				if strings.Contains(out, frag) {
					t.Errorf("stderr should not contain %q (it is misleading guidance):\n%s", frag, out)
				}
			}
			if tc.want.expectDoltNameSet != "" {
				if !logContains(doltLog, "set user.name "+tc.want.expectDoltNameSet) {
					t.Errorf("expected dolt user.name to be set to %q; log:\n%s",
						tc.want.expectDoltNameSet, readFile(doltLog))
				}
			}
			if tc.want.expectDoltEmailSet != "" {
				if !logContains(doltLog, "set user.email "+tc.want.expectDoltEmailSet) {
					t.Errorf("expected dolt user.email to be set to %q; log:\n%s",
						tc.want.expectDoltEmailSet, readFile(doltLog))
				}
			}
		})
	}
}

// TestSeedFreshBDCurrentEraWitness protects the only safe bypass of beads'
// legacy-upgrade guard: Gas City may seed a marker while creating a provably
// absent managed Dolt root, never beside pre-existing storage or over an
// unrecognized marker.
func TestSeedFreshBDCurrentEraWitness(t *testing.T) {
	t.Parallel()

	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not available; skipping shell-function test")
	}

	root := repoRootForLint(t)
	scriptPath := filepath.Join(root, "examples", "bd", "assets", "scripts", "gc-beads-bd.sh")
	scriptBytes, err := os.ReadFile(scriptPath)
	if err != nil {
		t.Fatalf("read script: %v", err)
	}
	fnSrc := extractShellFunction(t, string(scriptBytes), "bd_witness_is_current") + "\n" +
		extractShellFunction(t, string(scriptBytes), "read_bd_current_era_witness") + "\n" +
		extractShellFunction(t, string(scriptBytes), "bd_storage_layout_is_fresh_for_witness") + "\n" +
		extractShellFunction(t, string(scriptBytes), "require_gascity_fresh_witness_admission") + "\n" +
		extractShellFunction(t, string(scriptBytes), "seed_fresh_bd_current_era_witness")

	cases := []struct {
		name            string
		existingWitness string
		createDoltRoot  bool
		useCustomRoot   bool
		bdVersionOutput string
		bdVersionExit   int
		wantWitness     string
		wantOK          bool
	}{
		{
			name:            "absent_root_and_marker_uses_running_bd_version",
			bdVersionOutput: "bd version 1.2.2 (abc4a9fa)",
			wantWitness:     "1.2.2\n",
			wantOK:          true,
		},
		{
			name:            "valid_witness_without_root_is_refused_and_preserved",
			existingWitness: "v1.1.0\n",
			bdVersionOutput: "bd version 9.9.9",
			wantWitness:     "v1.1.0\n",
		},
		{
			name:            "legacy_witness_is_refused_and_preserved",
			existingWitness: "0.99.0\n",
			bdVersionOutput: "bd version 1.2.2",
			wantWitness:     "0.99.0\n",
		},
		{
			name:            "malformed_witness_is_refused_and_preserved",
			existingWitness: "not-a-version\n",
			bdVersionOutput: "bd version 1.2.2",
			wantWitness:     "not-a-version\n",
		},
		{
			name:            "oversized_marker_rejected_even_when_trimmed_value_looks_valid",
			existingWitness: "1.2.2" + strings.Repeat("\n", 70),
			bdVersionOutput: "bd version 1.2.2",
			wantWitness:     "1.2.2" + strings.Repeat("\n", 70),
		},
		{
			name:            "preexisting_dolt_root_without_witness_is_refused",
			createDoltRoot:  true,
			bdVersionOutput: "bd version 1.2.2",
		},
		{
			name:            "preexisting_dolt_root_with_valid_witness_is_still_not_fresh",
			existingWitness: "1.1.0\n",
			createDoltRoot:  true,
			bdVersionOutput: "bd version 1.2.2",
			wantWitness:     "1.1.0\n",
		},
		{
			name:            "unavailable_version_is_refused",
			bdVersionOutput: "not a version",
		},
		{
			name:            "unrelated_semver_is_refused",
			bdVersionOutput: "tool version 9.9.9",
		},
		{
			name:            "legacy_selected_bd_is_refused",
			bdVersionOutput: "bd version 0.99.0",
		},
		{
			name:            "failing_version_command_is_refused",
			bdVersionOutput: "bd version 1.2.2",
			bdVersionExit:   7,
		},
		{
			name:            "custom_root_cannot_relabel_preexisting_default_root",
			createDoltRoot:  true,
			useCustomRoot:   true,
			bdVersionOutput: "bd version 1.2.2",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			beadsDir := filepath.Join(dir, ".beads")
			witness := filepath.Join(beadsDir, ".local_version")
			dataRoot := filepath.Join(beadsDir, "dolt")
			if tc.useCustomRoot {
				dataRoot = filepath.Join(dir, "custom-dolt-data")
			}
			if err := os.MkdirAll(beadsDir, 0o755); err != nil {
				t.Fatalf("create .beads: %v", err)
			}
			if tc.createDoltRoot {
				if err := os.MkdirAll(filepath.Join(beadsDir, "dolt"), 0o755); err != nil {
					t.Fatalf("create Dolt root: %v", err)
				}
			}
			if tc.existingWitness != "" {
				if err := os.WriteFile(witness, []byte(tc.existingWitness), 0o644); err != nil {
					t.Fatalf("write witness: %v", err)
				}
			}
			if tc.wantOK {
				if err := os.WriteFile(filepath.Join(beadsDir, freshManagedDoltAdmissionName), []byte("test admission\n"), 0o600); err != nil {
					t.Fatalf("write test admission: %v", err)
				}
			}

			binDir := t.TempDir()
			bdBin := filepath.Join(binDir, "bd")
			writeExecutable(t, bdBin, "#!/bin/sh\nprintf '%s\\n' \"$GC_TEST_BD_VERSION\"\nexit \"$GC_TEST_BD_VERSION_EXIT\"\n")
			gcBin := filepath.Join(binDir, "gc")
			writeExecutable(t, gcBin, "#!/bin/sh\nexit 0\n")
			script := fnSrc + "\n" +
				"die() { printf '%s\\n' \"$*\" >&2; exit 1; }\n" +
				"seed_fresh_bd_current_era_witness \"$GC_TEST_DIR\" \"$GC_TEST_DATA_ROOT\"\n"
			_, stderr, runErr := runBashSnippet(t, script,
				"BD_BIN="+bdBin,
				"GC_BIN="+gcBin,
				"GC_TEST_BD_VERSION="+tc.bdVersionOutput,
				fmt.Sprintf("GC_TEST_BD_VERSION_EXIT=%d", tc.bdVersionExit),
				"GC_TEST_DIR="+dir,
				"GC_TEST_DATA_ROOT="+dataRoot,
			)
			if tc.wantOK && runErr != nil {
				t.Fatalf("seed witness: %v\nstderr:\n%s", runErr, stderr)
			}
			if !tc.wantOK && runErr == nil {
				t.Fatalf("seed witness succeeded, want fail-closed refusal")
			}
			got, readErr := os.ReadFile(witness)
			if tc.wantWitness == "" {
				if !os.IsNotExist(readErr) {
					t.Fatalf("witness read error = %v, want not-exist", readErr)
				}
			} else {
				if readErr != nil {
					t.Fatalf("read witness: %v", readErr)
				}
				if string(got) != tc.wantWitness {
					t.Fatalf("witness = %q, want %q", got, tc.wantWitness)
				}
			}
		})
	}
}

func TestSeedFreshBDCurrentEraWitnessRefusesPreExistingStorageLayoutsWithoutMutation(t *testing.T) {
	t.Parallel()

	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not available; skipping shell-function test")
	}

	root := repoRootForLint(t)
	scriptPath := filepath.Join(root, "examples", "bd", "assets", "scripts", "gc-beads-bd.sh")
	scriptBytes, err := os.ReadFile(scriptPath)
	if err != nil {
		t.Fatalf("read script: %v", err)
	}
	fnSrc := extractShellFunction(t, string(scriptBytes), "bd_witness_is_current") + "\n" +
		extractShellFunction(t, string(scriptBytes), "read_bd_current_era_witness") + "\n" +
		extractShellFunction(t, string(scriptBytes), "bd_storage_layout_is_fresh_for_witness") + "\n" +
		extractShellFunction(t, string(scriptBytes), "require_gascity_fresh_witness_admission") + "\n" +
		extractShellFunction(t, string(scriptBytes), "seed_fresh_bd_current_era_witness")

	for _, tc := range []struct {
		name  string
		setup func(t *testing.T, beadsDir string)
	}{
		{
			name: "metadata-less SQLite database and sidecars",
			setup: func(t *testing.T, beadsDir string) {
				for name, body := range map[string][]byte{
					"vc.db":        append([]byte("SQLite format 3\x00"), []byte("legacy rows")...),
					"vc.db-wal":    []byte("legacy wal"),
					"vc.db-shm":    []byte("legacy shm"),
					"issues.jsonl": []byte("{\"id\":\"legacy-1\"}\n"),
				} {
					if err := os.WriteFile(filepath.Join(beadsDir, name), body, 0o640); err != nil {
						t.Fatalf("write %s: %v", name, err)
					}
				}
			},
		},
		{
			name: "metadata-backed SQLite database",
			setup: func(t *testing.T, beadsDir string) {
				if err := os.WriteFile(filepath.Join(beadsDir, "metadata.json"), []byte(`{"backend":"sqlite","database":"beads.db"}`), 0o640); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(beadsDir, "beads.db"), append([]byte("SQLite format 3\x00"), []byte("legacy rows")...), 0o640); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "standalone issues export",
			setup: func(t *testing.T, beadsDir string) {
				if err := os.WriteFile(filepath.Join(beadsDir, "issues.jsonl"), []byte("{\"id\":\"legacy-1\"}\n"), 0o640); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "legacy embedded Dolt root",
			setup: func(t *testing.T, beadsDir string) {
				path := filepath.Join(beadsDir, "embeddeddolt", "legacy", ".dolt")
				if err := os.MkdirAll(path, 0o750); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(path, "repo_state"), []byte("legacy"), 0o640); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "unrecognized retained storage artifact",
			setup: func(t *testing.T, beadsDir string) {
				if err := os.MkdirAll(filepath.Join(beadsDir, "archive"), 0o750); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(beadsDir, "archive", "issues.db"), []byte("retained"), 0o640); err != nil {
					t.Fatal(err)
				}
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			city := t.TempDir()
			beadsDir := filepath.Join(city, ".beads")
			if err := os.Mkdir(beadsDir, 0o750); err != nil {
				t.Fatal(err)
			}
			tc.setup(t, beadsDir)
			before := snapshotBeadsTreeExact(t, beadsDir)

			binDir := t.TempDir()
			bdBin := filepath.Join(binDir, "bd")
			writeExecutable(t, bdBin, "#!/bin/sh\nprintf 'bd version 1.2.2\\n'\n")
			gcBin := filepath.Join(binDir, "gc")
			writeExecutable(t, gcBin, "#!/bin/sh\nexit 0\n")
			harness := fnSrc + "\n" +
				"die() { printf '%s\\n' \"$*\" >&2; exit 1; }\n" +
				"seed_fresh_bd_current_era_witness \"$GC_TEST_DIR\" \"$GC_TEST_DIR/.beads/dolt\"\n"
			_, _, runErr := runBashSnippet(t, harness, "BD_BIN="+bdBin, "GC_BIN="+gcBin, "GC_TEST_DIR="+city)
			if runErr == nil {
				t.Fatal("seed witness accepted pre-existing storage layout")
			}
			after := snapshotBeadsTreeExact(t, beadsDir)
			if !reflect.DeepEqual(after, before) {
				t.Fatalf("refused layout mutated .beads:\nbefore: %#v\nafter:  %#v", before, after)
			}
			if _, err := os.Lstat(filepath.Join(beadsDir, ".local_version")); !os.IsNotExist(err) {
				t.Fatalf("witness created for refused layout: %v", err)
			}
			if _, err := os.Lstat(filepath.Join(beadsDir, "dolt")); !os.IsNotExist(err) {
				t.Fatalf("Dolt root created for refused layout: %v", err)
			}
		})
	}
}

func TestGCBeadsBDWrapperRefusesUnprovenMetadataWithoutMutation(t *testing.T) {
	t.Parallel()

	root := repoRootForLint(t)
	scriptPath := filepath.Join(root, "examples", "bd", "assets", "scripts", "gc-beads-bd.sh")
	for _, tc := range []struct {
		name     string
		metadata string
	}{
		{name: "corrupt metadata", metadata: "{not-json\n"},
		{name: "unknown backend", metadata: `{"database":"mystery","backend":"mystery"}` + "\n"},
		{
			name: "established remote server metadata",
			metadata: `{"database":"dolt","backend":"dolt","dolt_mode":"server","dolt_database":"hq",` +
				`"project_id":"existing","dolt_server_host":"remote"}` + "\n",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			city := t.TempDir()
			beadsDir := filepath.Join(city, ".beads")
			if err := os.Mkdir(beadsDir, 0o750); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(beadsDir, "config.yaml"), []byte("issue-prefix: legacy\n"), 0o640); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(beadsDir, "metadata.json"), []byte(tc.metadata), 0o640); err != nil {
				t.Fatal(err)
			}
			before := snapshotBeadsTreeExact(t, beadsDir)

			binDir := t.TempDir()
			bdBin := filepath.Join(binDir, "bd")
			writeExecutable(t, bdBin, "#!/bin/sh\nprintf 'bd version 1.2.2\\n'\n")
			env := sanitizedBaseEnv(
				"BD_BIN="+bdBin,
				"GC_BIN=",
				"GC_BEADS_BACKEND=dolt",
				"GC_CITY_PATH="+city,
				"GC_CITY_RUNTIME_DIR="+filepath.Join(city, ".gc", "runtime"),
				"GC_DOLT=managed",
				"GC_DOLT_DATA_DIR="+filepath.Join(beadsDir, "dolt"),
				"GC_PACK_STATE_DIR="+filepath.Join(city, ".gc", "runtime", "packs", "dolt"),
			)
			stdout, stderr, runErr := runIdentityTestCommand("bash", []string{scriptPath, "probe"}, env)
			out := []byte(stdout + stderr)
			if runErr == nil {
				t.Fatal("wrapper accepted rootless metadata without Gas City provenance")
			}
			if !bytes.Contains(out, []byte("fresh-init admission")) {
				t.Fatalf("wrapper did not report missing provenance; output:\n%s", out)
			}
			after := snapshotBeadsTreeExact(t, beadsDir)
			if !reflect.DeepEqual(after, before) {
				t.Fatalf("wrapper mutated unproven .beads:\nbefore: %#v\nafter:  %#v\noutput:\n%s", before, after, out)
			}
			if _, err := os.Lstat(filepath.Join(beadsDir, ".local_version")); !os.IsNotExist(err) {
				t.Fatalf("witness created for unproven metadata: %v", err)
			}
			if _, err := os.Lstat(filepath.Join(beadsDir, "dolt")); !os.IsNotExist(err) {
				t.Fatalf("Dolt root created for unproven metadata: %v", err)
			}
		})
	}
}

func TestGCBeadsBDWrapperRefusesLegacySQLiteWithoutMutation(t *testing.T) {
	t.Parallel()

	root := repoRootForLint(t)
	scriptPath := filepath.Join(root, "examples", "bd", "assets", "scripts", "gc-beads-bd.sh")
	city := t.TempDir()
	beadsDir := filepath.Join(city, ".beads")
	if err := os.Mkdir(beadsDir, 0o750); err != nil {
		t.Fatal(err)
	}
	legacy := append([]byte("SQLite format 3\x00"), []byte("legacy rows must survive byte-for-byte")...)
	if err := os.WriteFile(filepath.Join(beadsDir, "vc.db"), legacy, 0o640); err != nil {
		t.Fatal(err)
	}
	before := snapshotBeadsTreeExact(t, beadsDir)

	binDir := t.TempDir()
	bdBin := filepath.Join(binDir, "bd")
	writeExecutable(t, bdBin, "#!/bin/sh\nprintf 'bd version 1.2.2\\n'\n")
	env := sanitizedBaseEnv(
		"BD_BIN="+bdBin,
		"GC_BIN=",
		"GC_BEADS_BACKEND=dolt",
		"GC_CITY_PATH="+city,
		"GC_CITY_RUNTIME_DIR="+filepath.Join(city, ".gc", "runtime"),
		"GC_DOLT=managed",
		"GC_DOLT_DATA_DIR="+filepath.Join(beadsDir, "dolt"),
		"GC_PACK_STATE_DIR="+filepath.Join(city, ".gc", "runtime", "packs", "dolt"),
	)
	stdout, stderr, runErr := runIdentityTestCommand("bash", []string{scriptPath, "probe"}, env)
	out := []byte(stdout + stderr)
	if runErr == nil {
		t.Fatal("wrapper accepted metadata-less legacy SQLite workspace")
	}
	if !bytes.Contains(out, []byte("refusing to seed bd witness")) {
		t.Fatalf("wrapper did not report the freshness refusal; output:\n%s", out)
	}
	after := snapshotBeadsTreeExact(t, beadsDir)
	if !reflect.DeepEqual(after, before) {
		t.Fatalf("wrapper mutated legacy .beads:\nbefore: %#v\nafter:  %#v\noutput:\n%s", before, after, out)
	}
}

func TestPrepareBDWitnessDirectoryNeverMutatesUnprovenExistingPath(t *testing.T) {
	t.Parallel()

	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not available; skipping shell-function test")
	}

	root := repoRootForLint(t)
	scriptBytes, err := os.ReadFile(filepath.Join(root, "examples", "bd", "assets", "scripts", "gc-beads-bd.sh"))
	if err != nil {
		t.Fatalf("read script: %v", err)
	}
	fnSrc := extractShellFunction(t, string(scriptBytes), "prepare_bd_witness_directory")

	for _, tc := range []struct {
		name     string
		setup    func(t *testing.T, beadsPath string) (target string, mode os.FileMode)
		wantOK   bool
		wantMode os.FileMode
	}{
		{
			name:   "absent_path_is_created_private",
			setup:  func(*testing.T, string) (string, os.FileMode) { return "", 0 },
			wantOK: true, wantMode: 0o700,
		},
		{
			name: "existing_directory_is_not_chmodded_before_validation",
			setup: func(t *testing.T, path string) (string, os.FileMode) {
				if err := os.Mkdir(path, 0o755); err != nil {
					t.Fatalf("create existing .beads: %v", err)
				}
				return path, 0o755
			},
			wantOK: true, wantMode: 0o755,
		},
		{
			name: "symlink_is_refused_without_chmodding_target",
			setup: func(t *testing.T, path string) (string, os.FileMode) {
				target := t.TempDir()
				if err := os.Chmod(target, 0o751); err != nil {
					t.Fatalf("chmod symlink target: %v", err)
				}
				if err := os.Symlink(target, path); err != nil {
					t.Fatalf("create .beads symlink: %v", err)
				}
				return target, 0o751
			},
		},
		{
			name: "regular_file_is_refused_without_mutation",
			setup: func(t *testing.T, path string) (string, os.FileMode) {
				if err := os.WriteFile(path, []byte("legacy"), 0o640); err != nil {
					t.Fatalf("create .beads file: %v", err)
				}
				return path, 0o640
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			city := t.TempDir()
			beadsPath := filepath.Join(city, ".beads")
			target, originalMode := tc.setup(t, beadsPath)
			if target == "" {
				target = beadsPath
				originalMode = tc.wantMode
			}
			harness := fnSrc + "\n" +
				"die() { printf '%s\\n' \"$*\" >&2; exit 1; }\n" +
				"prepare_bd_witness_directory \"$GC_TEST_DIR\"\n"
			_, stderr, runErr := runBashSnippet(t, harness, "GC_TEST_DIR="+city)
			if tc.wantOK && runErr != nil {
				t.Fatalf("prepare directory: %v\nstderr:\n%s", runErr, stderr)
			}
			if !tc.wantOK && runErr == nil {
				t.Fatal("prepare directory accepted an unproven path")
			}
			info, statErr := os.Stat(target)
			if statErr != nil {
				t.Fatalf("stat target: %v", statErr)
			}
			wantMode := originalMode
			if tc.wantMode != 0 {
				wantMode = tc.wantMode
			}
			if got := info.Mode().Perm(); got != wantMode {
				t.Fatalf("target mode = %#o, want unchanged %#o", got, wantMode)
			}
		})
	}
}

func TestRunBDInitPinnedRequiresWitnessForLocalDoltRoot(t *testing.T) {
	t.Parallel()

	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not available; skipping shell-function test")
	}

	root := repoRootForLint(t)
	scriptBytes, err := os.ReadFile(filepath.Join(root, "examples", "bd", "assets", "scripts", "gc-beads-bd.sh"))
	if err != nil {
		t.Fatalf("read script: %v", err)
	}
	src := string(scriptBytes)
	fnSrc := extractShellFunction(t, src, "bd_witness_is_current") + "\n" +
		extractShellFunction(t, src, "read_bd_current_era_witness") + "\n" +
		extractShellFunction(t, src, "require_bd_current_era_witness_for_local_dolt_root") + "\n" +
		extractShellFunction(t, src, "run_bd_init_pinned")

	for _, tc := range []struct {
		name           string
		createDoltRoot bool
		witness        string
		wantOK         bool
	}{
		{name: "local_root_with_current_witness", createDoltRoot: true, witness: "1.1.0\n", wantOK: true},
		{name: "local_root_without_witness_fails_closed", createDoltRoot: true},
		{name: "local_root_with_legacy_witness_fails_closed", createDoltRoot: true, witness: "0.62.0\n"},
		{name: "local_root_with_nul_witness_fails_closed", createDoltRoot: true, witness: "1.2.3\x00"},
		{name: "scope_without_local_root_needs_no_witness", wantOK: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			beadsDir := filepath.Join(dir, ".beads")
			if err := os.MkdirAll(beadsDir, 0o755); err != nil {
				t.Fatalf("create .beads: %v", err)
			}
			if tc.createDoltRoot {
				if err := os.MkdirAll(filepath.Join(beadsDir, "dolt"), 0o755); err != nil {
					t.Fatalf("create Dolt root: %v", err)
				}
			}
			if tc.witness != "" {
				if err := os.WriteFile(filepath.Join(beadsDir, ".local_version"), []byte(tc.witness), 0o644); err != nil {
					t.Fatalf("write witness: %v", err)
				}
			}
			callLog := filepath.Join(dir, "bd-call")
			harness := fnSrc + `
die() { printf '%s\n' "$*" >&2; exit 1; }
run_bd_pinned() {
    printf '%s\n' "$*" > "$GC_TEST_CALL_LOG"
}
run_bd_init_pinned "$GC_TEST_DIR" gc hq 127.0.0.1 false
`
			_, stderr, runErr := runBashSnippet(t, harness,
				"GC_TEST_CALL_LOG="+callLog,
				"GC_TEST_DIR="+dir,
			)
			if tc.wantOK && runErr != nil {
				t.Fatalf("run_bd_init_pinned: %v\nstderr:\n%s", runErr, stderr)
			}
			if !tc.wantOK && runErr == nil {
				t.Fatal("run_bd_init_pinned succeeded without a safe witness")
			}
			call, readErr := os.ReadFile(callLog)
			if tc.wantOK {
				if readErr != nil {
					t.Fatalf("read bd call log: %v", readErr)
				}
				if !strings.Contains(string(call), "init --quiet --server") {
					t.Fatalf("bd call = %q, want init --quiet --server", call)
				}
			} else if !os.IsNotExist(readErr) {
				t.Fatalf("refused init reached bd call: %q (err=%v)", call, readErr)
			}
		})
	}
}

func TestGCBeadsBDSeedsWitnessBeforeCreatingFreshDataRoot(t *testing.T) {
	t.Parallel()
	root := repoRootForLint(t)
	script, err := os.ReadFile(filepath.Join(root, "examples", "bd", "assets", "scripts", "gc-beads-bd.sh"))
	if err != nil {
		t.Fatalf("read script: %v", err)
	}
	_, mainBody, ok := strings.Cut(string(script), "\n# --- Main ---\n")
	if !ok {
		t.Fatal("script missing main boundary")
	}
	prepareAt := strings.Index(mainBody, `prepare_bd_witness_directory "$GC_CITY_PATH"`)
	seedAt := strings.Index(mainBody, `seed_fresh_bd_current_era_witness "$GC_CITY_PATH" "$DATA_DIR"`)
	permissionsAt := strings.Index(mainBody, `ensure_beads_dir_permissions "$GC_CITY_PATH"`)
	mkdirAt := strings.Index(mainBody, `mkdir -p "$DATA_DIR" "$PACK_STATE_DIR"`)
	if prepareAt < 0 || seedAt < 0 || permissionsAt < 0 || mkdirAt < 0 ||
		prepareAt >= seedAt || seedAt >= permissionsAt || permissionsAt >= mkdirAt {
		t.Fatalf("managed path order prepare=%d seed=%d permissions=%d mkdir=%d, want prepare < seed < permissions < mkdir", prepareAt, seedAt, permissionsAt, mkdirAt)
	}
}

func TestSeedFreshBDWitnessPreservesWinnerDuringConcurrentResume(t *testing.T) {
	t.Parallel()
	root := repoRootForLint(t)
	scriptBytes, err := os.ReadFile(filepath.Join(root, "examples", "bd", "assets", "scripts", "gc-beads-bd.sh"))
	if err != nil {
		t.Fatal(err)
	}
	seedFn := extractShellFunction(t, string(scriptBytes), "seed_fresh_bd_current_era_witness")
	city := t.TempDir()
	beadsDir := filepath.Join(city, ".beads")
	if err := os.Mkdir(beadsDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(beadsDir, freshManagedDoltAdmissionName), []byte("sealed\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	bdBin := filepath.Join(t.TempDir(), "bd")
	writeExecutable(t, bdBin, "#!/bin/sh\nprintf 'bd version 1.2.2\\n'\n")
	harness := `
die() { printf '%s\n' "$*" >&2; exit 1; }
require_gascity_fresh_witness_admission() { return 0; }
read_bd_current_era_witness() { return 1; }
cleanup_gascity_fresh_witness_temps() { return 0; }
bd_storage_layout_is_fresh_for_witness() { return 0; }
bd_witness_is_current() { return 0; }
mkdir() {
    command mkdir "$1" || return $?
    rm -f "$GC_TEST_DIR/.beads/.gascity-fresh-dolt-admission"
    return 1
}
` + seedFn + `
seed_fresh_bd_current_era_witness "$GC_TEST_DIR" "$GC_TEST_DIR/.beads/dolt"
`
	_, _, runErr := runBashSnippet(t, harness, "GC_TEST_DIR="+city, "BD_BIN="+bdBin)
	if runErr == nil {
		t.Fatal("simulated losing creator unexpectedly succeeded")
	}
	if _, err := os.Stat(filepath.Join(beadsDir, "dolt")); err != nil {
		t.Fatalf("concurrent resumer root is missing: %v", err)
	}
	if got, err := os.ReadFile(filepath.Join(beadsDir, ".local_version")); err != nil {
		t.Fatalf("losing creator removed the winning witness: %v", err)
	} else if string(got) != "1.2.2\n" {
		t.Fatalf("winning witness changed: %q", got)
	}
}

func TestSeedFreshBDWitnessRevalidatesCreatedRootBeforeAdmissionConsumption(t *testing.T) {
	t.Parallel()
	root := repoRootForLint(t)
	scriptBytes, err := os.ReadFile(filepath.Join(root, "examples", "bd", "assets", "scripts", "gc-beads-bd.sh"))
	if err != nil {
		t.Fatal(err)
	}
	seedFn := extractShellFunction(t, string(scriptBytes), "seed_fresh_bd_current_era_witness")
	city := t.TempDir()
	beadsDir := filepath.Join(city, ".beads")
	if err := os.Mkdir(beadsDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(beadsDir, freshManagedDoltAdmissionName), []byte("sealed\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	bdBin := filepath.Join(t.TempDir(), "bd")
	writeExecutable(t, bdBin, "#!/bin/sh\nprintf 'bd version 1.2.2\\n'\n")
	validationLog := filepath.Join(t.TempDir(), "validations")
	harness := `
die() { printf '%s\n' "$*" >&2; exit 1; }
require_gascity_fresh_witness_admission() { printf '<%s>\n' "${3:-}" >> "$GC_TEST_VALIDATION_LOG"; }
read_bd_current_era_witness() { return 1; }
cleanup_gascity_fresh_witness_temps() { return 0; }
bd_storage_layout_is_fresh_for_witness() { return 0; }
bd_witness_is_current() { return 0; }
` + seedFn + `
seed_fresh_bd_current_era_witness "$GC_TEST_DIR" "$GC_TEST_DIR/.beads/dolt"
`
	_, stderr, runErr := runBashSnippet(t, harness,
		"GC_TEST_DIR="+city,
		"GC_TEST_VALIDATION_LOG="+validationLog,
		"BD_BIN="+bdBin,
	)
	if runErr != nil {
		t.Fatalf("fresh witness seed failed: %v\n%s", runErr, stderr)
	}
	got, err := os.ReadFile(validationLog)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "<>\n<true>\n" {
		t.Fatalf("admission validations = %q, want pre-link plus final allow-created validation", got)
	}
	if _, err := os.Lstat(filepath.Join(beadsDir, freshManagedDoltAdmissionName)); !os.IsNotExist(err) {
		t.Fatalf("validated admission was not consumed: %v", err)
	}
}

func TestGCBeadsBDRemoteAndHostedMainBypassLocalFreshInit(t *testing.T) {
	t.Parallel()
	root := repoRootForLint(t)
	script := filepath.Join(root, "examples", "bd", "assets", "scripts", "gc-beads-bd.sh")
	for _, tc := range []struct {
		name  string
		extra []string
	}{
		{name: "remote host", extra: []string{"GC_DOLT_HOST=remote.example"}},
		{name: "secondary loopback treated as remote", extra: []string{"GC_DOLT_HOST=127.0.0.2"}},
		{name: "uppercase localhost treated as remote", extra: []string{"GC_DOLT_HOST=LOCALHOST"}},
		{name: "hosted credential command", extra: []string{"GC_DOLT_HOST=127.0.0.1", "BEADS_DOLT_CREDENTIAL_COMMAND=/bin/false"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			city := t.TempDir()
			beadsDir := filepath.Join(city, ".beads")
			if err := os.Mkdir(beadsDir, 0o750); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(beadsDir, "remote-binding.sentinel"), []byte("must survive\n"), 0o640); err != nil {
				t.Fatal(err)
			}
			before := snapshotBeadsTreeExact(t, beadsDir)
			packState := filepath.Join(city, ".gc", "runtime", "packs", "dolt")
			env := sanitizedBaseEnv(
				"GC_BIN=",
				"GC_BEADS_BACKEND=dolt",
				"GC_CITY_PATH="+city,
				"GC_DOLT=managed",
				"GC_DOLT_PORT=19999",
				"GC_PACK_STATE_DIR="+packState,
			)
			env = append(env, tc.extra...)
			stdout, stderr, err := runIdentityTestCommand("sh", []string{script, "future-provider-operation"}, env)
			if err == nil {
				t.Fatalf("unknown provider operation unexpectedly succeeded; output:\n%s%s", stdout, stderr)
			}
			after := snapshotBeadsTreeExact(t, beadsDir)
			if !reflect.DeepEqual(after, before) {
				t.Fatalf("remote/hosted provider mutated local .beads:\nbefore: %#v\nafter:  %#v", before, after)
			}
			if _, err := os.Stat(packState); err != nil {
				t.Fatalf("provider runtime directory was not created: %v", err)
			}
		})
	}
}

func TestGCBeadsBDRemainsPOSIXShellParseable(t *testing.T) {
	t.Parallel()
	shell, err := exec.LookPath("dash")
	if err != nil {
		shell, err = exec.LookPath("sh")
		if err != nil {
			t.Skip("no POSIX shell installed")
		}
	}
	root := repoRootForLint(t)
	script := filepath.Join(root, "examples", "bd", "assets", "scripts", "gc-beads-bd.sh")
	stdout, stderr, err := runIdentityTestCommand(shell, []string{"-n", script}, nil)
	if err != nil {
		t.Fatalf("gc-beads-bd.sh is not parseable by its /bin/sh interpreter: %v\n%s%s", err, stdout, stderr)
	}
}

// TestRunBDPinnedUsesSelectedBinary locks the provider subprocess to BD_BIN.
// The schema-66 cutover is unsafe if the shell silently rediscovers the old bd
// on PATH after Gas City selected and staged the matching source-built binary.
func TestRunBDPinnedUsesSelectedBinary(t *testing.T) {
	t.Parallel()

	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not available; skipping shell-function test")
	}

	root := repoRootForLint(t)
	scriptBytes, err := os.ReadFile(filepath.Join(root, "examples", "bd", "assets", "scripts", "gc-beads-bd.sh"))
	if err != nil {
		t.Fatalf("read script: %v", err)
	}
	fnSrc := extractShellFunction(t, string(scriptBytes), "run_bd_pinned")

	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, ".beads"), 0o755); err != nil {
		t.Fatalf("create .beads: %v", err)
	}
	ambientDir := t.TempDir()
	writeExecutable(t, filepath.Join(ambientDir, "bd"), "#!/bin/sh\nprintf 'ambient bd executed\\n' >&2\nexit 77\n")
	pinnedDir := t.TempDir()
	pinnedBD := filepath.Join(pinnedDir, "bd-current")
	writeExecutable(t, pinnedBD, "#!/bin/sh\nprintf 'pinned:%s\\n' \"$*\"\n")
	harness := "connect_host() { printf '127.0.0.1'; }\n" + fnSrc + "\n" +
		"run_bd_pinned \"$GC_TEST_DIR\" version\n"
	stdout, stderr, runErr := runBashSnippet(t, harness,
		"BD_BIN="+pinnedBD,
		"DOLT_PASSWORD=",
		"DOLT_PORT=40899",
		"DOLT_USER=root",
		"GC_TEST_DIR="+dir,
		"PATH="+ambientDir+string(os.PathListSeparator)+os.Getenv("PATH"),
	)
	if runErr != nil {
		t.Fatalf("run_bd_pinned: %v\nstderr:\n%s", runErr, stderr)
	}
	if strings.TrimSpace(stdout) != "pinned:version" {
		t.Fatalf("stdout = %q, want selected BD_BIN output", stdout)
	}
}

func snapshotBeadsTreeExact(t *testing.T, root string) map[string]string {
	t.Helper()
	snapshot := make(map[string]string)
	err := filepath.WalkDir(root, func(path string, _ os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		info, err := os.Lstat(path)
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		payload := []byte(nil)
		switch {
		case info.Mode().IsRegular():
			payload, err = os.ReadFile(path)
		case info.Mode()&os.ModeSymlink != 0:
			var target string
			target, err = os.Readlink(path)
			payload = []byte(target)
		}
		if err != nil {
			return err
		}
		snapshot[rel] = fmt.Sprintf("mode=%#o size=%d payload=%x", uint32(info.Mode()), info.Size(), payload)
		return nil
	})
	if err != nil {
		t.Fatalf("snapshot %s: %v", root, err)
	}
	return snapshot
}

func runBashSnippet(t *testing.T, script string, env ...string) (stdout, stderr string, runErr error) {
	t.Helper()
	return runIdentityTestCommand("bash", []string{"-c", script}, append(os.Environ(), env...))
}

// runIdentityTestCommand owns this file's sole subprocess creation site. The
// resource-census ratchet counts source call sites, so every shell regression
// test goes through this helper instead of expanding the repository's process
// surface for each scenario.
func runIdentityTestCommand(name string, args, env []string) (stdout, stderr string, runErr error) {
	cmd := exec.Command(name, args...)
	if env != nil {
		cmd.Env = env
	}
	var stdoutBuf, stderrBuf bytes.Buffer
	cmd.Stdout = &stdoutBuf
	cmd.Stderr = &stderrBuf
	runErr = cmd.Run()
	return stdoutBuf.String(), stderrBuf.String(), runErr
}

func extractShellFunction(t *testing.T, script, name string) string {
	t.Helper()
	// Match the function header and capture lines until the matching
	// closing brace at column 0. The script uses the conventional
	// `name() {` ... `\n}` shape.
	pattern := regexp.MustCompile(`(?ms)^` + regexp.QuoteMeta(name) + `\(\)\s*\{.*?\n\}`)
	loc := pattern.FindStringIndex(script)
	if loc == nil {
		t.Fatalf("could not find shell function %q in script", name)
	}
	return script[loc[0]:loc[1]]
}

func writeFakeDolt(t *testing.T, dir, name, email string) {
	t.Helper()
	body := `#!/usr/bin/env bash
# Stub: only handles "config --global --get|--add user.name|user.email".
set -e
log_file=${FAKE_DOLT_LOG:-/dev/null}
case "$1 $2" in
  "config --global")
    case "$3" in
      --get)
        case "$4" in
          user.name)
` + emitGetIf(name) + `
            ;;
          user.email)
` + emitGetIf(email) + `
            ;;
        esac
        ;;
      --add)
        echo "set $4 $5" >> "$log_file"
        exit 0
        ;;
    esac
    ;;
esac
exit 0
`
	writeExecutable(t, filepath.Join(dir, "dolt"), body)
}

func writeFakeGit(t *testing.T, dir, name, email string) {
	t.Helper()
	body := `#!/usr/bin/env bash
set -e
case "$1 $2" in
  "config --global")
    case "$3" in
      user.name)
` + emitGetIf(name) + `
        ;;
      user.email)
` + emitGetIf(email) + `
        ;;
    esac
    ;;
esac
exit 0
`
	writeExecutable(t, filepath.Join(dir, "git"), body)
}

func emitGetIf(value string) string {
	if value == "" {
		return "            exit 1"
	}
	return "            echo " + value + "; exit 0"
}

func writeExecutable(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o755); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func logContains(path, want string) bool {
	data, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	return strings.Contains(string(data), want)
}

func readFile(path string) string {
	data, _ := os.ReadFile(path)
	return string(data)
}
