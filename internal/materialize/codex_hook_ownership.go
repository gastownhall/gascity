package materialize

import (
	"crypto/sha256"
	"fmt"
	"path"
	"path/filepath"
	"sort"
	"strings"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/fsys"
	"github.com/gastownhall/gascity/internal/hooks"
	"github.com/gastownhall/gascity/internal/pathutil"
	"github.com/gastownhall/gascity/internal/runtime"
	workdirutil "github.com/gastownhall/gascity/internal/workdir"
)

// CodexManagedMergeablePath is the workdir-relative Codex hook document for
// which the controller can establish a single-writer ownership boundary.
const CodexManagedMergeablePath = ".codex/hooks.json"

// ApplyConfiguredSessionOverlayHints projects the provider and configured
// agent overlay sources that runtime staging will consume. It is shared by CLI
// and API runtime construction so a persisted session's metadata shape cannot
// make the two front doors choose different hook layers.
func ApplyConfiguredSessionOverlayHints(
	cityPath string,
	cfg *config.City,
	template string,
	resolved *config.ResolvedProvider,
	hints *runtime.Config,
) {
	if cfg == nil || resolved == nil || hints == nil {
		return
	}
	hints.ProviderName = resolvedProviderLaunchFamily(resolved)
	hints.ProviderOverlayName = strings.TrimSpace(resolved.Name)
	hints.PackOverlayDirs = append([]string(nil), cfg.PackOverlayDirs...)

	agentCfg := config.FindAgent(cfg, strings.TrimSpace(template))
	if agentCfg == nil {
		return
	}
	hints.InstallAgentHooks = config.ResolveInstallHooks(agentCfg, &cfg.Workspace)
	pathContext := workdirutil.PathContextForQualifiedName(
		cityPath,
		workdirutil.CityName(cityPath, cfg),
		agentCfg.QualifiedName(),
		*agentCfg,
		cfg.Rigs,
	)
	hints.PackOverlayDirs = append(hints.PackOverlayDirs, cfg.RigOverlayDirs[pathContext.Rig]...)
	hints.OverlayDir = strings.TrimSpace(agentCfg.OverlayDir)
	if hints.OverlayDir != "" && !filepath.IsAbs(hints.OverlayDir) {
		hints.OverlayDir = filepath.Join(cityPath, hints.OverlayDir)
	}
}

// ResolveConfiguredCodexHookOwnership classifies ownership by filesystem
// location rather than by session-origin metadata. It returns the managed path
// with its verified raw content digest when workDir is one of the current
// config's agent or named-session homes and its Codex hook document is at the
// configured fixed point. An eligible configured home whose document cannot
// be verified returns the path with an empty digest so runtime preflight fails
// closed before overlay staging can mutate it. Isolated session workdirs return
// nil and retain the legacy full-overlay staging behavior.
//
// prepared may contain the result of the controller's immediately preceding
// reconciliation. A non-empty digest is reverified against the stable regular
// file snapshot; an empty digest preserves the controller's fail-closed claim.
// Resume callers pass nil and are verified directly from the live document.
func ResolveConfiguredCodexHookOwnership(
	cityPath string,
	cfg *config.City,
	template string,
	workDir string,
	hints runtime.Config,
	prepared map[string]string,
) map[string]string {
	if cfg == nil {
		return nil
	}
	agentCfg := config.FindAgent(cfg, strings.TrimSpace(template))
	if agentCfg == nil || strings.TrimSpace(cityPath) == "" || strings.TrimSpace(workDir) == "" {
		return nil
	}
	if !runtimeConfigIncludesCodex(hints, cfg.Providers) {
		return nil
	}
	if !matchesConfiguredAgentHome(cityPath, cfg, agentCfg, workDir) {
		return nil
	}

	result := map[string]string{CodexManagedMergeablePath: ""}
	if preparedHash, ok := prepared[CodexManagedMergeablePath]; ok && strings.TrimSpace(preparedHash) == "" {
		return result
	}

	providers := runtime.EffectiveOverlayProviderNames(hints)
	var layers [][]byte
	overlayDirs := append([]string(nil), hints.PackOverlayDirs...)
	if strings.TrimSpace(hints.OverlayDir) != "" {
		overlayDirs = append(overlayDirs, hints.OverlayDir)
	}
	for _, overlayDir := range overlayDirs {
		configured, err := hooks.ReadCodexHookOverlayLayers(overlayDir, providers)
		if err != nil {
			return result
		}
		layers = append(layers, configured...)
	}

	hookPath := filepath.Join(workDir, filepath.FromSlash(CodexManagedMergeablePath))
	data, _, err := fsys.ReadRegularFileStable(fsys.OSFS{}, hookPath)
	if err != nil || !hooks.CodexHooksAreConvergedWithOverlays(data, cityPath, layers) {
		return result
	}
	digest := fmt.Sprintf("%x", sha256.Sum256(data))
	if preparedHash, ok := prepared[CodexManagedMergeablePath]; ok && preparedHash != digest {
		return result
	}
	result[CodexManagedMergeablePath] = digest
	return result
}

