package core

import (
	"bytes"
	"io/fs"
	"reflect"
	"regexp"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/BurntSushi/toml"
)

var (
	bareBDSubcommand       = regexp.MustCompile(`\bbd[[:space:]\\]+(?:blocked|children|close|comment|comments|completion|config|count|create|delete|dep|doctor|dolt|epic|export|formula|gate|graph|help|hook|hooks|import|info|init|label|list|migrate|mol|orphans|prime|prune|ready|remember|rename-prefix|reopen|restore|search|show|sql|stale|stats|status|sync|update|version|where|worktree)\b`)
	bareBDDynamicArg       = regexp.MustCompile(`\bbd[[:space:]\\]+["']\$(?:\{)?[A-Za-z_]`)
	bareBDLeadingFlag      = regexp.MustCompile(`\bbd[[:space:]\\]+--[A-Za-z0-9]`)
	bareBDCommand          = regexp.MustCompile(`\bcommand[[:space:]]+bd\b`)
	bareBDSerializedArgv   = regexp.MustCompile(`["']bd["'][[:space:]]*,`)
	bareBDSerializedScalar = regexp.MustCompile(`\b(?:command|cmd|executable)[ \t]*[:=][ \t]*["']?bd["']?(?:[ \t]|$)`)
	bareBDYAMLArgv         = regexp.MustCompile(`(?m)^[ \t]*-[ \t]+bd[ \t]*(?:\r?\n)[ \t]*-[ \t]+[A-Za-z]`)
	gcImmediatelyBefore    = regexp.MustCompile(`(?:^|[^A-Za-z0-9_-])gc(?:[ \t]+--(?:city|rig)(?:=[^ \t\r\n]+|[ \t]+(?:"[^"\r\n]*"|'[^'\r\n]*'|[^ \t\r\n]+)))*(?:[ \t]|\\\r?\n)+$`)
	gcSerializedBefore     = regexp.MustCompile(`["']gc["'][[:space:]]*,(?:(?:[[:space:]]*["']--(?:city|rig)=[^"']+["'][[:space:]]*,)|(?:[[:space:]]*["']--(?:city|rig)["'][[:space:]]*,[[:space:]]*["'][^"']+["'][[:space:]]*,))*[[:space:]]*$`)
)

func findBareBDCommands(data []byte) []int {
	body := string(data)
	offsets := make(map[int]struct{})

	for _, pattern := range []*regexp.Regexp{bareBDSubcommand, bareBDDynamicArg, bareBDLeadingFlag} {
		for _, match := range pattern.FindAllStringIndex(body, -1) {
			if gcImmediatelyBefore.MatchString(body[:match[0]]) {
				continue
			}
			offsets[match[0]] = struct{}{}
		}
	}
	for _, match := range bareBDSerializedArgv.FindAllStringIndex(body, -1) {
		if gcSerializedBefore.MatchString(body[:match[0]]) {
			continue
		}
		offsets[match[0]] = struct{}{}
	}
	for _, pattern := range []*regexp.Regexp{bareBDCommand, bareBDSerializedScalar, bareBDYAMLArgv} {
		for _, match := range pattern.FindAllStringIndex(body, -1) {
			offsets[match[0]] = struct{}{}
		}
	}

	result := make([]int, 0, len(offsets))
	for offset := range offsets {
		result = append(result, offset)
	}
	sort.Ints(result)
	return result
}

func TestCoreShippedAssetsRouteBDCommandsThroughGC(t *testing.T) {
	err := fs.WalkDir(PackFS, ".", func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			return nil
		}

		data, err := fs.ReadFile(PackFS, path)
		if err != nil {
			return err
		}
		for _, offset := range findBareBDCommands(data) {
			lineNumber := bytes.Count(data[:offset], []byte{'\n'}) + 1
			lineStart := bytes.LastIndexByte(data[:offset], '\n') + 1
			lineEnd := bytes.IndexByte(data[offset:], '\n')
			if lineEnd < 0 {
				lineEnd = len(data)
			} else {
				lineEnd += offset
			}
			t.Errorf("%s:%d: shipped bd commands must route through gc bd: %s", path, lineNumber, strings.TrimSpace(string(data[lineStart:lineEnd])))
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walking embedded core pack: %v", err)
	}
}

