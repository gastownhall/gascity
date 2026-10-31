package main

import (
	"context"
	"fmt"
	"io"
	"maps"
	"os"
	"path/filepath"
	"strings"

	"github.com/gastownhall/gascity/internal/agent"
	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/runtime"
	workdirutil "github.com/gastownhall/gascity/internal/workdir"
	"github.com/gastownhall/gascity/internal/worktree"
)

const (
	hookWorktreeCreator    = "gc-hook-claim"
	hookWorktreeGeneration = "1"
)

type hookClaimWorktreeProvisioner struct {
	cityPath      string
	cityName      string
	cfg           *config.City
	repoDir       string
	root          string
	rigName       string
	rigComponent  string
	base          string
	defaultRef    string
	owner         string
	runtimeConfig runtime.Config
}

func newHookClaimWorktreeProvisioner(cityPath, cityName string, cfg *config.City, cfgAgent *config.Agent, qualifiedName string, resolved *config.ResolvedProvider) (hookProvisionWorktreeFunc, error) {
	if cfg == nil || cfgAgent == nil {
		return nil, fmt.Errorf("--worktree requires a resolved rig agent")
	}
	rigName := configuredRigName(cityPath, cfgAgent, cfg.Rigs)
	if rigName == "" {
		return nil, fmt.Errorf("--worktree requires a rig-scoped agent with a configured rig")
	}
	rigRoot := rigRootForName(rigName, cfg.Rigs)
	if strings.TrimSpace(rigRoot) == "" {
		return nil, fmt.Errorf("--worktree cannot resolve root for rig %q", rigName)
	}
	base := strings.TrimSpace(workdirutil.DefaultBranchForRigName(rigName, cfg.Rigs))
	if base == "" {
		return nil, fmt.Errorf("--worktree requires rig %q to set default_branch", rigName)
	}
	rigComponent := agent.SanitizeQualifiedNameForSession(rigName)
	if rigComponent == "" {
		return nil, fmt.Errorf("--worktree cannot derive a branch component for rig %q", rigName)
	}
	owner := agent.SanitizeQualifiedNameForSession(qualifiedName)
	if owner == "" {
		owner = rigComponent
	}
	resolvedRoot := filepath.Clean(workdirutil.WorktreesRoot(cityPath))
	cityWorktreesRoot := filepath.Clean(filepath.Join(cityPath, ".gc", "worktrees"))
	if !samePath(resolvedRoot, cityWorktreesRoot) {
		return nil, fmt.Errorf("--worktree requires worktrees under %q; resolved root %q is outside the city", cityWorktreesRoot, resolvedRoot)
	}
	installHooks := config.ResolveInstallHooks(cfgAgent, &cfg.Workspace)
	overlayDir := resolveOverlayDir(cfgAgent.OverlayDir, cityPath)
	providerOverlayName := ""
	if resolved != nil {
		providerOverlayName = strings.TrimSpace(resolved.Name)
	}
	runtimeConfig := runtime.Config{
		ProviderName:        resolvedProviderLaunchFamily(resolved),
		ProviderOverlayName: providerOverlayName,
		InstallAgentHooks:   installHooks,
		PackOverlayDirs:     effectiveOverlayDirs(cfg.PackOverlayDirs, cfg.RigOverlayDirs, rigName),
		OverlayDir:          overlayDir,
	}
	provisioner := hookClaimWorktreeProvisioner{
		cityPath:      cityPath,
		cityName:      cityName,
		cfg:           cfg,
		repoDir:       rigRoot,
		root:          filepath.Join(cityWorktreesRoot, rigComponent),
		rigName:       rigName,
		rigComponent:  rigComponent,
		base:          base,
		defaultRef:    "rig:" + rigName,
		owner:         owner,
		runtimeConfig: runtimeConfig,
	}
	return provisioner.provision, nil
}

