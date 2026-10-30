//go:build windows

package herdr

import "strings"

func filterAgentInfos(agents []agentInfo) []agentInfo {
	out := agents[:0]
	for _, a := range agents {
		if strings.TrimSpace(a.Name) == "" {
			continue
		}
		out = append(out, a)
	}
	return out
}
