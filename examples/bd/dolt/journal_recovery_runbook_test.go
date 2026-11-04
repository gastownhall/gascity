package dolt_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

const journalCorruptionRunbook = "docs/journal-corruption-recovery.md"

// gcDoltVerbPattern matches a `gc dolt <verb>` invocation in the runbook.
var gcDoltVerbPattern = regexp.MustCompile(`\bgc dolt ([a-z][a-z0-9-]*)`)

// fencedBlockPattern matches one fenced code block's body.
var fencedBlockPattern = regexp.MustCompile("(?s)```[a-z]*\n(.*?)```")

func readJournalCorruptionRunbook(t *testing.T) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(repoRoot(t), journalCorruptionRunbook))
	if err != nil {
		t.Fatalf("read runbook: %v", err)
	}
	return string(data)
}

// runbookRestoreBlock returns the runbook's gc-managed restore block: the one
// fenced block that runs `dolt backup restore`. A second copy could drift
// from the block TestJournalCorruptionRunbookRestoreBlock runs.
func runbookRestoreBlock(t *testing.T, runbook string) string {
	t.Helper()
	var blocks []string
	for _, block := range fencedBlockPattern.FindAllStringSubmatch(runbook, -1) {
		if strings.Contains(block[1], "dolt backup restore") {
			blocks = append(blocks, block[1])
		}
	}
	if len(blocks) != 1 {
		t.Fatalf("runbook has %d fenced blocks that run `dolt backup restore`, want exactly 1:\n%s", len(blocks), strings.Join(blocks, "\n---\n"))
	}
	return blocks[0]
}

// TestJournalCorruptionRunbookNamesShippedDoltCommands pins the operator-facing
// journal corruption runbook to commands and paths that exist: every
// `gc dolt <verb>` it names is a shipped dolt pack command, it says where the
// automatic start-time recovery records a restore (the managed Dolt log,
// whose `gc-beads-bd:` lines are the only record of a recovery that
// succeeded), where it stages a restore, holds the finished copy and keeps
// the corrupt store, that a restore that runs out of time needs
// `gc dolt start`, and that health patrol does not run the recovery; its one
// restore block registers the backup on the restored copy again, and the
// JSONL import comes from the core jsonl-archive and is labeled a lossy last
// resort.
func TestJournalCorruptionRunbookNamesShippedDoltCommands(t *testing.T) {
	runbook := readJournalCorruptionRunbook(t)

	if strings.Contains(runbook, "gc dolt backup") {
		t.Errorf("runbook names `gc dolt backup`, which the dolt pack does not ship")
	}
	for _, match := range gcDoltVerbPattern.FindAllStringSubmatch(runbook, -1) {
		verb := match[1]
		info, err := os.Stat(filepath.Join(repoRoot(t), "commands", verb))
		if err != nil || !info.IsDir() {
			t.Errorf("runbook names `gc dolt %s`, but commands/%s/ is not shipped (stat err: %v)", verb, verb, err)
		}
	}

	if block := runbookRestoreBlock(t, runbook); !strings.Contains(block, "dolt backup add") {
		t.Errorf("the restore block does not register the backup on the restored copy (`dolt backup add`), which `dolt backup restore` leaves with none:\n%s", block)
	}

	for _, want := range []string{
		".gc/runtime/packs/dolt/corrupt-aside/",
		".gc/runtime/packs/dolt/restore-staging/",
		".gc/runtime/packs/dolt/restore-ready/",
		"GC_DOLT_JOURNAL_RESTORE_TIMEOUT_SECONDS",
		"did not finish within 45s",
		"Run `gc dolt start`",
		"gc start",
		"gc dolt logs",
		"gc-beads-bd:",
		"RESTORED '<dbname>'",
		"jsonl-archive",
		"JSONL recovery loses all non-issue history",
	} {
		if !strings.Contains(runbook, want) {
			t.Errorf("runbook missing %q", want)
		}
	}
	if !strings.Contains(strings.ToLower(runbook), "automatic") {
		t.Errorf("runbook does not describe the automatic start-time recovery")
	}
	if strings.Contains(runbook, "logged by `gc start`") {
		t.Errorf("runbook says recovery is logged by `gc start`, which discards the provider's output when start succeeds; the record is the managed Dolt log")
	}
	if strings.Contains(runbook, "runs unattended when health patrol") {
		t.Errorf("runbook says health patrol runs the recovery; its restart goes through `gc dolt-state recover-managed`, which does not")
	}
	if strings.Contains(runbook, ".beads/issues.jsonl") {
		t.Errorf("runbook still snapshots .beads/issues.jsonl, which gc no longer writes (bd auto-export is off)")
	}
}

