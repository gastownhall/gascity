package config

import (
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/bazeltest"
	"github.com/gastownhall/gascity/internal/builtinpacks"
)

// gascity-packs 9b4df3e ("route shipped beads commands through gc",
// 2026-07-13) moved every shipped bead command onto `gc bd`. Graph.v2 steps
// and roots live in the city binding on a split-store city; a raw `bd` in a
// role prompt, workflow page or check script reads the agent's work store
// instead, so role workers loop on CLAIM_REJECTED, steps never close and
// every check fails. These tests keep the public pack pins, and the copy the
// binary embeds, at or past that fix.

// preBdRoutingFixPins are the gascity-packs registry release commits that
// predate 9b4df3e. Fresh init must never pin one of them again.
var preBdRoutingFixPins = map[string]string{
	// gascity releases 0.1.1 through 0.1.6.
	"sha:788b6e8ec224a8951c728ef6da74dab8bc04d474": "gascity 0.1.1",
	"sha:5fc675b85d4ae0ebca2f17cb027a24b03f2832f8": "gascity 0.1.2",
	"sha:af1640917a24f88126c37a1e3697a619b731cc0f": "gascity 0.1.3",
	"sha:99464ed9240b1f6e6b7ab1d351f67016e1a973ff": "gascity 0.1.4",
	"sha:3b3b89f2011e06d84459aa7bea1552382f13930a": "gascity 0.1.6",
	// gastown releases 0.1.6 through 0.1.11. 0.1.11 was cut on
	// release/gastown-0.1.11 from 0.1.10 and does not carry the fix.
	"sha:4212acb7046c11f6f633df73307006493185233a": "gastown 0.1.6",
	"sha:342bcfb0775ad79d2c67df3b235edf70a0a7e372": "gastown 0.1.7",
	"sha:2cd3360f36b2ff55f6c306546841963cbca1ed69": "gastown 0.1.8",
	"sha:92a9e8558b86854264ccc082fe9f27d48db3c749": "gastown 0.1.9",
	"sha:33d3a430a67d1782ad364556cb566bdb01d0afe3": "gastown 0.1.10",
	"sha:66c672b5ed00af143158b4eac960523c56b977ee": "gastown 0.1.11",
}

func TestPublicPackPinsPostdateBdRoutingFix(t *testing.T) {
	for name, pin := range map[string]string{
		"PublicGastownPackVersion": PublicGastownPackVersion,
		"PublicGascityPackVersion": PublicGascityPackVersion,
	} {
		if release, bad := preBdRoutingFixPins[pin]; bad {
			t.Errorf("%s = %s (%s), which predates gascity-packs 9b4df3e: its shipped prompts and check scripts run raw bd against graph steps", name, pin, release)
		}
	}
}

// The gastown pin and the embedded module are one commit
// (scripts/update-bundled-gastown-pack pins go.mod with go get), so the
// embedded-content scan below speaks for the pinned commit. CI's
// `update-bundled-gastown-pack --check` ties the gascity pin to the same
// embedded gascity tree.
func TestPublicGastownPinIsEmbeddedModuleCommit(t *testing.T) {
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	repoRoot := bazeltest.OverrideRoot()
	if repoRoot == "" {
		repoRoot = filepath.Clean(filepath.Join(filepath.Dir(thisFile), "..", ".."))
	}
	data, err := os.ReadFile(filepath.Join(repoRoot, "go.mod"))
	if err != nil {
		t.Fatalf("read go.mod: %v", err)
	}
	m := regexp.MustCompile(`(?m)^\s*github\.com/gastownhall/gascity-packs\s+v\S+-([0-9a-f]{12})\s*$`).FindSubmatch(data)
	if m == nil {
		t.Fatal("go.mod: gascity-packs is not pinned to a commit pseudo-version")
	}
	if !strings.HasPrefix(PublicGastownPackVersion, "sha:"+string(m[1])) {
		t.Errorf("PublicGastownPackVersion = %s, but go.mod embeds gascity-packs %s", PublicGastownPackVersion, m[1])
	}
}

