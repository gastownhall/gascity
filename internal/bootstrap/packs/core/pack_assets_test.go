package core

import (
	"io/fs"
	"reflect"
	"testing"

	"github.com/BurntSushi/toml"
)

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
