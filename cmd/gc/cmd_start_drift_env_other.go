//go:build !windows

package main

func driftRestartSupervisorEnv(parent []string) []string {
	return withoutSessionIdentityEnv(parent)
}