func TestFindBareBDCommands(t *testing.T) {
	tests := []struct {
		name string
		body string
		want int
	}{
		{name: "plain shell", body: `bd show ga-123`, want: 1},
		{name: "wrapped shell", body: "bd \\\n  show ga-123", want: 1},
		{name: "wrapped markdown", body: "`bd\nshow ga-123`", want: 1},
		{name: "serialized argv", body: "[\"bd\",\n \"future-command\", \"ga-123\"]", want: 1},
		{name: "dir-scoped command", body: `bd --dir /tmp/rig show ga-123`, want: 1},
		{name: "leading passthrough flag", body: `bd --no-daemon list`, want: 1},
		{name: "unknown leading passthrough flag", body: `bd --future-routing-bypass list`, want: 1},
		{name: "dynamic subcommand", body: `bd "$verb"`, want: 1},
		{name: "wrapped gc command", body: "gc bd \\\n  show ga-123"},
		{name: "plain gc command", body: `gc bd show ga-123`},
		{name: "explicit gc city", body: `gc --city /tmp/city bd show ga-123`},
		{name: "quoted explicit gc city", body: `gc --city "$CITY" bd list`},
		{name: "explicit gc rig", body: `gc --rig frontend bd list`},
		{name: "explicit gc city and rig", body: `gc --city /tmp/city --rig frontend bd list`},
		{name: "serialized gc argv", body: `["gc", "bd", "show", "ga-123"]`},
		{name: "serialized scoped gc argv", body: `["gc", "--city", "/x", "--rig", "r", "bd", "show"]`},
		{name: "binary prose", body: `the bd CLI reads a bd-managed store`},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := len(findBareBDCommands([]byte(tt.body))); got != tt.want {
				t.Fatalf("findBareBDCommands() found %d commands, want %d in %q", got, tt.want, tt.body)
			}
		})
	}
}

func TestCoreMaintenanceExecAssets(t *testing.T) {
	required := []string{
		"assets/scripts/_bd_trace.sh",
		"assets/scripts/escalate.sh",
		"assets/scripts/jsonl-export.sh",
		"assets/scripts/order_outcome.sh",
		"assets/scripts/reaper.sh",
		"assets/scripts/scope_bd.sh",
		"orders/jsonl-export.toml",
		"orders/reaper.toml",
	}
	for _, path := range required {
		if _, err := fs.Stat(PackFS, path); err != nil {
			t.Fatalf("core pack missing %s: %v", path, err)
		}
	}

	retired := []string{
		"formulas/mol-dog-jsonl.toml",
		"formulas/mol-dog-reaper.toml",
		"orders/mol-dog-jsonl.toml",
		"orders/mol-dog-reaper.toml",
		// Retired with the move to bd verbs: the maintenance orders no
		// longer resolve a Dolt port of their own.
		"assets/scripts/dolt-target.sh",
		// Retired because bd list hides the wisps plane, so the order never
		// selected a wisp; reaper and the controller wisp GC own that work.
		"orders/wisp-compact.toml",
		"assets/scripts/wisp-compact.sh",
	}
	for _, path := range retired {
		if _, err := fs.Stat(PackFS, path); err == nil {
			t.Fatalf("core pack must not carry retired maintenance asset %s", path)
		}
	}
}

