package runtime

import (
	"path/filepath"
	"strings"
)

// TransportForRuntimeName reports the transport bundled with a runtime
// selection. Runtime and transport are not fully composable yet, so the
// selection fixes the transport: ACP uses "acp", the native and legacy T3
// selections use "t3", and the remaining runtimes use the tmux carrier.
func TransportForRuntimeName(name string) string {
	name = strings.TrimSpace(name)
	switch {
	case name == "acp":
		return "acp"
	case name == "t3bridge":
		return "t3"
	case strings.HasPrefix(name, "exec:") &&
		filepath.Base(strings.TrimSpace(strings.TrimPrefix(name, "exec:"))) == "gc-session-t3":
		return "t3"
	default:
		return "tmux"
	}
}
