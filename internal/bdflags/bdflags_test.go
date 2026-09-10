package bdflags

import (
	"errors"
	"reflect"
	"sort"
	"sync"
	"testing"
)

func TestParseHelpFlagsToSets(t *testing.T) {
	helpText := `Flags:
      -a, --assignee string   Assignee
      --help                  help for update
      --if-assignee string    Apply only if assignee matches
  Global Flags:
      --json                  Output in JSON format`

	parsed := discoveredFlags{
		value: map[string]bool{},
		bool:  map[string]bool{},
	}
	parseHelpFlagsToSets(helpText, &parsed)
	if !parsed.value["--assignee"] {
		t.Fatalf("--assignee expected as value flag")
	}
	if !parsed.bool["--help"] {
		t.Fatalf("--help expected as bool flag")
	}
	if !parsed.value["--if-assignee"] {
		t.Fatalf("--if-assignee expected as value flag")
	}
	if !parsed.bool["--json"] {
		t.Fatalf("--json expected as bool flag")
	}
}

func TestValueFlagsIncorporatesDiscovered(t *testing.T) {
	orig := runBdHelpForSubcommand
	runBdHelpForSubcommand = func(sub string) ([]byte, error) {
		if sub != "update" {
			return nil, errors.New("unexpected subcommand")
		}
		return []byte(`Flags:
  -h, --help                     help for update
  -s, --status string            New status
  --if-assignee string           Apply the update only if assignee matches
Global Flags:
  --json                          Output in JSON format`), nil
	}
	defer func() {
		runBdHelpForSubcommand = orig
		parseDiscoveredOnce = sync.Map{}
	}()

	flags := ValueFlagsWithDiscovery("update")
	if !flags["--if-assignee"] {
		t.Fatalf("ValueFlagsWithDiscovery(update)[--if-assignee] = false, want true")
	}
	if !flags["--status"] {
		t.Fatalf("ValueFlagsWithDiscovery(update)[--status] = false, want true")
	}
}

func TestBoolFlagsIncorporatesDiscovered(t *testing.T) {
	orig := runBdHelpForSubcommand
	runBdHelpForSubcommand = func(sub string) ([]byte, error) {
		if sub != "update" {
			return nil, errors.New("unexpected subcommand")
		}
		return []byte(`Flags:
  --allow-empty-description      Allow empty description
  --if-assignee string           Apply only if assignee matches`), nil
	}
	defer func() {
		runBdHelpForSubcommand = orig
		parseDiscoveredOnce = sync.Map{}
	}()

	flags := BoolFlagsWithDiscovery("update")
	if !flags["--allow-empty-description"] {
		t.Fatalf("BoolFlagsWithDiscovery(update)[--allow-empty-description] = false, want true")
	}
	if !flags["--no-history"] {
		t.Fatalf("BoolFlagsWithDiscovery(update)[--no-history] = false, want true")
	}
}

func TestSubcommandsListsAllKnownKeys(t *testing.T) {
	want := []string{
		"close", "create", "delete", "dep add", "dep list", "dep remove",
		"gate check", "gate list", "list", "mol burn", "mol current",
		"mol pour", "mol wisp", "mol wisp gc", "ready", "reopen", "show", "update",
	}
	got := Subcommands()
	sort.Strings(got)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Subcommands() = %v, want %v", got, want)
	}
}

func TestKnownRecognizesManifestKeys(t *testing.T) {
	for _, sub := range Subcommands() {
		if !Known(sub) {
			t.Errorf("Known(%q) = false, want true", sub)
		}
	}
	if Known("formula show") {
		t.Errorf("Known(%q) = true, want false (out of scope subcommand)", "formula show")
	}
	if Known("") {
		t.Errorf(`Known("") = true, want false`)
	}
}

func TestValueFlagsUnknownSubcommandReturnsNil(t *testing.T) {
	if got := ValueFlags("formula show"); got != nil {
		t.Fatalf("ValueFlags(unknown) = %v, want nil", got)
	}
}

func TestBoolFlagsUnknownSubcommandReturnsNil(t *testing.T) {
	if got := BoolFlags("formula show"); got != nil {
		t.Fatalf("BoolFlags(unknown) = %v, want nil", got)
	}
}