func TestReaperScriptUsesUTCTimestampForAgeGates(t *testing.T) {
	data, err := fs.ReadFile(PackFS, "assets/scripts/reaper.sh")
	if err != nil {
		t.Fatalf("core pack missing reaper.sh: %v", err)
	}
	commentLine := regexp.MustCompile(`^[ \t]*#`)
	localNow := regexp.MustCompile(`\bNOW[ \t]*\(`)
	for i, line := range strings.Split(string(data), "\n") {
		if commentLine.MatchString(line) {
			continue
		}
		if localNow.MatchString(line) {
			t.Errorf("reaper.sh:%d compares against server-local NOW() but bead timestamps are UTC; use UTC_TIMESTAMP(): %s", i+1, strings.TrimSpace(line))
		}
	}
}

func TestJSONLExportMaintainsArchiveObjectDatabase(t *testing.T) {
	data, err := fs.ReadFile(PackFS, "assets/scripts/jsonl-export.sh")
	if err != nil {
		t.Fatalf("ReadFile(jsonl-export.sh): %v", err)
	}
	// A comment that names a setting or a call does not keep the contract.
	commentLine := regexp.MustCompile(`^[ \t]*#`)
	var code []string
	for _, line := range strings.Split(string(data), "\n") {
		if !commentLine.MatchString(line) {
			code = append(code, line)
		}
	}
	body := strings.Join(code, "\n")

	for _, want := range []string{
		`GC_JSONL_GC_LOOSE_OBJECTS`,
		`GC_JSONL_GC_LOOSE_KB`,
		`GC_JSONL_GC_WINDOW_MEMORY`,
		`GC_JSONL_GC_MIN_FREE_KB`,
		`maintain_archive_repository()`,
		`check_archive_gc_free_space`,
		`count-objects -v`,
		`pack.windowMemory=`,
		`pack.threads=1`,
		`--geometric=2`,
		`maintenance.auto=false`,
		`gc.auto=0`,
		`nice -n 19`,
		`ionice -t -c 3`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("jsonl-export.sh is missing archive-maintenance contract %q", want)
		}
	}
	calls := regexp.MustCompile(`\bmaintain_archive_repository\b(?:[^(]|$)`)
	if got := len(calls.FindAllString(body, -1)); got != 1 {
		t.Fatalf("jsonl-export.sh invokes archive maintenance %d times, want one (a helper that every exit after the export calls)", got)
	}
	exits := regexp.MustCompile(`\bmaintain_archive_or_exit\b(?:[^(]|$)`)
	if got := len(exits.FindAllString(body, -1)); got != 5 {
		t.Fatalf("jsonl-export.sh runs archive maintenance at %d exits, want five (normal, HALT, pending push, failed DB, no changes)", got)
	}
}

