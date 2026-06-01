package proctable

import "strings"

func darwinPSCommand(fields []string) string {
	if len(fields) < 3 {
		return ""
	}
	return fields[2]
}

func isInfrastructureCommand(command string) bool {
	return strings.Contains(strings.ToLower(command), "tmux")
}