// bdSubcommands is the bd verb list gascity-packs main's
// tests/test_no_bare_bd_commands.py guards on; keep the two in step.
var bdSubcommands = []string{
	"admin",
	"ado",
	"assign",
	"audit",
	"backup",
	"batch",
	"blocked",
	"bootstrap",
	"branch",
	"children",
	"close",
	"comment",
	"comments",
	"compact",
	"completion",
	"config",
	"context",
	"cook",
	"count",
	"create",
	"create-form",
	"defer",
	"delete",
	"dep",
	"diff",
	"doctor",
	"dolt",
	"duplicate",
	"duplicates",
	"edit",
	"epic",
	"export",
	"federation",
	"find-duplicates",
	"flatten",
	"forget",
	"formula",
	"gate",
	"gc",
	"github",
	"gitlab",
	"graph",
	"heartbeat",
	"help",
	"history",
	"hooks",
	"human",
	"import",
	"info",
	"init",
	"init-safety",
	"jira",
	"kv",
	"label",
	"linear",
	"link",
	"list",
	"lint",
	"mail",
	"memories",
	"merge-slot",
	"meta",
	"metrics",
	"migrate",
	"migrate-personal",
	"mol",
	"note",
	"notion",
	"onboard",
	"orphans",
	"ping",
	"preflight",
	"prime",
	"priority",
	"promote",
	"prune",
	"purge",
	"q",
	"query",
	"quickstart",
	"ready",
	"recall",
	"reclaim",
	"recompute-blocked",
	"remember",
	"rename-prefix",
	"rename",
	"reopen",
	"repo",
	"restore",
	"rules",
	"search",
	"set-state",
	"setup",
	"show",
	"ship",
	"sql",
	"stale",
	"state",
	"status",
	"statuses",
	"supersede",
	"swarm",
	"sync",
	"tag",
	"todo",
	"type",
	"types",
	"unclaim",
	"undefer",
	"update",
	"upgrade",
	"vc",
	"version",
	"where",
	"worktree",
}

// The detector is a port of gascity-packs main's
// tests/test_no_bare_bd_commands.py, plus `bd "$@"` / `bd $*` (the form the
// gastown review check scripts used) and backslash-continued commands.
var (
	bdSubcommandPattern = func() string {
		quoted := make([]string, len(bdSubcommands))
		for i, c := range bdSubcommands {
			quoted[i] = regexp.QuoteMeta(c)
		}
		return strings.Join(quoted, "|")
	}()
	bareBdCommand     = regexp.MustCompile(`\bbd[ \t]+(?:` + bdSubcommandPattern + `)\b`)
	bareBdLeadingFlag = regexp.MustCompile(`\bbd[ \t]+-{1,2}[A-Za-z]`)
	bareBdDynamic     = regexp.MustCompile(`\bbd[ \t]+(?:["']?\$\{?[A-Za-z_@*]|\$\()`)
	bareBdMultiline   = regexp.MustCompile(`\bbd[ \t]*(?:\\[ \t]*)?\r?\n[ \t]*(?:` + bdSubcommandPattern + `)\b`)
	bareBdGoExec      = regexp.MustCompile(`(?:exec\.Command(?:Context)?|dispatchExecCommand)\(\s*["']bd["']`)
	bareBdPathCheck   = regexp.MustCompile(`\bcommand[ \t]+-v[ \t]+bd\b`)
	// An argv list (Python, JSON, Go) whose first element is "bd".
	bareBdArgv = regexp.MustCompile(`(?:\[|\{|\(|=|:)\s*["']bd["']\s*[,\]\)]`)
)