// ApplyVerifiedMergeableOwnership projects a verified-files map onto cfg. All
// map keys become owned paths, including empty-digest entries that deliberately
// fail runtime preflight. A canonical self-copy is added only when the current
// stable regular file still matches its non-empty digest.
func ApplyVerifiedMergeableOwnership(cfg *runtime.Config, verified map[string]string) {
	if cfg == nil || len(verified) == 0 {
		return
	}
	ownedPaths := make([]string, 0, len(verified))
	ownedCopies := make([]runtime.CopyEntry, 0, len(verified))
	for rawRel, expectedHash := range verified {
		rel := path.Clean(filepath.ToSlash(rawRel))
		if rel == "." || rel == ".." || strings.HasPrefix(rel, "../") || path.IsAbs(rel) {
			continue
		}
		ownedPaths = append(ownedPaths, rel)
		if strings.TrimSpace(expectedHash) == "" || strings.TrimSpace(cfg.WorkDir) == "" {
			continue
		}
		src := filepath.Join(cfg.WorkDir, filepath.FromSlash(rel))
		data, _, err := fsys.ReadRegularFileStable(fsys.OSFS{}, src)
		if err != nil || fmt.Sprintf("%x", sha256.Sum256(data)) != expectedHash {
			continue
		}
		ownedCopies = append(ownedCopies, runtime.CopyEntry{
			Src:         src,
			RelDst:      rel,
			Probed:      true,
			ContentHash: expectedHash,
		})
	}
	sort.Strings(ownedPaths)
	if len(ownedPaths) == 0 {
		return
	}
	cfg.ReconcilerOwnedMergeablePaths = ownedPaths
	cfg.CopyFiles = replaceOwnedCopyFiles(cfg.CopyFiles, ownedPaths, ownedCopies)
}

func runtimeConfigIncludesCodex(hints runtime.Config, providers map[string]config.ProviderSpec) bool {
	if strings.TrimSpace(hints.ProviderName) == "codex" {
		return true
	}
	for _, provider := range hints.InstallAgentHooks {
		provider = strings.TrimSpace(provider)
		if provider == "codex" || config.BuiltinFamily(provider, providers) == "codex" {
			return true
		}
	}
	return false
}

func resolvedProviderLaunchFamily(resolved *config.ResolvedProvider) string {
	if resolved == nil {
		return ""
	}
	if family := strings.TrimSpace(resolved.BuiltinAncestor); family != "" {
		return family
	}
	return strings.TrimSpace(resolved.Name)
}

func matchesConfiguredAgentHome(cityPath string, cfg *config.City, agentCfg *config.Agent, actual string) bool {
	if cfg == nil || agentCfg == nil {
		return false
	}
	identities := []string{agentCfg.QualifiedName()}
	for i := range cfg.NamedSessions {
		named := &cfg.NamedSessions[i]
		if named.TemplateQualifiedName() == agentCfg.QualifiedName() {
			identities = append(identities, named.QualifiedName())
		}
	}
	seen := make(map[string]bool, len(identities))
	for _, identity := range identities {
		identity = strings.TrimSpace(identity)
		if identity == "" || seen[identity] {
			continue
		}
		seen[identity] = true
		home, err := workdirutil.ResolveWorkDirPathStrict(
			cityPath,
			workdirutil.CityName(cityPath, cfg),
			identity,
			*agentCfg,
			cfg.Rigs,
		)
		if err == nil && pathutil.SamePath(home, actual) {
			return true
		}
	}
	return false
}

func replaceOwnedCopyFiles(existing []runtime.CopyEntry, ownedPaths []string, owned []runtime.CopyEntry) []runtime.CopyEntry {
	ownedDestinations := make(map[string]bool, len(ownedPaths))
	for _, rel := range ownedPaths {
		ownedDestinations[path.Clean(filepath.ToSlash(rel))] = true
	}
	ownedSources := make([]string, 0, len(owned))
	for _, entry := range owned {
		if strings.TrimSpace(entry.Src) != "" {
			ownedSources = append(ownedSources, entry.Src)
		}
	}
	result := make([]runtime.CopyEntry, 0, len(existing)+len(owned))
	for _, entry := range existing {
		if ownedDestinations[path.Clean(filepath.ToSlash(entry.RelDst))] {
			continue
		}
		duplicateSource := false
		for _, source := range ownedSources {
			if pathutil.SamePath(entry.Src, source) {
				duplicateSource = true
				break
			}
		}
		if !duplicateSource {
			result = append(result, entry)
		}
	}
	return append(result, owned...)
}
