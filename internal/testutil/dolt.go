package testutil

import (
	"fmt"
	"os"
	"path/filepath"
)

// DoltGlobalConfig is the dolt global config a test seeds under an isolated
// DOLT_ROOT_PATH. It carries the author identity gc's init preflight and
// `dolt commit` require, and turns dolt's usage metrics off so no dolt a test
// runs calls eventsapi.dolthub.com (an undeclared network input).
const DoltGlobalConfig = `{"user.name":"gc-test","user.email":"gc-test@test.local","metrics.disabled":"true"}`

// SeedDoltGlobalConfig writes DoltGlobalConfig to root/.dolt/config_global.json,
// where dolt reads its global config when DOLT_ROOT_PATH (or, without it, HOME)
// is root.
func SeedDoltGlobalConfig(root string) error {
	dir := filepath.Join(root, ".dolt")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("creating dolt config dir %s: %w", dir, err)
	}
	path := filepath.Join(dir, "config_global.json")
	if err := os.WriteFile(path, []byte(DoltGlobalConfig), 0o644); err != nil {
		return fmt.Errorf("seeding dolt global config %s: %w", path, err)
	}
	return nil
}