// gcRoutesBd reports whether the bd starting at line[bdStart] is the verb of a
// `gc [--city X] [--rig Y] bd` invocation. The gc must be its own command word:
// `GC_BIN=gc bd show` runs raw bd with an environment variable set.
func gcRoutesBd(line string, bdStart int) bool {
	prefix := line[:bdStart]
	gcEnd := -1
	for i := strings.LastIndex(prefix, "gc"); i >= 0; i = strings.LastIndex(prefix[:i], "gc") {
		if i > 0 && strings.ContainsRune("ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789_=/.-", rune(prefix[i-1])) {
			continue
		}
		end := i + 2
		if end < len(prefix) && !strings.ContainsRune(" \t", rune(prefix[end])) {
			continue
		}
		gcEnd = end
		break
	}
	if gcEnd < 0 {
		return false
	}
	tokens := strings.Fields(prefix[gcEnd:])
	for i := 0; i < len(tokens); i++ {
		switch tok := tokens[i]; {
		case tok == "--city" || tok == "--rig":
			if i+1 >= len(tokens) {
				return false
			}
			i++
		case (strings.HasPrefix(tok, "--city=") || strings.HasPrefix(tok, "--rig=")) && !strings.HasSuffix(tok, "="):
		default:
			return false
		}
	}
	return true
}

// rawBdInvocations reports the 1-based lines of text that run bd directly
// instead of through `gc bd`.
func rawBdInvocations(text string) []int {
	seen := map[int]bool{}
	var lines []int
	add := func(n int) {
		if !seen[n] {
			seen[n] = true
			lines = append(lines, n)
		}
	}
	for i, line := range strings.Split(text, "\n") {
		for _, re := range []*regexp.Regexp{bareBdCommand, bareBdLeadingFlag, bareBdDynamic} {
			for _, loc := range re.FindAllStringIndex(line, -1) {
				if !gcRoutesBd(line, loc[0]) {
					add(i + 1)
				}
			}
		}
		if bareBdGoExec.MatchString(line) || bareBdPathCheck.MatchString(line) || strings.Contains(line, "BD_BIN") || bareBdArgv.MatchString(line) {
			add(i + 1)
		}
	}
	for _, loc := range bareBdMultiline.FindAllStringIndex(text, -1) {
		lineStart := strings.LastIndex(text[:loc[0]], "\n") + 1
		lineEnd := strings.Index(text[loc[0]:], "\n") + loc[0]
		if gcRoutesBd(text[lineStart:lineEnd], loc[0]-lineStart) {
			continue
		}
		add(strings.Count(text[:loc[0]], "\n") + 1)
	}
	sort.Ints(lines)
	return lines
}

func TestRawBdInvocationsDetector(t *testing.T) {
	for line, want := range map[string]bool{
		// Bypass attempts: every one of these runs bd without gc.
		`bd show "$WORK_ID" --json`:                                  true,
		`  STATUS=$(bd show "$ID" --json | jq -r .status)`:           true,
		"run `bd close $GC_BEAD_ID`":                                 true,
		`/usr/local/bin/bd update x --status closed`:                 true,
		`if output=$(bd "$@" 2>"$stderr_file" | json_payload); then`: true,
		`bd $* --json`:                               true,
		`bd "$verb" "$id"`:                           true,
		`bd ${VERB} x`:                               true,
		`bd $(pick_verb) x`:                          true,
		`bd --json show x`:                           true,
		`bd -q close x`:                              true,
		`GC_BIN=gc bd show x`:                        true,
		`FOO=gc bd list`:                             true,
		`gc-bd list --json`:                          true,
		`subprocess.run(["bd", "show", bead_id])`:    true,
		`argv = ['bd', 'list', '--json']`:            true,
		"cmd := exec.Command(" + `"bd", "show", id)`: true,
		`if ! command -v bd >/dev/null; then`:        true,
		`"${BD_BIN:-bd}" show x`:                     true,
		`bd unclaim x`:                               true,
		`bd set-state x y`:                           true,
		// Routed through gc, or not a command.
		`gc bd show "$WORK_ID" --json`:     false,
		`gc --city "$GC_CITY" bd update x`: false,
		`gc --rig=r bd close x`:            false,
		`gc --city c --rig r bd list`:      false,
		`exec gc bd "$@"`:                  false,
		`the bd CLI owns its own commit`:   false,
		`a bead id like bd-123`:            false,
	} {
		if got := len(rawBdInvocations(line)) > 0; got != want {
			t.Errorf("rawBdInvocations(%q) = %v, want %v", line, got, want)
		}
	}
	multiline := "STEP=$(bd \\\n  show x)\n"
	if got := rawBdInvocations(multiline); len(got) != 1 || got[0] != 1 {
		t.Errorf("rawBdInvocations(backslash-continued bd show) = %v, want [1]", got)
	}
	routed := "STEP=$(gc bd \\\n  show x)\n"
	if got := rawBdInvocations(routed); len(got) != 0 {
		t.Errorf("rawBdInvocations(backslash-continued gc bd show) = %v, want none", got)
	}
}

