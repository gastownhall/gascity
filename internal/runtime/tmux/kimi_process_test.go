package tmux

import "testing"

func TestKimiCodeProcessLiveness(t *testing.T) {
	// Kimi Code changes both COMM and argv[0] to kimi-code. The production
	// launcher still invokes kimi beneath sudo and bubblewrap.
	for _, command := range []string{"kimi", "kimi-code"} {
		snapshot := newProcessSnapshot([]processRuntimeState{
			{PID: "100", PPID: "1", Command: "sudo", Args: "sudo bwrap"},
			{PID: "101", PPID: "100", Command: "bwrap", Args: "bwrap"},
			{PID: "102", PPID: "101", Command: command, Args: command},
		})
		pane := paneRuntimeState{Command: "sudo", PID: "100"}
		if !pane.processAlive(processNameSet([]string{"kimi"}), snapshot) {
			t.Errorf("live %s descendant reported dead", command)
		}
		if !processMatchesNamesForTest(command, command, []string{"kimi"}) {
			t.Errorf("uncached matcher missed %s", command)
		}
	}
	for _, command := range []string{"sleep", "kimi-code-helper", "not-kimi-code"} {
		if processMatchesNamesForTest(command, command+" kimi-code", []string{"kimi"}) {
			t.Errorf("unrelated process %s matched", command)
		}
	}
	if processMatchesNamesForTest("kimi-code", "kimi-code", []string{"claude"}) {
		t.Fatal("Kimi matched another provider")
	}
	if processMatchesNamesForTest("sudo", "sudo kimi-code", []string{"kimi"}) {
		t.Fatal("dead Kimi's wrapper alone matched")
	}
}

func processMatchesNamesForTest(command, args string, names []string) bool {
	return processMatchesNameSet(command, args, processNameSet(names))
}
