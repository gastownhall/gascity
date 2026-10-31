//go:build !windows

package herdr

func filterAgentInfos(agents []agentInfo) []agentInfo {
	return agents
}