// The shipped prompts, workflow pages, formulas and scripts of every pack the
// binary embeds must not run bd directly: the public gastown and gascity packs,
// and the bundled core, bd and dolt packs.
func TestEmbeddedPacksRouteBeadsCommandsThroughGC(t *testing.T) {
	scanned := map[string]int{}
	for _, pack := range builtinpacks.All() {
		err := fs.WalkDir(pack.FS, ".", func(p string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() {
				if d.Name() == "tests" || d.Name() == "vendor" || d.Name() == "testdata" {
					return fs.SkipDir
				}
				return nil
			}
			switch path.Ext(p) {
			case ".md", ".tmpl", ".toml", ".sh", ".bash", ".py":
			default:
				return nil
			}
			data, err := fs.ReadFile(pack.FS, p)
			if err != nil {
				return err
			}
			scanned[pack.Name]++
			key := pack.Name + "/" + p
			if _, ok := rawBdAllowedFiles[key]; ok {
				return nil
			}
			lines := strings.Split(string(data), "\n")
			for _, n := range rawBdInvocations(string(data)) {
				line := strings.TrimSpace(lines[n-1])
				// The bundled packs are gc's own maintenance scripts; their
				// comments name bd verbs to explain what the gc bd call does.
				// The public packs are agent instructions, where prose is read
				// as a command, so their comments stay in scope.
				if bundledPacks[pack.Name] && strings.HasPrefix(line, "#") {
					continue
				}
				if rawBdAllowedLines[key][line] {
					continue
				}
				t.Errorf("%s:%d runs bd directly; use gc bd so graph steps resolve in the binding: %s", key, n, line)
			}
			return nil
		})
		if err != nil {
			t.Fatalf("walk %s: %v", pack.Name, err)
		}
	}
	for _, name := range []string{"core", "bd", "dolt", "gastown", "gascity"} {
		if scanned[name] == 0 {
			t.Errorf("scanned no embedded %s pack files", name)
		}
	}
}

var bundledPacks = map[string]bool{"core": true, "bd": true, "dolt": true}

// rawBdAllowedFiles are embedded files that run bd directly by design.
var rawBdAllowedFiles = map[string]string{
	"bd/assets/scripts/gc-beads-bd.sh": "the exec beads provider: it is the bd transport gc itself calls",
	"bd/doctor/check-bd/run.sh":        "the doctor check for the bd binary: it must probe bd, not gc",
}

// rawBdAllowedLines are exact (trimmed) lines that mention bd without running
// it against graph beads, keyed by embedded file.
var rawBdAllowedLines = func() map[string]map[string]bool {
	lines := map[string]map[string]bool{
		// `gc dolt sync --purge` walks the Dolt databases and purges closed
		// ephemeral beads per scope, pinning each store by its BEADS_DIR. It
		// is store maintenance on a resolved path, not a graph-step write.
		// It does not reach a split city's binding (a known S6 coverage gap).
		"dolt/commands/sync/run.sh": {
			`purged=$(BEADS_DIR="$beads_dir" bd purge $purge_args 2>/dev/null | grep -c "purged" || true)`: true,
		},
		// Prose: the order description names bd's backup verb; the exec runs
		// it through gc bd.
		"dolt/orders/mol-dog-backup.toml": {
			`description = "Back up every bead scope with bd backup"`: true,
		},
	}
	// The dolt pack is also embedded as the bd pack's dolt/ subpack.
	for key, set := range lines {
		if strings.HasPrefix(key, "dolt/") {
			lines["bd/"+key] = set
		}
	}
	return lines
}()