// TestJournalCorruptionRunbookRestoreBlock runs the runbook's restore block,
// placeholders filled in, against a stub `dolt` for every repo_state.json
// backup shape and a stub `mv` that can refuse a move. It must pick bd's
// `default` backup when registered, else the legacy <dbname>-backup entry in
// its object or plain string form; restore into staging, register the
// selected backup on the restored copy, and only then swap it in for the
// corrupt store, which it keeps; leave the store untouched, running nothing,
// when the selected backup is not a non-empty local directory, another
// database registers it, or <somewhere-safe> already exists; leave it
// untouched when the restore fails, when the store cannot be moved aside,
// and when the copy cannot be moved in, which moves the store back; keep and
// name both copies when neither can be moved; keep the restore and print the
// command to run when only the registration fails; and leave a staging
// directory behind only when it holds the restored copy a CRITICAL line
// names.
func TestJournalCorruptionRunbookRestoreBlock(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not available; skipping runbook snippet test")
	}
	if _, err := exec.LookPath("jq"); err != nil {
		t.Skip("jq not available; skipping runbook snippet test")
	}
	realMv, err := exec.LookPath("mv")
	if err != nil {
		t.Fatalf("find mv: %v", err)
	}
	block := runbookRestoreBlock(t, readJournalCorruptionRunbook(t))

	// usable and empty are backup directories; data is <data_dir> and aside
	// is <somewhere-safe>.
	type dirs struct{ usable, empty, data, aside string }
	usableDefault := func(d dirs) string { return `{"backups":{"default":{"url":"file://` + d.usable + `"}}}` }
	usableURL := func(d dirs) string { return "file://" + d.usable }
	// Where prod ends up.
	const (
		swapped   = "swapped"   // the restored copy is in the data dir, the store at <somewhere-safe>
		untouched = "untouched" // the corrupt store is still in place
		missing   = "missing"   // neither could be moved, and both copies are kept
	)
	tests := []struct {
		name      string
		repoState func(d dirs) string
		// otherRepoState, when set, is the repo_state.json of a second
		// database, other, in the same data dir.
		otherRepoState func(d dirs) string
		// restoreExit and addExit are the stub's exit codes for
		// `dolt backup restore` and `dolt backup add`.
		restoreExit, addExit int
		// failMoves are the sources, as globs, of the moves the mv stub
		// refuses.
		failMoves func(d dirs) []string
		// asideExists creates <somewhere-safe> before the block runs.
		asideExists bool
		// wantName is the backup the block restores from and registers
		// again; empty means no restore succeeds.
		wantName string
		wantURL  func(d dirs) string
		// result is where prod ends up: swapped when wantName is set and
		// untouched when it is not, unless result says otherwise.
		result string
		// wantOut is a substring of the block's output.
		wantOut func(d dirs) string
	}{
		{
			name: "default object form",
			repoState: func(d dirs) string {
				return `{"head":"refs/heads/main","remotes":{},"backups":{"default":{"name":"default","url":"file://` + d.usable + `","fetch_specs":["refs/heads/*:refs/remotes/default/*"],"params":{}}},"branches":{}}`
			},
			wantName: "default",
			wantURL:  usableURL,
		},
		{
			name: "legacy object form",
			repoState: func(d dirs) string {
				return `{"backups":{"prod-backup":{"name":"prod-backup","url":"file://` + d.usable + `","fetch_specs":[],"params":{}}}}`
			},
			wantName: "prod-backup",
			wantURL:  usableURL,
		},
		{
			name:      "legacy string form",
			repoState: func(d dirs) string { return `{"backups":{"prod-backup":"file://` + d.usable + `"}}` },
			wantName:  "prod-backup",
			wantURL:   usableURL,
		},
		{
			name: "default wins over legacy",
			repoState: func(d dirs) string {
				return `{"backups":{"prod-backup":"file:///backups/legacy","default":{"url":"file://` + d.usable + `"}}}`
			},
			wantName: "default",
			wantURL:  usableURL,
		},
		{
			name:      "no backup registered",
			repoState: func(dirs) string { return `{"head":"refs/heads/main","backups":{}}` },
		},
		{
			// Like auto-recovery, the block does not fall through to an
			// older legacy backup when the default one is unusable.
			name: "default dir empty with usable legacy entry",
			repoState: func(d dirs) string {
				return `{"backups":{"prod-backup":{"url":"file://` + d.usable + `"},"default":{"url":"file://` + d.empty + `"}}}`
			},
		},
		{
			name:      "default entry without a url",
			repoState: func(dirs) string { return `{"backups":{"default":{"name":"default","params":{}}}}` },
		},
		{
			name:      "default is a remote backup",
			repoState: func(dirs) string { return `{"backups":{"default":{"url":"aws://bucket/prod"}}}` },
		},
		{
			// A directory two databases sync to holds whichever synced
			// last, so it may hold the other database's data.
			name:           "default dir shared with another database",
			repoState:      usableDefault,
			otherRepoState: func(d dirs) string { return `{"backups":{"other-backup":"file://` + d.usable + `"}}` },
			wantOut:        func(dirs) string { return "database other registers the same directory; the store is untouched" },
		},
		{
			name:           "another database with its own backup",
			repoState:      usableDefault,
			otherRepoState: func(dirs) string { return `{"backups":{"default":{"url":"file:///backups/other"}}}` },
			wantName:       "default",
			wantURL:        usableURL,
		},
		{
			// mv would move the store into an existing directory, and moving
			// that directory back would then put the wrong path in place.
			name:        "somewhere-safe already exists",
			repoState:   usableDefault,
			asideExists: true,
			wantOut: func(d dirs) string {
				return d.aside + " already exists: pick a new path to keep the corrupt store at; the store is untouched"
			},
		},
		{
			name:        "restore fails",
			repoState:   usableDefault,
			restoreExit: 1,
			wantOut:     func(dirs) string { return "the restore failed; the store is untouched" },
		},
		{
			// The restore stands; only the registration is left to do.
			name:      "backup add fails",
			repoState: usableDefault,
			addExit:   1,
			wantName:  "default",
			wantURL:   usableURL,
			wantOut: func(d dirs) string {
				return "WARNING: the restored prod has no backup registered: with the server down, run \"dolt backup add default 'file://" + d.usable + "'\" in "
			},
		},
		{
			name:      "corrupt store cannot be moved aside",
			repoState: usableDefault,
			failMoves: func(d dirs) []string { return []string{filepath.Join(d.data, "prod")} },
			wantName:  "default",
			wantURL:   usableURL,
			result:    untouched,
			wantOut: func(d dirs) string {
				return "could not move " + filepath.Join(d.data, "prod") + " to " + d.aside + "; the store is untouched"
			},
		},
		{
			name:      "restored copy cannot be moved in",
			repoState: usableDefault,
			failMoves: func(d dirs) []string { return []string{d.data + ".restore.*/prod"} },
			wantName:  "default",
			wantURL:   usableURL,
			result:    untouched,
			wantOut: func(d dirs) string {
				return "could not move the restored copy into " + d.data + "; the store is back in place, untouched"
			},
		},
		{
			name:      "neither can be moved",
			repoState: usableDefault,
			failMoves: func(d dirs) []string { return []string{d.data + ".restore.*/prod", d.aside} },
			wantName:  "default",
			wantURL:   usableURL,
			result:    missing,
			wantOut: func(d dirs) string {
				return "CRITICAL: the restored copy could not be moved into " + d.data + " AND the corrupt store could not be moved back; original data is at " +
					d.aside + ", the restored copy at " + d.data + ".restore."
			},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			d := dirs{
				usable: filepath.Join(root, "backups", "prod"),
				empty:  filepath.Join(root, "backups", "empty"),
				data:   filepath.Join(root, "dolt-data"),
				aside:  filepath.Join(root, "aside-prod"),
			}
			binDir := filepath.Join(root, "bin")
			restoreCalls := filepath.Join(root, "restore-calls.log")
			addCalls := filepath.Join(root, "backup-add-calls.log")
			for _, dir := range []string{d.usable, d.empty, filepath.Join(d.data, "prod", ".dolt"), binDir} {
				if err := os.MkdirAll(dir, 0o755); err != nil {
					t.Fatalf("mkdir %s: %v", dir, err)
				}
			}
			if tc.asideExists {
				if err := os.MkdirAll(d.aside, 0o755); err != nil {
					t.Fatalf("mkdir %s: %v", d.aside, err)
				}
			}
			if err := os.WriteFile(filepath.Join(d.usable, "manifest"), []byte("backup"), 0o644); err != nil {
				t.Fatalf("seed backup: %v", err)
			}
			if err := os.WriteFile(filepath.Join(d.data, "prod", "original-marker"), []byte("prod"), 0o644); err != nil {
				t.Fatalf("write original marker: %v", err)
			}
			if err := os.WriteFile(filepath.Join(d.data, "prod", ".dolt", "repo_state.json"), []byte(tc.repoState(d)), 0o644); err != nil {
				t.Fatalf("write repo_state.json: %v", err)
			}
			if tc.otherRepoState != nil {
				if err := os.MkdirAll(filepath.Join(d.data, "other", ".dolt"), 0o755); err != nil {
					t.Fatalf("mkdir other database: %v", err)
				}
				if err := os.WriteFile(filepath.Join(d.data, "other", ".dolt", "repo_state.json"), []byte(tc.otherRepoState(d)), 0o644); err != nil {
					t.Fatalf("write other repo_state.json: %v", err)
				}
			}
			writeExecutable(t, filepath.Join(binDir, "dolt"), `#!/usr/bin/env bash
set -u
case "${1:-} ${2:-}" in
  "backup restore")
    printf '%s %s\n' "$3" "$4" >> "$RESTORE_CALLS"
    mkdir -p "$PWD/$4/.dolt"
    if [ "$RESTORE_EXIT" != 0 ]; then
      echo partial > "$PWD/$4/partial-marker"
      exit "$RESTORE_EXIT"
    fi
    echo restored > "$PWD/$4/restored-marker"
    ;;
  "backup add")
    printf '%s %s %s\n' "$(basename "$PWD")" "$3" "$4" >> "$ADD_CALLS"
    exit "$ADD_EXIT"
    ;;
  *)
    echo "unexpected dolt call: $*" >&2
    exit 64
    ;;
esac
`)
			writeExecutable(t, filepath.Join(binDir, "mv"), `#!/usr/bin/env bash
set -f
IFS=:
for glob in ${FAIL_MOVES:-}; do
  case "$1" in
    $glob)
      echo "mv: cannot move '$1' to '$2': Permission denied" >&2
      exit 1
      ;;
  esac
done
exec '`+realMv+`' "$@"
`)
			var failMoves []string
			if tc.failMoves != nil {
				failMoves = tc.failMoves(d)
			}
			script := strings.NewReplacer(
				"<data_dir>", d.data,
				"<dbname>", "prod",
				"<somewhere-safe>", d.aside,
			).Replace(block)
			cmd := exec.Command("bash", "-c", "set -u\n"+script)
			cmd.Env = append(os.Environ(),
				"PATH="+binDir+string(os.PathListSeparator)+os.Getenv("PATH"),
				"RESTORE_CALLS="+restoreCalls,
				"ADD_CALLS="+addCalls,
				"RESTORE_EXIT="+strconv.Itoa(tc.restoreExit),
				"ADD_EXIT="+strconv.Itoa(tc.addExit),
				"FAIL_MOVES="+strings.Join(failMoves, ":"),
			)
			out, err := cmd.CombinedOutput()
			if err != nil {
				t.Fatalf("restore block failed: %v\n%s", err, out)
			}
			result := tc.result
			if result == "" {
				result = untouched
				if tc.wantName != "" {
					result = swapped
				}
			}
			restored := readFileOrEmpty(t, restoreCalls)
			added := readFileOrEmpty(t, addCalls)
			staging, _ := filepath.Glob(d.data + ".restore.*")
			if result == missing {
				if len(staging) != 1 {
					t.Fatalf("the block must keep the staging directory that holds the restored copy, got %v\n%s", staging, out)
				}
				if _, err := os.Stat(filepath.Join(staging[0], "prod", "restored-marker")); err != nil {
					t.Fatalf("the restored copy must be kept in %s: %v\n%s", staging[0], err, out)
				}
			} else if len(staging) != 0 {
				t.Fatalf("the block left staging directories behind: %v\n%s", staging, out)
			}
			if tc.wantOut != nil && !strings.Contains(string(out), tc.wantOut(d)) {
				t.Fatalf("output missing %q:\n%s", tc.wantOut(d), out)
			}

			if tc.wantName == "" {
				if added != "" {
					t.Fatalf("no backup add may run unless the restore succeeded, backup add calls %q\n%s", added, out)
				}
				if tc.restoreExit == 0 && restored != "" {
					t.Fatalf("no restore may run without a usable backup and a free <somewhere-safe>, restore calls %q\n%s", restored, out)
				}
				if tc.restoreExit != 0 && restored == "" {
					t.Fatalf("the failing restore never ran\n%s", out)
				}
			} else {
				url := tc.wantURL(d)
				if want := url + " prod\n"; restored != want {
					t.Fatalf("restore calls = %q, want %q\n%s", restored, want, out)
				}
				if want := "prod " + tc.wantName + " " + url + "\n"; added != want {
					t.Fatalf("backup add calls = %q, want %q: the restored copy must register its backup again\n%s", added, want, out)
				}
			}

			switch result {
			case swapped:
				if _, err := os.Stat(filepath.Join(d.aside, "original-marker")); err != nil {
					t.Fatalf("the corrupt store must be kept at %s: %v\n%s", d.aside, err, out)
				}
				if _, err := os.Stat(filepath.Join(d.data, "prod", "restored-marker")); err != nil {
					t.Fatalf("prod was not restored: %v\n%s", err, out)
				}
			case untouched:
				for _, marker := range []string{"original-marker", "restored-marker", "partial-marker"} {
					_, err := os.Stat(filepath.Join(d.data, "prod", marker))
					if exists := err == nil; exists != (marker == "original-marker") {
						t.Fatalf("the store must stay in place, untouched (%s exists: %v, stat err: %v)\n%s", marker, exists, err, out)
					}
				}
				if tc.asideExists {
					if entries, err := os.ReadDir(d.aside); err != nil || len(entries) != 0 {
						t.Fatalf("nothing may be moved into the existing %s (entries %v, err %v)\n%s", d.aside, entries, err, out)
					}
				} else if _, err := os.Stat(d.aside); !os.IsNotExist(err) {
					t.Fatalf("the store must not stay moved aside (stat err: %v)\n%s", err, out)
				}
				if !strings.Contains(string(out), "the store is untouched") && !strings.Contains(string(out), "the store is back in place, untouched") {
					t.Fatalf("the block must say it left the store alone:\n%s", out)
				}
			case missing:
				if _, err := os.Stat(filepath.Join(d.data, "prod")); !os.IsNotExist(err) {
					t.Fatalf("nothing may take prod's place in the data dir (stat err: %v)\n%s", err, out)
				}
				if _, err := os.Stat(filepath.Join(d.aside, "original-marker")); err != nil {
					t.Fatalf("the corrupt store must be kept at %s: %v\n%s", d.aside, err, out)
				}
			}
		})
	}
}