// Every known subcommand must include the global flags shared by the whole
// bd CLI (--json, --actor, etc.) merged into its per-subcommand set.
func TestGlobalFlagsPresentOnEverySubcommand(t *testing.T) {
	for _, sub := range Subcommands() {
		boolFlags := BoolFlags(sub)
		if !boolFlags["--json"] {
			t.Errorf("BoolFlags(%q) missing global --json", sub)
		}
		if !boolFlags["-v"] || !boolFlags["--verbose"] {
			t.Errorf("BoolFlags(%q) missing global -v/--verbose", sub)
		}
		valueFlags := ValueFlags(sub)
		if !valueFlags["--actor"] {
			t.Errorf("ValueFlags(%q) missing global --actor", sub)
		}
		if !valueFlags["-C"] || !valueFlags["--directory"] {
			t.Errorf("ValueFlags(%q) missing global -C/--directory", sub)
		}
	}
}

func TestCreateStatusFlagsConsumeValues(t *testing.T) {
	value := ValueFlags("create")
	for _, flag := range []string{"-s", "--status"} {
		if !value[flag] {
			t.Errorf("ValueFlags(create)[%q] = false, want true", flag)
		}
	}
}

func TestUpdateFlagSets(t *testing.T) {
	value := ValueFlags("update")
	for _, f := range []string{"--assignee", "-a", "--status", "-s", "--priority", "-p", "--set-metadata", "--unset-metadata", "--parent", "--type", "-t"} {
		if !value[f] {
			t.Errorf("ValueFlags(update)[%q] = false, want true", f)
		}
	}
	boolFlags := BoolFlags("update")
	for _, f := range []string{"--claim", "--ephemeral", "--persistent", "--stdin"} {
		if !boolFlags[f] {
			t.Errorf("BoolFlags(update)[%q] = false, want true", f)
		}
	}
}

func TestCloseFlagSets(t *testing.T) {
	value := ValueFlags("close")
	for _, f := range []string{"-r", "--reason", "--reason-file", "--session"} {
		if !value[f] {
			t.Errorf("ValueFlags(close)[%q] = false, want true", f)
		}
	}
	boolFlags := BoolFlags("close")
	for _, f := range []string{"--claim-next", "--continue", "-f", "--force", "--no-auto", "--suggest-next"} {
		if !boolFlags[f] {
			t.Errorf("BoolFlags(close)[%q] = false, want true", f)
		}
	}
}

func TestListFlagSets(t *testing.T) {
	value := ValueFlags("list")
	for _, f := range []string{"--assignee", "--status", "--parent", "--label", "--priority", "--limit"} {
		if !value[f] {
			t.Errorf("ValueFlags(list)[%q] = false, want true", f)
		}
	}
	boolFlags := BoolFlags("list")
	for _, f := range []string{
		"--all", "--deferred", "--empty-description", "--flat", "--include-gates",
		"--include-infra", "--include-templates", "--long", "--no-assignee",
		"--no-labels", "--no-pager", "--no-parent", "--no-pinned", "--overdue",
		"--pinned", "--pretty", "--ready", "--reverse", "-r", "--skip-labels",
		"--tree", "--watch", "-w",
	} {
		if !boolFlags[f] {
			t.Errorf("BoolFlags(list)[%q] = false, want true", f)
		}
	}
}

func TestReadyFlagSets(t *testing.T) {
	boolFlags := BoolFlags("ready")
	for _, f := range []string{"--unassigned", "-u"} {
		if !boolFlags[f] {
			t.Errorf("BoolFlags(ready)[%q] = false, want true", f)
		}
	}
}

