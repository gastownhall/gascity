package core

import (
	"bytes"
	"io/fs"
	"reflect"
	"regexp"
	"sort"
	"strings"
	"testing"

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

	// bdSchemaSkewProbeImmediatelyBefore recognizes the SessionStart bd-doctor
	// schema-skew probe emitted by internal/hooks.bdDoctorProbeShell: a
	// bounded `bd doctor` health check that must run before gc itself is
	// trusted to be reachable, so it cannot be routed through `gc bd`
	// without defeating its purpose (ga-ua1h7d / architect decision
	// ga-2gs3pl-A). cmd/gc/doctor_bd_schema_skew.go's advisory check calls
	// bd the same direct way for the same reason. Anchored on the probe's
	// unique $_gcbd_skew capture variable rather than "bd doctor" alone, so
	// this exemption cannot be widened by accident into a general doctor
	// bypass — a bare `bd doctor` without this exact preamble still fails.
	bdSchemaSkewProbeImmediatelyBefore = regexp.MustCompile(`\b_gcbd_skew=\$\((?:timeout 10 |gtimeout 10 )?$`)
)

func findBareBDCommands(data []byte) []int {
	body := string(data)
	offsets := make(map[int]struct{})

	for _, pattern := range []*regexp.Regexp{bareBDSubcommand, bareBDDynamicArg, bareBDLeadingFlag} {
		for _, match := range pattern.FindAllStringIndex(body, -1) {
			if gcImmediatelyBefore.MatchString(body[:match[0]]) {
				continue
			}
			if bdSchemaSkewProbeImmediatelyBefore.MatchString(body[:match[0]]) {
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
		{name: "bd-doctor skew probe, timeout-bounded", body: `_gcbd_skew=$(timeout 10 bd doctor 2>&1)`},
		{name: "bd-doctor skew probe, gtimeout-bounded", body: `_gcbd_skew=$(gtimeout 10 bd doctor 2>&1)`},
		{name: "bd-doctor skew probe, unbounded fallback", body: `_gcbd_skew=$(bd doctor 2>&1)`},
		{name: "bd doctor without the skew-probe preamble is still flagged", body: `bd doctor`, want: 1},
		{name: "skew-probe variable name alone does not exempt an unrelated bd call", body: `_gcbd_skew=1; bd doctor`, want: 1},
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
		"assets/scripts/dolt-target.sh",
		"assets/scripts/escalate.sh",
		"assets/scripts/jsonl-export.sh",
		"assets/scripts/reaper.sh",
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
	}
	for _, path := range retired {
		if _, err := fs.Stat(PackFS, path); err == nil {
			t.Fatalf("core pack must not carry retired Dog maintenance asset %s", path)
		}
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
