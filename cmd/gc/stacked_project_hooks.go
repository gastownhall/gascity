package main

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/fsys"
	"github.com/gastownhall/gascity/internal/hooks"
	"github.com/gastownhall/gascity/internal/runtime"
)

// Stacked project hooks (#5872). Some provider CLIs (codex) load a user-level
// hook file AND every project-level one and run the hooks of both. gc stages
// its project-level copy as the deliberate fallback for users with no user
// hooks; when the user layer a host-local session loads already provides every
// gc hook kind that copy would carry, staging it makes each gc hook fire twice.
// The decision below withholds the project copy for exactly that case.

// stackedHookAgent is what the stacked project-hook decision needs to know
// about one agent.
type stackedHookAgent struct {
	city            *config.City
	cityName        string
	cityPath        string
	sessionProvider string
	providers       map[string]config.ProviderSpec
	workspace       *config.Workspace
	agent           *config.Agent
	resolved        *config.ResolvedProvider
	qualifiedName   string
	packDirs        []string
	overlayDir      string
}

// stackedHookVerdict is an agent's decision for one stacking provider's
// project-level hook path that its overlays stage.
type stackedHookVerdict struct {
	family string
	// skip reports that the agent's sessions must not receive the file.
	skip bool
	// userPath is the user-level hook file that covers it when skip is set.
	userPath string
}

// stackedHookReadFunc reads a user-level hook file.
type stackedHookReadFunc func(path string) ([]byte, error)

// stackedProjectHookVerdicts returns, keyed by work-dir-relative path, the
// verdict for every stacking provider project hook file the agent's overlays
// would stage. A path is skipped only when the agent's session reads the
// controller host's filesystem (so the user layer gc probes is the one the
// session loads), no installer owns the path (install_agent_hooks naming the
// provider keeps the installer its sole writer), and
// hooks.ProjectHooksCoveredByUserLayer holds for the overlay sources. A user
// file that cannot be read or parsed keeps the fallback and is reported on
// stderr.
func stackedProjectHookVerdicts(in stackedHookAgent, read stackedHookReadFunc, stderr io.Writer) map[string]stackedHookVerdict {
	if in.agent == nil || in.resolved == nil {
		return nil
	}
	if stderr == nil {
		stderr = io.Discard
	}
	if read == nil {
		read = os.ReadFile
	}
	installHooks := config.ResolveInstallHooks(in.agent, in.workspace)
	slots := runtime.EffectiveOverlayProviderNames(runtime.Config{
		ProviderName:        resolvedProviderLaunchFamily(in.resolved),
		ProviderOverlayName: strings.TrimSpace(in.resolved.Name),
		InstallAgentHooks:   installHooks,
		PackOverlayDirs:     in.packDirs,
		OverlayDir:          in.overlayDir,
	})
	dirs := append([]string(nil), in.packDirs...)
	if in.overlayDir != "" {
		dirs = append(dirs, in.overlayDir)
	}
	type candidate struct {
		family  string
		sources [][]byte
		readErr error
	}
	candidates := make(map[string]*candidate)
	for _, slot := range slots {
		family := config.BuiltinFamily(slot, in.providers)
		rel, ok := hooks.StackedProjectHookPath(family)
		if !ok {
			continue
		}
		for _, dir := range dirs {
			for _, src := range []string{
				filepath.Join(dir, filepath.FromSlash(rel)),
				filepath.Join(dir, "per-provider", slot, filepath.FromSlash(rel)),
			} {
				data, err := os.ReadFile(src)
				if errors.Is(err, fs.ErrNotExist) {
					continue
				}
				c := candidates[rel]
				if c == nil {
					c = &candidate{family: family}
					candidates[rel] = c
				}
				if err != nil {
					c.readErr = fmt.Errorf("reading overlay %s: %w", src, err)
					continue
				}
				c.sources = append(c.sources, data)
			}
		}
	}
	if len(candidates) == 0 {
		return nil
	}
	resolver := func(name string) string { return config.BuiltinFamily(name, in.providers) }
	installed := hooks.InstalledWorkDirPaths(installHooks, resolver)
	hostLocal := canStage1Materialize(materializationRuntimeFor(in.city, in.cityName, in.cityPath, in.sessionProvider), in.agent)
	getenv := stackedHookGetenv(in.workspace, in.resolved, in.agent)

	out := make(map[string]stackedHookVerdict, len(candidates))
	for rel, c := range candidates {
		v := stackedHookVerdict{family: c.family}
		out[rel] = v
		if installed[rel] || !hostLocal {
			continue
		}
		if c.readErr != nil {
			fmt.Fprintf(stderr, "agent %q: %s user hooks: %v\n", in.qualifiedName, c.family, c.readErr) //nolint:errcheck
			continue
		}
		userPath, ok := hooks.StackedUserHooksPath(c.family, getenv)
		if !ok {
			continue
		}
		userDoc, err := read(userPath)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			fmt.Fprintf(stderr, "agent %q: %s user hooks: reading %s: %v\n", in.qualifiedName, c.family, userPath, err) //nolint:errcheck
			continue
		}
		covered, err := hooks.ProjectHooksCoveredByUserLayer(c.family, in.cityPath, userDoc, c.sources)
		if err != nil {
			fmt.Fprintf(stderr, "agent %q: %s user hooks %s: %v\n", in.qualifiedName, c.family, userPath, err) //nolint:errcheck
			continue
		}
		if covered {
			out[rel] = stackedHookVerdict{family: c.family, skip: true, userPath: userPath}
		}
	}
	return out
}

