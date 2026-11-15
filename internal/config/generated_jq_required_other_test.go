//go:build !windows

package config

import (
	"os/exec"
	"testing"
)

func requireGeneratedCorpusJQ(t *testing.T, message string) {
	t.Helper()
	if _, err := exec.LookPath("jq"); err != nil {
		t.Skip(message)
	}
}