// A maintenance failure that repeats is counted and escalated, and the repack
// is bounded so one hung pass cannot run the order into its own timeout.
func TestJSONLExportEscalatesRepeatedArchiveMaintenanceFailures(t *testing.T) {
	data, err := fs.ReadFile(PackFS, "assets/scripts/jsonl-export.sh")
	if err != nil {
		t.Fatalf("ReadFile(jsonl-export.sh): %v", err)
	}
	commentLine := regexp.MustCompile(`^[ \t]*#`)
	var code []string
	for _, line := range strings.Split(string(data), "\n") {
		if !commentLine.MatchString(line) {
			code = append(code, line)
		}
	}
	body := strings.Join(code, "\n")

	for _, want := range []string{
		`GC_JSONL_MAX_MAINTENANCE_FAILURES`,
		`GC_JSONL_GC_TIMEOUT_SECS`,
		`consecutive_maintenance_failures`,
		`maintenance_failure_escalated`,
		`last_maintenance_failure_at`,
		`last_maintenance_failure_cause`,
		`last_maintenance_failure_arm`,
		`last_maintenance_failure_evidence`,
		`ESCALATION: JSONL archive maintenance failing [HIGH]`,
		`troubleshooting.md#archive-object-maintenance`,
		`stats_read_failed`,
		`stats_malformed`,
		`stats_incomplete`,
		`low_space`,
		`gc_failed`,
		`gc_timeout`,
		`gtimeout`,
		`maintenance escalation was not delivered; the next failing run will retry`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("jsonl-export.sh is missing maintenance-escalation contract %q", want)
		}
	}

	// Every exit that runs maintenance names its arm.
	arms := map[string]int{}
	callSite := regexp.MustCompile(`\bmaintain_archive_or_exit\s+"\$SUMMARY"\s+(halt|post_push|no_change)\b`)
	for _, match := range callSite.FindAllStringSubmatch(body, -1) {
		arms[match[1]]++
	}
	if want := map[string]int{"halt": 1, "post_push": 1, "no_change": 3}; !reflect.DeepEqual(arms, want) {
		t.Errorf("maintenance call sites by arm = %v, want %v", arms, want)
	}

	// The repack's default bound plus the 300 s the run keeps for the rest of
	// its work fits inside the order's timeout.
	timeoutDefault := regexp.MustCompile(`GC_JSONL_GC_TIMEOUT_SECS:-(\d+)`).FindStringSubmatch(body)
	if timeoutDefault == nil {
		t.Fatalf("jsonl-export.sh does not default GC_JSONL_GC_TIMEOUT_SECS")
	}
	bound, err := time.ParseDuration(timeoutDefault[1] + "s")
	if err != nil {
		t.Fatalf("default GC_JSONL_GC_TIMEOUT_SECS %q: %v", timeoutDefault[1], err)
	}
	orderData, err := fs.ReadFile(PackFS, "orders/jsonl-export.toml")
	if err != nil {
		t.Fatalf("ReadFile(jsonl-export.toml): %v", err)
	}
	var order struct {
		Order struct {
			Timeout string `toml:"timeout"`
		} `toml:"order"`
	}
	if _, err := toml.Decode(string(orderData), &order); err != nil {
		t.Fatalf("decoding jsonl-export.toml: %v", err)
	}
	orderTimeout, err := time.ParseDuration(order.Order.Timeout)
	if err != nil {
		t.Fatalf("order timeout %q: %v", order.Order.Timeout, err)
	}
	if bound+300*time.Second > orderTimeout {
		t.Errorf("default repack bound %s plus 300s exceeds the order timeout %s", bound, orderTimeout)
	}
}

func TestCoreControlDispatcherAgent(t *testing.T) {
	type agentFile struct {
		Description       string   `toml:"description"`
		StartCommand      string   `toml:"start_command"`
		PromptMode        string   `toml:"prompt_mode"`
		ProcessNames      []string `toml:"process_names"`
		MaxActiveSessions *int     `toml:"max_active_sessions"`
		Scope             string   `toml:"scope"`
	}

	data, err := fs.ReadFile(PackFS, "agents/control-dispatcher/agent.toml")
	if err != nil {
		t.Fatalf("core pack missing control-dispatcher agent: %v", err)
	}
	var agent agentFile
	if _, err := toml.Decode(string(data), &agent); err != nil {
		t.Fatalf("Decode(control-dispatcher agent.toml): %v", err)
	}
	if agent.Description == "" {
		t.Fatal("control-dispatcher description is empty")
	}
	if agent.Scope != "" {
		t.Fatalf("control-dispatcher scope = %q, want empty so it expands at city and rig scope", agent.Scope)
	}
	wantStartCommand := `sh -c 'export GC_WORKFLOW_TRACE="${GC_WORKFLOW_TRACE:-${GC_CONTROL_DISPATCHER_TRACE_DEFAULT:-${GC_CITY}/.gc/runtime/control-dispatcher-trace.log}}"; trace_dir="${GC_WORKFLOW_TRACE%/*}"; if [ "$trace_dir" = "$GC_WORKFLOW_TRACE" ]; then trace_dir="."; elif [ -z "$trace_dir" ]; then trace_dir="/"; fi; mkdir -p "$trace_dir"; exec "${GC_BIN:-gc}" convoy control --serve --follow {{.Agent}}'`
	if agent.StartCommand != wantStartCommand {
		t.Fatalf("control-dispatcher start_command = %q, want templated dispatcher command", agent.StartCommand)
	}
	if agent.PromptMode != "none" {
		t.Fatalf("control-dispatcher prompt_mode = %q, want none", agent.PromptMode)
	}
	if !reflect.DeepEqual(agent.ProcessNames, []string{"gc"}) {
		t.Fatalf("control-dispatcher process_names = %v, want [gc]", agent.ProcessNames)
	}
	if agent.MaxActiveSessions == nil || *agent.MaxActiveSessions != 1 {
		t.Fatalf("control-dispatcher max_active_sessions = %v, want 1", agent.MaxActiveSessions)
	}
}