// stackedHookGetenv resolves environment variables the way a host session
// inherits them: the operator-authored env layers resolveTemplate folds into
// the session (workspace, provider, agent) over the controller's own process
// environment. gc sets neither HOME nor CODEX_HOME for sessions.
func stackedHookGetenv(workspace *config.Workspace, resolved *config.ResolvedProvider, a *config.Agent) func(string) string {
	var workspaceEnv map[string]string
	if workspace != nil {
		workspaceEnv = workspace.Env
	}
	var providerEnv map[string]string
	if resolved != nil {
		providerEnv = resolved.Env
	}
	env := mergeEnv(expandEnvMap(workspaceEnv), expandEnvMap(providerEnv), expandEnvMap(a.Env))
	return func(key string) string {
		if v, ok := env[key]; ok {
			return v
		}
		return os.Getenv(key)
	}
}

// stackedHookTickState caches the stacked project-hook inputs of one
// desired-state build: each user-level hook file is read once, each agent's
// verdicts are computed once, and the work dirs of configured agents are
// resolved once.
type stackedHookTickState struct {
	mu            sync.Mutex
	userDocs      map[string]stackedHookUserDoc
	verdicts      map[string]*stackedHookAgentEntry
	workDirAgents map[string][]int
}

// stackedHookAgentEntry is one agent's cached verdicts and the warnings
// computing them produced, reported once per build to the agent's own stderr.
type stackedHookAgentEntry struct {
	verdicts map[string]stackedHookVerdict
	warnings string
	reported bool
}

type stackedHookUserDoc struct {
	data []byte
	err  error
}

func newStackedHookTickState() *stackedHookTickState {
	return &stackedHookTickState{
		userDocs: make(map[string]stackedHookUserDoc),
		verdicts: make(map[string]*stackedHookAgentEntry),
	}
}

