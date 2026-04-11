package acceptancehelpers

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// StageProviderBinary materializes a provider executable in binDir.
// If GC_ACCEPTANCE_PROVIDER_SHIM_<NAME> is set, that shell prefix is used as
// a wrapper command. An explicitly empty env var disables any default shim.
func StageProviderBinary(binDir, name, defaultShim string) error {
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		return err
	}

	dst := filepath.Join(binDir, name)
	_ = os.Remove(dst)

	if shim, ok := providerShimCommand(name, defaultShim); ok {
		script := fmt.Sprintf("#!/bin/sh\nexec %s \"$@\"\n", shim)
		return os.WriteFile(dst, []byte(script), 0o755)
	}

	path, err := exec.LookPath(name)
	if err != nil {
		return err
	}
	return os.Symlink(path, dst)
}

func providerShimCommand(name, defaultShim string) (string, bool) {
	key := "GC_ACCEPTANCE_PROVIDER_SHIM_" + strings.ToUpper(strings.NewReplacer("-", "_", "/", "_", ".", "_").Replace(name))
	if value, ok := os.LookupEnv(key); ok {
		shim := strings.TrimSpace(value)
		if shim == "" {
			return "", false
		}
		return shim, true
	}
	shim := strings.TrimSpace(defaultShim)
	if shim == "" {
		return "", false
	}
	return shim, true
}