func (p hookClaimWorktreeProvisioner) provision(bead beads.Bead, opts hookClaimOptions, ops hookClaimOps, dir string, stderr io.Writer) (hookProvisionedWorktree, beads.Bead, error) {
	beadID := strings.TrimSpace(bead.ID)
	if beadID == "" {
		return hookProvisionedWorktree{}, bead, fmt.Errorf("claimed bead has no id")
	}
	storeRef := p.claimStoreRef(opts, dir)
	if err := p.validateExistingEvidence(bead, storeRef); err != nil {
		return hookProvisionedWorktree{}, bead, err
	}
	if provisioned, updated, reused, err := p.reusePublishedEvidence(bead, storeRef, stderr); reused || err != nil {
		return provisioned, updated, err
	}
	beadComponent := agent.SanitizeQualifiedNameForSession(beadID)
	if beadComponent == "" {
		return hookProvisionedWorktree{}, bead, fmt.Errorf("cannot derive a worktree path component for bead %q", beadID)
	}
	spec := worktree.Spec{
		RepoDir:       p.repoDir,
		Root:          p.root,
		Path:          filepath.Join(p.root, beadComponent),
		Branch:        "gc/" + p.rigComponent + "/" + beadComponent,
		Base:          p.base,
		BeadID:        beadID,
		StoreRef:      storeRef,
		Creator:       hookWorktreeCreator,
		Owner:         p.owner,
		Generation:    hookWorktreeGeneration,
		Lifecycle:     worktree.LifecycleActive,
		SkipLFSSmudge: true,
	}
	if err := os.MkdirAll(spec.Root, 0o755); err != nil {
		return hookProvisionedWorktree{}, bead, fmt.Errorf("creating worktree root %q: %w", spec.Root, err)
	}
	report, err := worktree.Ensure(spec)
	if err != nil {
		return hookProvisionedWorktree{}, bead, err
	}
	stageConfig := p.runtimeConfig
	stageConfig.WorkDir = report.Path
	if err := runtime.StageSessionWorkDirWithWarnings(stageConfig, stderr, runtime.WithSkipTrackedOverlays()); err != nil {
		return hookProvisionedWorktree{}, bead, fmt.Errorf("staging overlays into %q: %w", report.Path, err)
	}
	patch, err := hookWorktreeMetadataPatch(spec, report)
	if err != nil {
		return hookProvisionedWorktree{}, bead, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), hookClaimMutationTimeout)
	defer cancel()
	if err := ops.StampWorkMeta(ctx, dir, opts.Env, beadID, opts.Assignee, patch); err != nil {
		return hookProvisionedWorktree{}, bead, fmt.Errorf("publishing worktree metadata: %w", err)
	}
	updated := bead
	if updated.Metadata == nil {
		updated.Metadata = map[string]string{}
	} else {
		updated.Metadata = maps.Clone(updated.Metadata)
	}
	maps.Copy(updated.Metadata, patch)
	return hookProvisionedWorktree{WorkDir: report.Path, WorkBranch: report.Branch}, updated, nil
}

func (p hookClaimWorktreeProvisioner) claimStoreRef(opts hookClaimOptions, dir string) string {
	if ref := strings.TrimSpace(opts.StoreRef); ref != "" {
		return ref
	}
	if ref := workflowStoreRefForDir(dir, p.cityPath, p.cityName, p.cfg); ref != "" {
		return ref
	}
	return p.defaultRef
}

func (p hookClaimWorktreeProvisioner) reusePublishedEvidence(bead beads.Bead, storeRef string, stderr io.Writer) (hookProvisionedWorktree, beads.Bead, bool, error) {
	spec, err := worktreeSpecForBead(bead, storeRef)
	if err != nil {
		return hookProvisionedWorktree{}, bead, true, err
	}
	if spec == nil {
		return hookProvisionedWorktree{}, bead, false, nil
	}
	report, err := worktree.Verify(*spec)
	if err != nil {
		return hookProvisionedWorktree{}, bead, true, fmt.Errorf("verifying published worktree evidence for %s: %w", bead.ID, err)
	}
	stageConfig := p.runtimeConfig
	stageConfig.WorkDir = report.Path
	if err := runtime.StageSessionWorkDirWithWarnings(stageConfig, stderr, runtime.WithSkipTrackedOverlays()); err != nil {
		return hookProvisionedWorktree{}, bead, true, fmt.Errorf("staging overlays into %q: %w", report.Path, err)
	}
	return hookProvisionedWorktree{WorkDir: report.Path, WorkBranch: report.Branch}, bead, true, nil
}

func (p hookClaimWorktreeProvisioner) validateExistingEvidence(bead beads.Bead, storeRef string) error {
	if !hookClaimWorktreeEvidenceIsWholeOrAbsent(bead) {
		return fmt.Errorf("work bead %s carries incomplete worktree ownership metadata", bead.ID)
	}
	if ref := strings.TrimSpace(bead.Metadata[beadmeta.RootStoreRefMetadataKey]); ref != "" &&
		canonicalEvidenceStoreRef(ref) != canonicalEvidenceStoreRef(storeRef) {
		return fmt.Errorf("work bead %s belongs to store %q, not %q", bead.ID, ref, storeRef)
	}
	return nil
}

func hookWorktreeMetadataPatch(spec worktree.Spec, report worktree.Report) (map[string]string, error) {
	if report.Provenance == nil {
		return nil, fmt.Errorf("worktree %q returned no provenance", report.Path)
	}
	return map[string]string{
		beadmeta.WorkDirMetadataKey:            report.Path,
		beadmeta.LegacyWorkDirMetadataKey:      report.Path,
		beadmeta.WorkBranchMetadataKey:         report.Branch,
		beadmeta.RootStoreRefMetadataKey:       spec.StoreRef,
		beadmeta.WorktreeRepoMetadataKey:       spec.RepoDir,
		beadmeta.WorktreeRootMetadataKey:       spec.Root,
		beadmeta.WorktreeBaseRefMetadataKey:    spec.Base,
		beadmeta.WorktreeBaseSHAMetadataKey:    report.Provenance.BaseSHA,
		beadmeta.WorktreeCreatorMetadataKey:    spec.Creator,
		beadmeta.WorktreeOwnerMetadataKey:      spec.Owner,
		beadmeta.WorktreeGenerationMetadataKey: spec.Generation,
		beadmeta.WorktreeLifecycleMetadataKey:  spec.Lifecycle,
	}, nil
}