func TestCoreMaintenanceOrdersCarryLegacySkipAliases(t *testing.T) {
	type orderFile struct {
		Order struct {
			SkipAliases []string `toml:"skip_aliases"`
		} `toml:"order"`
	}

	for _, tt := range []struct {
		path string
		want string
	}{
		{path: "orders/jsonl-export.toml", want: "mol-dog-jsonl"},
		{path: "orders/reaper.toml", want: "mol-dog-reaper"},
	} {
		data, err := fs.ReadFile(PackFS, tt.path)
		if err != nil {
			t.Fatalf("ReadFile(%s): %v", tt.path, err)
		}
		var parsed orderFile
		if _, err := toml.Decode(string(data), &parsed); err != nil {
			t.Fatalf("Decode(%s): %v", tt.path, err)
		}
		if len(parsed.Order.SkipAliases) != 1 || parsed.Order.SkipAliases[0] != tt.want {
			t.Fatalf("%s skip_aliases = %#v, want [%q]", tt.path, parsed.Order.SkipAliases, tt.want)
		}
	}
}

// TestClaimProtocolFragmentIsEmbedded pins the go:embed inclusion of
// template-fragments/. The fragment render tests in cmd/gc read the on-disk
// source tree, but production cities hydrate the core pack from PackFS —
// dropping all:template-fragments from the embed directive would ship
// pool-worker prompts whose {{ template "claim-protocol" . }} degrades to an
// unexpanded literal while every repo test stayed green.
func TestClaimProtocolFragmentIsEmbedded(t *testing.T) {
	data, err := fs.ReadFile(PackFS, "template-fragments/claim-protocol.template.md")
	if err != nil {
		t.Fatalf("core PackFS is missing template-fragments/claim-protocol.template.md: %v", err)
	}
	for _, want := range []string{`{{ define "claim-protocol" -}}`, "gc hook --claim --drain-ack --json"} {
		if !strings.Contains(string(data), want) {
			t.Errorf("embedded claim-protocol fragment missing %q", want)
		}
	}
}

// coreWorkerPrompts are the core pack prompts that tell a blocked worker how
// to escalate.
var coreWorkerPrompts = []string{
	"assets/prompts/graph-worker.md",
	"assets/prompts/pool-worker.template.md",
}

// promptSection returns the body of the "## <heading>" section of a Markdown
// prompt, up to the next level-two heading.
func promptSection(t *testing.T, body, heading string) string {
	t.Helper()
	marker := "\n## " + heading + "\n"
	start := strings.Index(body, marker)
	if start < 0 {
		t.Fatalf("prompt has no %q section", "## "+heading)
	}
	section := body[start+len(marker):]
	if end := strings.Index(section, "\n## "); end >= 0 {
		section = section[:end+1]
	}
	return section
}

// singleBashBlock returns the contents of the only fenced bash block in a
// prompt section.
func singleBashBlock(t *testing.T, section string) string {
	t.Helper()
	const open, closing = "```bash\n", "\n```"
	if n := strings.Count(section, open); n != 1 {
		t.Fatalf("section has %d fenced bash blocks, want exactly 1:\n%s", n, section)
	}
	start := strings.Index(section, open) + len(open)
	end := strings.Index(section[start:], closing)
	if end < 0 {
		t.Fatalf("section's bash block is not closed:\n%s", section)
	}
	return section[start:start+end] + "\n"
}