// read returns path's content, reading it at most once per build.
func (s *stackedHookTickState) read(path string) ([]byte, error) {
	if s == nil {
		return os.ReadFile(path)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if doc, ok := s.userDocs[path]; ok {
		return doc.data, doc.err
	}
	data, err := os.ReadFile(path)
	s.userDocs[path] = stackedHookUserDoc{data: data, err: err}
	return data, err
}

// stackedHookInputs builds the decision inputs for one agent of this build.
func (bp *agentBuildParams) stackedHookInputs(cfgAgent *config.Agent, resolved *config.ResolvedProvider, qualifiedName string) stackedHookAgent {
	rigName := sessionSetupContextForAgent(bp.cityPath, bp.cityName, qualifiedName, cfgAgent, bp.rigs).Rig
	return stackedHookAgent{
		city:            bp.city,
		cityName:        bp.cityName,
		cityPath:        bp.cityPath,
		sessionProvider: bp.sessionProvider,
		providers:       bp.providers,
		workspace:       bp.workspace,
		agent:           cfgAgent,
		resolved:        resolved,
		qualifiedName:   qualifiedName,
		packDirs:        effectiveOverlayDirs(bp.packOverlayDirs, bp.rigOverlayDirs, rigName),
		overlayDir:      resolveOverlayDir(cfgAgent.OverlayDir, bp.cityPath),
	}
}

// agentStackedHookVerdicts returns qualifiedName's verdicts, computed once per
// build. When report is set, the warnings computing them produced are written
// to stderr, once per build.
func (bp *agentBuildParams) agentStackedHookVerdicts(cfgAgent *config.Agent, resolved *config.ResolvedProvider, qualifiedName string, report bool, stderr io.Writer) map[string]stackedHookVerdict {
	s := bp.stackedHooks
	if s == nil {
		w := io.Discard
		if report && stderr != nil {
			w = stderr
		}
		return stackedProjectHookVerdicts(bp.stackedHookInputs(cfgAgent, resolved, qualifiedName), nil, w)
	}
	s.mu.Lock()
	entry, ok := s.verdicts[qualifiedName]
	s.mu.Unlock()
	if !ok {
		var warnings strings.Builder
		entry = &stackedHookAgentEntry{
			verdicts: stackedProjectHookVerdicts(bp.stackedHookInputs(cfgAgent, resolved, qualifiedName), s.read, &warnings),
			warnings: warnings.String(),
		}
		s.mu.Lock()
		if cached, raced := s.verdicts[qualifiedName]; raced {
			entry = cached
		} else {
			s.verdicts[qualifiedName] = entry
		}
		s.mu.Unlock()
	}
	if report && stderr != nil {
		s.mu.Lock()
		warn := !entry.reported && entry.warnings != ""
		entry.reported = true
		s.mu.Unlock()
		if warn {
			io.WriteString(stderr, entry.warnings) //nolint:errcheck
		}
	}
	return entry.verdicts
}

// configuredAgentsInWorkDir returns the indexes into bp.agents of the
// configured agents whose work dir is workDir.
func (bp *agentBuildParams) configuredAgentsInWorkDir(workDir string) []int {
	s := bp.stackedHooks
	if s != nil {
		s.mu.Lock()
		defer s.mu.Unlock()
		if s.workDirAgents != nil {
			return s.workDirAgents[workDir]
		}
	}
	byDir := make(map[string][]int)
	for i := range bp.agents {
		a := &bp.agents[i]
		dir, err := resolveConfiguredWorkDirPath(bp.cityPath, bp.cityName, a.QualifiedName(), a, bp.rigs)
		if err != nil {
			continue
		}
		byDir[filepath.Clean(dir)] = append(byDir[filepath.Clean(dir)], i)
	}
	if s != nil {
		s.workDirAgents = byDir
	}
	return byDir[workDir]
}

// stackedHookSkips returns the stacked project hook paths the agent's
// sessions in workDir must not receive on this build. A path the agent skips
// is kept when any other configured agent sharing workDir stages it and does
// not skip it, so one tick never stages a file another agent's tick retires.
func (bp *agentBuildParams) stackedHookSkips(cfgAgent *config.Agent, resolved *config.ResolvedProvider, qualifiedName, workDir string, stderr io.Writer) map[string]stackedHookVerdict {
	if bp == nil || cfgAgent == nil || resolved == nil || workDir == "" {
		return nil
	}
	own := bp.agentStackedHookVerdicts(cfgAgent, resolved, qualifiedName, true, stderr)
	skips := make(map[string]stackedHookVerdict)
	for rel, v := range own {
		if v.skip {
			skips[rel] = v
		}
	}
	if len(skips) == 0 {
		return nil
	}
	for _, i := range bp.configuredAgentsInWorkDir(filepath.Clean(workDir)) {
		other := &bp.agents[i]
		otherResolved, err := config.ResolveProvider(other, bp.workspace, bp.providers, bp.lookPath)
		if err != nil {
			continue
		}
		for rel, v := range bp.agentStackedHookVerdicts(other, otherResolved, other.QualifiedName(), false, nil) {
			if !v.skip {
				delete(skips, rel)
			}
		}
	}
	if len(skips) == 0 {
		return nil
	}
	return skips
}

// retireStackedProjectHooks removes the gc-managed entries a previous tick or
// session start staged into each skipped project hook file in workDir: an
// all-managed copy is deleted, a copy that also holds user-authored entries
// is rewritten with only those. A git-tracked copy is user-owned and left
// alone, as is a copy that cannot be parsed.
func retireStackedProjectHooks(workDir, qualifiedName string, skips map[string]stackedHookVerdict, stderr io.Writer) {
	rels := make([]string, 0, len(skips))
	for rel := range skips {
		rels = append(rels, rel)
	}
	sort.Strings(rels)
	for _, rel := range rels {
		v := skips[rel]
		path := filepath.Join(workDir, filepath.FromSlash(rel))
		data, err := os.ReadFile(path)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			fmt.Fprintf(stderr, "agent %q: reading %s: %v\n", qualifiedName, path, err) //nolint:errcheck
			continue
		}
		if runtime.OverlayPathMayBeTracked(workDir, rel) {
			continue
		}
		rest, empty, changed, err := hooks.StripManagedProjectHooks(v.family, data)
		if err != nil {
			fmt.Fprintf(stderr, "agent %q: leaving %s in place: %v\n", qualifiedName, path, err) //nolint:errcheck
			continue
		}
		if !changed {
			continue
		}
		if empty {
			if err := os.Remove(path); err != nil {
				fmt.Fprintf(stderr, "agent %q: removing %s: %v\n", qualifiedName, path, err) //nolint:errcheck
				continue
			}
			fmt.Fprintf(stderr, "agent %q: removed %s: its gc hooks already load from %s\n", qualifiedName, path, v.userPath) //nolint:errcheck
			continue
		}
		info, err := os.Stat(path)
		if err != nil {
			fmt.Fprintf(stderr, "agent %q: stat %s: %v\n", qualifiedName, path, err) //nolint:errcheck
			continue
		}
		if err := fsys.WriteFileAtomic(fsys.OSFS{}, path, rest, info.Mode().Perm()); err != nil {
			fmt.Fprintf(stderr, "agent %q: rewriting %s: %v\n", qualifiedName, path, err) //nolint:errcheck
			continue
		}
		fmt.Fprintf(stderr, "agent %q: removed gc hooks from %s: they already load from %s\n", qualifiedName, path, v.userPath) //nolint:errcheck
	}
}
