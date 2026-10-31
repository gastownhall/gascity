package main

import (
	"context"
	"fmt"
	"path/filepath"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/beads/contract"
	"github.com/gastownhall/gascity/internal/beads/proxyendpoint"
	"github.com/gastownhall/gascity/internal/fsys"
	"github.com/steveyegge/beads/issueops"
)

// readRigAdoptStorePrefix asks the existing workspace for its namespace, never
// derives it from the database name or projects the destination city's binding
// onto a queue that has not been adopted. In preview even provider startup is
// forbidden. Normal adoption uses bd's config capability and owns its startup.
func readRigAdoptStorePrefix(cityPath, rigPath string, readOnly bool) (string, error) {
	metadataPath := filepath.Join(rigPath, ".beads", "metadata.json")
	mode, ok, err := contract.ReadDoltMode(fsys.OSFS{}, metadataPath)
	if err != nil {
		return "", err
	}
	if !ok {
		return "", fmt.Errorf("workspace metadata does not identify a Dolt mode; cannot verify its issue_prefix")
	}
	if err := validateProviderOwnedProxiedScopeStore(cityPath, rigPath); err != nil {
		return "", err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if readOnly && mode == "proxied-server" {
		return proxyendpoint.ReadIssuePrefix(ctx, rigPath)
	}
	run := beads.ExecCommandRunnerWithEnvContextWithoutAmbientBeads(ctx, nil)
	if readOnly {
		normalRun := run
		run = func(dir, name string, args ...string) ([]byte, error) {
			return normalRun(dir, name, append([]string{"--readonly"}, args...)...)
		}
	}
	return beads.NewBdStore(rigPath, run).ConfigGet(issueops.SettingKeyIssuePrefix)
}