func TestCompoundSubcommandFlagSets(t *testing.T) {
	if ValueFlags("mol pour") == nil {
		t.Fatal("ValueFlags(\"mol pour\") = nil, want non-nil")
	}
	if !ValueFlags("mol pour")["--assignee"] {
		t.Error(`ValueFlags("mol pour")["--assignee"] = false, want true`)
	}
	if !ValueFlags("mol pour")["--var"] {
		t.Error(`ValueFlags("mol pour")["--var"] = false, want true`)
	}
	if !ValueFlags("mol pour")["--attach"] {
		t.Error(`ValueFlags("mol pour")["--attach"] = false, want true`)
	}
	if !BoolFlags("mol pour")["--dry-run"] {
		t.Error(`BoolFlags("mol pour")["--dry-run"] = false, want true`)
	}
	if ValueFlags("dep add") == nil {
		t.Fatal(`ValueFlags("dep add") = nil, want non-nil`)
	}
	if ValueFlags("gate check") == nil {
		t.Fatal(`ValueFlags("gate check") = nil, want non-nil`)
	}
	for _, flag := range []string{"--age", "--exclude-type"} {
		if !ValueFlags("mol wisp gc")[flag] {
			t.Errorf(`ValueFlags("mol wisp gc")[%q] = false, want true`, flag)
		}
	}
	for _, flag := range []string{"--all", "--closed", "--dry-run", "-f", "--force"} {
		if !BoolFlags("mol wisp gc")[flag] {
			t.Errorf(`BoolFlags("mol wisp gc")[%q] = false, want true`, flag)
		}
	}
}

func TestScanUnknownFlagsCleanInvocationsProduceNoFindings(t *testing.T) {
	cases := []string{
		`gc bd list --json --assignee="{{.AgentName}}" --status=in-progress`,
		`gc bd update <bead_id> --set-metadata work_dir=<absolute_worktree_path>`,
		"gc bd update <id> --claim",
		"gc bd show <id> --json",
		"`gc bd ready --unassigned`",
		"`gc bd update <id> --claim`",
		"`gc bd close <id>`",
		"gc bd reopen <id>",
		"`gc bd close <bead-id> --reason \"Hyperscale demo: task completed\"`",
		"gc bd ready --label=pool:worker --unassigned --limit=1 --json",
		`gc bd create "..." -t task`,
		"gc bd dep add <tests-id> <auth-id>   # tests need auth first",
		"gc bd mol wisp gc --age 24h --dry-run",
		"bd mol wisp gc --closed --force --exclude-type agent,rig",
		"`gc bd list --status=open`",
		"`gc bd list --status=in_progress`",
		"`gc bd ready --unassigned`",
		`gc mail send --all "New tasks filed - check gc bd ready --unassigned"`,
	}
	for _, line := range cases {
		findings := ScanUnknownFlags([]byte(line))
		if len(findings) != 0 {
			t.Errorf("ScanUnknownFlags(%q) = %v, want no findings", line, findings)
		}
	}
}

func TestScanUnknownFlagsOutOfScopeSubcommandIsSkipped(t *testing.T) {
	findings := ScanUnknownFlags([]byte("gc bd formula show <formula-name> --json"))
	if len(findings) != 0 {
		t.Fatalf("ScanUnknownFlags(formula show) = %v, want no findings (out of scope, silently skipped)", findings)
	}
}

func TestScanUnknownFlagsDetectsTypo(t *testing.T) {
	findings := ScanUnknownFlags([]byte("gc bd update <id> --asignee bob"))
	if len(findings) != 1 {
		t.Fatalf("ScanUnknownFlags() = %v, want exactly 1 finding", findings)
	}
	f := findings[0]
	if f.Flag != "--asignee" {
		t.Errorf("Flag = %q, want %q", f.Flag, "--asignee")
	}
	if f.Subcommand != "update" {
		t.Errorf("Subcommand = %q, want %q", f.Subcommand, "update")
	}
	if f.Line != 1 {
		t.Errorf("Line = %d, want 1", f.Line)
	}
}

func TestScanUnknownFlagsDetectsTypoInCompoundSubcommand(t *testing.T) {
	findings := ScanUnknownFlags([]byte("gc bd mol pour mol-tdd-build --asignee builder"))
	if len(findings) != 1 {
		t.Fatalf("ScanUnknownFlags() = %v, want exactly 1 finding", findings)
	}
	if findings[0].Subcommand != "mol pour" {
		t.Errorf("Subcommand = %q, want %q", findings[0].Subcommand, "mol pour")
	}
	if findings[0].Flag != "--asignee" {
		t.Errorf("Flag = %q, want %q", findings[0].Flag, "--asignee")
	}
}

