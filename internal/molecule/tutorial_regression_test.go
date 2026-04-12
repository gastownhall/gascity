package molecule

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
)

func TestCookTutorialConditionUsesDefaultVars(t *testing.T) {
	dir := t.TempDir()
	const formulaBody = `
formula = "deploy-flow"

[vars]
env = "dev"

[[steps]]
id = "build"
title = "Build"

[[steps]]
id = "deploy"
title = "Deploy to staging"
condition = "{{env}} == staging"
`
	if err := os.WriteFile(filepath.Join(dir, "deploy-flow.formula.toml"), []byte(formulaBody), 0o644); err != nil {
		t.Fatalf("write formula: %v", err)
	}

	store := beads.NewMemStore()
	result, err := Cook(context.Background(), store, "deploy-flow", []string{dir}, Options{})
	if err != nil {
		t.Fatalf("Cook: %v", err)
	}

	if result.Created != 2 {
		t.Fatalf("Cook should create only root + build when default env=dev filters deploy; created=%d mapping=%v", result.Created, result.IDMapping)
	}
}
