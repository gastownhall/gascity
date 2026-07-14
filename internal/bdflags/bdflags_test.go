package bdflags

import (
	"reflect"
	"sort"
	"testing"
)

func TestSubcommandsListsAllKnownKeys(t *testing.T) {
	want := []string{
		"close", "create", "delete", "dep add", "dep list", "dep remove",
		"gate check", "gate list", "list", "mol burn", "mol current",
		"mol pour", "mol wisp", "ready", "reopen", "show", "update",
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