func TestScanUnknownFlagsUsesLongestNestedSubcommand(t *testing.T) {
	findings := ScanUnknownFlags([]byte("gc bd mol wisp gc --age 24h --dry-rnu"))
	if len(findings) != 1 {
		t.Fatalf("ScanUnknownFlags() = %v, want exactly 1 finding", findings)
	}
	if findings[0].Subcommand != "mol wisp gc" {
		t.Errorf("Subcommand = %q, want %q", findings[0].Subcommand, "mol wisp gc")
	}
	if findings[0].Flag != "--dry-rnu" {
		t.Errorf("Flag = %q, want %q", findings[0].Flag, "--dry-rnu")
	}
}

func TestScanUnknownFlagsReportsCorrectLineNumbers(t *testing.T) {
	source := "line one is fine\ngc bd update <id> --asignee bob\nline three is fine too"
	findings := ScanUnknownFlags([]byte(source))
	if len(findings) != 1 {
		t.Fatalf("ScanUnknownFlags() = %v, want exactly 1 finding", findings)
	}
	if findings[0].Line != 2 {
		t.Errorf("Line = %d, want 2", findings[0].Line)
	}
}

func TestScanUnknownFlagsDoubleDashTerminatesFlagScanning(t *testing.T) {
	findings := ScanUnknownFlags([]byte("gc bd update <id> --claim -- --asignee"))
	if len(findings) != 0 {
		t.Fatalf("ScanUnknownFlags() = %v, want no findings (positional after --)", findings)
	}
}

func TestScanUnknownFlagsBareBdWithoutGcPrefix(t *testing.T) {
	findings := ScanUnknownFlags([]byte("bd update <id> --asignee bob"))
	if len(findings) != 1 {
		t.Fatalf("ScanUnknownFlags() = %v, want exactly 1 finding", findings)
	}
}

func TestScanUnknownFlagsAcceptsGCScopeFlagsOnGCBD(t *testing.T) {
	// --city and --rig are gc-owned: cmd/gc/cmd_bd.go extractBdScopeFlags
	// strips them before forwarding the remaining args to bd, and gc's own
	// help documents "gc bd list --rig my-project -s open".
	cases := []string{
		"gc bd list --rig my-project --status open --json",
		"gc bd list --rig={{ .Rig }} --status=open --json",
		"gc bd --rig my-project create \"New task\"",
		"gc bd create --city /path/to/city --type task \"t\"",
		"gc bd --city=/path/to/city list --json",
	}
	for _, line := range cases {
		if findings := ScanUnknownFlags([]byte(line)); len(findings) != 0 {
			t.Errorf("ScanUnknownFlags(%q) = %v, want no findings", line, findings)
		}
	}
}

func TestScanUnknownFlagsRejectsGCScopeFlagsOnBareBD(t *testing.T) {
	// Bare bd has no --rig: "bd list --rig gascity" exits with
	// "Error: unknown flag: --rig". Only the "gc bd" form accepts it.
	findings := ScanUnknownFlags([]byte("bd list --rig my-project --json"))
	if len(findings) != 1 {
		t.Fatalf("ScanUnknownFlags() = %v, want exactly 1 finding", findings)
	}
	if findings[0].Flag != "--rig" {
		t.Errorf("Flag = %q, want %q", findings[0].Flag, "--rig")
	}
}

func TestScanUnknownFlagsDetectsTypoAfterLeadingScopeFlags(t *testing.T) {
	// "gc bd --rig <name> <sub> ..." and the --city form place the gc-owned
	// scope flags before the subcommand. The scanner must consume the leading
	// scope flags and still validate the resolved subcommand, so a typo after
	// the verb is reported instead of the whole invocation being skipped.
	cases := []struct {
		line string
		flag string
		sub  string
	}{
		{"gc bd --rig my-project create --asignee bob", "--asignee", "create"},
		{"gc bd --rig=my-project create --asignee bob", "--asignee", "create"},
		{"gc bd --city /path/to/city update <id> --asignee bob", "--asignee", "update"},
		{"gc bd --city=/path/to/city update <id> --asignee bob", "--asignee", "update"},
		{"gc bd --rig my-project mol pour <id> --asignee builder", "--asignee", "mol pour"},
	}
	for _, tc := range cases {
		findings := ScanUnknownFlags([]byte(tc.line))
		if len(findings) != 1 {
			t.Errorf("ScanUnknownFlags(%q) = %v, want exactly 1 finding", tc.line, findings)
			continue
		}
		if findings[0].Flag != tc.flag {
			t.Errorf("ScanUnknownFlags(%q) Flag = %q, want %q", tc.line, findings[0].Flag, tc.flag)
		}
		if findings[0].Subcommand != tc.sub {
			t.Errorf("ScanUnknownFlags(%q) Subcommand = %q, want %q", tc.line, findings[0].Subcommand, tc.sub)
		}
	}
}