// TestDoltStartLetsAJournalRestoreRunToCompletion pins the command the
// recovery's time-limit message and this runbook send operators to: gc runs
// `gc dolt start` with no time limit, so it runs the provider's start with no
// limit on an automatic journal restore either, unless the operator set one.
func TestDoltStartLetsAJournalRestoreRunToCompletion(t *testing.T) {
	root := repoRoot(t)
	for _, tc := range []struct {
		name, setting, want string
	}{
		{name: "no limit by default", want: "start 0"},
		{name: "operator limit kept", setting: "600", want: "start 600"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cityPath := t.TempDir()
			seen := filepath.Join(cityPath, "start-calls.log")
			scriptsDir := filepath.Join(cityPath, ".gc", "scripts")
			if err := os.MkdirAll(scriptsDir, 0o755); err != nil {
				t.Fatalf("mkdir fake gc-beads-bd dir: %v", err)
			}
			writeExecutable(t, filepath.Join(scriptsDir, "gc-beads-bd.sh"),
				"#!/bin/sh\nprintf '%s %s\\n' \"$1\" \"${GC_DOLT_JOURNAL_RESTORE_TIMEOUT_SECONDS-unset}\" >> '"+seen+"'\n")
			cmd := exec.Command("sh", filepath.Join(root, "commands", "start", "run.sh"))
			cmd.Env = append(filteredEnv("GC_CITY_PATH", "GC_PACK_DIR", "GC_DOLT_HOST", "GC_DOLT_PORT", "PATH", "GC_DOLT_JOURNAL_RESTORE_TIMEOUT_SECONDS"),
				"GC_CITY_PATH="+cityPath,
				"GC_PACK_DIR="+root,
				"GC_DOLT_PORT=3307",
				"PATH="+os.Getenv("PATH"),
			)
			if tc.setting != "" {
				cmd.Env = append(cmd.Env, "GC_DOLT_JOURNAL_RESTORE_TIMEOUT_SECONDS="+tc.setting)
			}
			out, err := cmd.CombinedOutput()
			if err != nil {
				t.Fatalf("gc dolt start failed: %v\n%s", err, out)
			}
			if got := strings.TrimSpace(readFileOrEmpty(t, seen)); got != tc.want {
				t.Fatalf("gc-beads-bd calls = %q, want %q\n%s", got, tc.want, out)
			}
		})
	}
}

// readFileOrEmpty returns a file a stub writes only when it is called, and
// "" when it was never created.
func readFileOrEmpty(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return ""
	}
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(data)
}