func TestScanUnknownFlagsAcceptsCleanLeadingScopeFlags(t *testing.T) {
	// The leading scope-flag forms with no typo must still produce no
	// findings after the scanner starts validating the subcommand that
	// follows the consumed scope flags.
	cases := []string{
		"gc bd --rig my-project create --json",
		"gc bd --city /path/to/city update <id> --claim",
		"gc bd --rig=my-project list --status open --json",
	}
	for _, line := range cases {
		if findings := ScanUnknownFlags([]byte(line)); len(findings) != 0 {
			t.Errorf("ScanUnknownFlags(%q) = %v, want no findings", line, findings)
		}
	}
}

func TestScanUnknownFlagsStopsAtShellSeparators(t *testing.T) {
	// Flags after a pipe belong to the downstream command, not to bd.
	cases := []string{
		"gc bd show <id> --json | jq -r '.metadata.reason'",
		"gc bd list --json | head -5",
		"gc bd ready --json && echo -n done",
		"gc bd show <id> --json ; grep -c foo",
	}
	for _, line := range cases {
		if findings := ScanUnknownFlags([]byte(line)); len(findings) != 0 {
			t.Errorf("ScanUnknownFlags(%q) = %v, want no findings", line, findings)
		}
	}
}

func TestScanUnknownFlagsAngleBracketPlaceholdersDoNotSplit(t *testing.T) {
	// "<id>" is a placeholder in prompt templates, not a redirection, so it
	// must not end the invocation and hide a typo that follows it.
	findings := ScanUnknownFlags([]byte("gc bd update <id> --asignee bob"))
	if len(findings) != 1 {
		t.Fatalf("ScanUnknownFlags() = %v, want exactly 1 finding", findings)
	}
	if findings[0].Flag != "--asignee" {
		t.Errorf("Flag = %q, want %q", findings[0].Flag, "--asignee")
	}
}

func TestScanUnknownFlagsQuotedSeparatorDoesNotSplit(t *testing.T) {
	// A pipe inside a quoted argument is data, not a shell separator, so the
	// bd invocation continues and a real typo after it is still reported.
	findings := ScanUnknownFlags([]byte(`gc bd list --json --title "a | b" --asignee bob`))
	if len(findings) != 1 {
		t.Fatalf("ScanUnknownFlags() = %v, want exactly 1 finding", findings)
	}
	if findings[0].Flag != "--asignee" {
		t.Errorf("Flag = %q, want %q", findings[0].Flag, "--asignee")
	}
}

func TestScanUnknownFlagsMarkdownTableCellIsolatesInvocations(t *testing.T) {
	// Prompt templates document commands in markdown tables; the cell pipes
	// must not carry a neighboring cell's flags into the bd invocation.
	line := "| Show bead | `gc bd show <id> --json` | pipe to `jq -r .id` |"
	if findings := ScanUnknownFlags([]byte(line)); len(findings) != 0 {
		t.Errorf("ScanUnknownFlags(%q) = %v, want no findings", line, findings)
	}
}

// TestUpdateCompareAndSetFlagsArePinned fails on the unfixed build and passes on
// the fixed one with nothing else changed. It deliberately does NOT rely on help
// discovery: discovery ignores its own errors, so a build where `bd` cannot be
// run falls back to the pinned table, and the pinned table is what broke fenced
// dispatch. gc-sling works around the gap by writing --if-assignee=VALUE, which
// the argv scanner skips entirely; this is the fix that workaround points at.
func TestUpdateCompareAndSetFlagsArePinned(t *testing.T) {
	for _, flag := range []string{"--if-assignee", "--if-status"} {
		if !updateValueFlags()[flag] {
			t.Errorf("update value flags missing %s", flag)
		}
	}
}

func updateValueFlags() map[string]bool {
	return valueFlagsBySub["update"]
}
