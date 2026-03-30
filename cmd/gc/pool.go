package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"text/template"
	"time"

	"github.com/gastownhall/gascity/internal/agent"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/telemetry"
)

type poolSessionRef struct {
	qualifiedInstance string
	sessionName       string
}

// ScaleCheckRunner runs a scale_check command and returns stdout.
// dir specifies the working directory for the command (e.g., rig path
// for rig-scoped pools so bd queries the correct database).
type ScaleCheckRunner func(command, dir string) (string, error)

// shellScaleCheck runs a scale_check command via sh -c and returns stdout.
// dir sets the command's working directory. Times out after 30 seconds.
func shellScaleCheck(command, dir string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "sh", "-c", command)
	cmd.WaitDelay = 2 * time.Second
	if dir != "" {
		cmd.Dir = dir
	}
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("running scale_check %q: %w", command, err)
	}
	return string(out), nil
}

// evaluatePool runs check, parses the output as an integer, and clamps
// the result to [min, max]. Returns min on error (honors configured minimum).
func evaluatePool(agentName string, pool config.PoolConfig, dir string, runner ScaleCheckRunner) (int, error) {
	start := time.Now()
	out, err := runner(pool.Check, dir)
	durationMs := float64(time.Since(start).Milliseconds())
	if err != nil {
		telemetry.RecordPoolCheck(context.Background(), agentName, durationMs, pool.Min, err)
		return pool.Min, fmt.Errorf("agent %q: %w", agentName, err)
	}
	trimmed := strings.TrimSpace(out)
	if trimmed == "" {
		checkErr := fmt.Errorf("agent %q: check %q produced empty output", agentName, pool.Check)
		telemetry.RecordPoolCheck(context.Background(), agentName, durationMs, pool.Min, checkErr)
		return pool.Min, checkErr
	}
	n, err := strconv.Atoi(trimmed)
	if err != nil {
		parseErr := fmt.Errorf("agent %q: check output %q is not an integer", agentName, trimmed)
		telemetry.RecordPoolCheck(context.Background(), agentName, durationMs, pool.Min, parseErr)
		return pool.Min, parseErr
	}
	desired := n
	if desired < pool.Min {
		desired = pool.Min
	}
	if pool.Max >= 0 && desired > pool.Max {
		desired = pool.Max
	}
	telemetry.RecordPoolCheck(context.Background(), agentName, durationMs, desired, nil)
	return desired, nil
}

// SessionSetupContext holds template variables for session_setup command expansion.
type SessionSetupContext struct {
	Session   string // tmux session name
	Agent     string // qualified agent name
	AgentBase string // unqualified agent name or pool instance name
	Rig       string // rig name (empty for city-scoped)
	RigRoot   string // absolute path to the rig root (empty for city-scoped)
	CityRoot  string // city directory path
	CityName  string // workspace name
	WorkDir   string // agent working directory
	ConfigDir string // source directory where agent config was defined
}

// expandSessionSetup expands Go text/template strings in session_setup commands.
// On parse or execute error, the raw command is kept (graceful fallback).
func expandSessionSetup(cmds []string, ctx SessionSetupContext) []string {
	if len(cmds) == 0 {
		return nil
	}
	result := make([]string, len(cmds))
	for i, raw := range cmds {
		tmpl, err := template.New("setup").Parse(raw)
		if err != nil {
			result[i] = raw
			continue
		}
		var buf bytes.Buffer
		if err := tmpl.Execute(&buf, ctx); err != nil {
			result[i] = raw
			continue
		}
		result[i] = buf.String()
	}
	return result
}

// resolveSetupScript resolves a session_setup_script path relative to cityPath.
// Returns the path unchanged if already absolute.
func resolveSetupScript(script, cityPath string) string {
	if script == "" || filepath.IsAbs(script) {
		return script
	}
	return filepath.Join(cityPath, script)
}

// deepCopyAgent creates a deep copy of a config.Agent with a new name and dir.
// Slice and map fields are independently allocated so mutations to the copy
// don't affect the original.
func deepCopyAgent(src *config.Agent, name, dir string) config.Agent {
	dst := config.Agent{
		Name:                 name,
		Description:          src.Description,
		Dir:                  dir,
		WorkDir:              src.WorkDir,
		Scope:                src.Scope,
		Session:              src.Session,
		Provider:             src.Provider,
		PromptTemplate:       src.PromptTemplate,
		Nudge:                src.Nudge,
		StartCommand:         src.StartCommand,
		PromptMode:           src.PromptMode,
		PromptFlag:           src.PromptFlag,
		ReadyPromptPrefix:    src.ReadyPromptPrefix,
		DefaultSlingFormula:  src.DefaultSlingFormula,
		WorkQuery:            src.WorkQuery,
		SlingQuery:           src.SlingQuery,
		SessionSetupScript:   src.SessionSetupScript,
		OverlayDir:           src.OverlayDir,
		SourceDir:            src.SourceDir,
		Fallback:             src.Fallback,
		IdleTimeout:          src.IdleTimeout,
		SleepAfterIdle:       src.SleepAfterIdle,
		SleepAfterIdleSource: src.SleepAfterIdleSource,
		Suspended:            src.Suspended,
		ResumeCommand:        src.ResumeCommand,
		WakeMode:             src.WakeMode,
		PoolName:             src.QualifiedName(),
		Implicit:             src.Implicit,
		ScaleCheck:           src.ScaleCheck,
	}
	if len(src.DependsOn) > 0 {
		dst.DependsOn = make([]string, len(src.DependsOn))
		copy(dst.DependsOn, src.DependsOn)
	}
	if len(src.Args) > 0 {
		dst.Args = make([]string, len(src.Args))
		copy(dst.Args, src.Args)
	}
	if len(src.ProcessNames) > 0 {
		dst.ProcessNames = make([]string, len(src.ProcessNames))
		copy(dst.ProcessNames, src.ProcessNames)
	}
	if len(src.Env) > 0 {
		dst.Env = make(map[string]string, len(src.Env))
		for k, v := range src.Env {
			dst.Env[k] = v
		}
	}
	if len(src.PreStart) > 0 {
		dst.PreStart = make([]string, len(src.PreStart))
		copy(dst.PreStart, src.PreStart)
	}
	if len(src.SessionSetup) > 0 {
		dst.SessionSetup = make([]string, len(src.SessionSetup))
		copy(dst.SessionSetup, src.SessionSetup)
	}
	if len(src.SessionLive) > 0 {
		dst.SessionLive = make([]string, len(src.SessionLive))
		copy(dst.SessionLive, src.SessionLive)
	}
	if len(src.InjectFragments) > 0 {
		dst.InjectFragments = make([]string, len(src.InjectFragments))
		copy(dst.InjectFragments, src.InjectFragments)
	}
	if len(src.InstallAgentHooks) > 0 {
		dst.InstallAgentHooks = make([]string, len(src.InstallAgentHooks))
		copy(dst.InstallAgentHooks, src.InstallAgentHooks)
	}
	if src.Pool != nil {
		poolCopy := *src.Pool
		if len(src.Pool.NamepoolNames) > 0 {
			poolCopy.NamepoolNames = make([]string, len(src.Pool.NamepoolNames))
			copy(poolCopy.NamepoolNames, src.Pool.NamepoolNames)
		}
		dst.Pool = &poolCopy
	}
	if src.ReadyDelayMs != nil {
		v := *src.ReadyDelayMs
		dst.ReadyDelayMs = &v
	}
	if src.EmitsPermissionWarning != nil {
		v := *src.EmitsPermissionWarning
		dst.EmitsPermissionWarning = &v
	}
	if src.HooksInstalled != nil {
		v := *src.HooksInstalled
		dst.HooksInstalled = &v
	}
	if src.Attach != nil {
		v := *src.Attach
		dst.Attach = &v
	}
	if src.MaxActiveSessions != nil {
		v := *src.MaxActiveSessions
		dst.MaxActiveSessions = &v
	}
	if src.MinActiveSessions != nil {
		v := *src.MinActiveSessions
		dst.MinActiveSessions = &v
	}
	return dst
}

// runPoolOnBoot runs on_boot commands for all pool agents at controller startup.
// Errors are logged but not fatal — the controller continues regardless.
func runPoolOnBoot(cfg *config.City, cityPath string, runner ScaleCheckRunner, stderr io.Writer) {
	for _, a := range cfg.Agents {
		if !a.IsPool() || a.Implicit {
			continue
		}
		cmd := a.EffectiveOnBoot()
		if cmd == "" {
			continue
		}
		dir := agentCommandDir(cityPath, &a, cfg.Rigs)
		if _, err := runner(cmd, dir); err != nil {
			fmt.Fprintf(stderr, "on_boot %s: %v\n", a.QualifiedName(), err) //nolint:errcheck // best-effort stderr
		}
	}
}

// discoverPoolInstances returns qualified instance names for a multi-instance pool.
// For bounded pools (max > 1), generates static names {name}-1..{name}-{max}.
// For unlimited pools (max < 0), discovers running instances via session provider
// prefix matching.
func discoverPoolInstances(agentName, agentDir string, pool config.PoolConfig,
	cityName, st string, sp runtime.Provider,
) []string {
	if !pool.IsUnlimited() {
		// Bounded pool: static enumeration.
		var names []string
		for i := 1; i <= pool.Max; i++ {
			instanceName := poolInstanceName(agentName, i, pool)
			qn := instanceName
			if agentDir != "" {
				qn = agentDir + "/" + instanceName
			}
			names = append(names, qn)
		}
		return names
	}

	// Unlimited pool: discover running instances via session prefix.
	// TODO(Phase 2): This uses legacy SessionNameFor for prefix matching.
	// When bead-derived session names ("s-{beadID}") are active, this prefix
	// match will fail. Migrate to bead store query by template metadata.
	qnPrefix := agentName + "-"
	if agentDir != "" {
		qnPrefix = agentDir + "/" + agentName + "-"
	}
	// Build the session name prefix to match against running sessions.
	snPrefix := agent.SessionNameFor(cityName, qnPrefix, st)
	running, err := sp.ListRunning("")
	if err != nil {
		return nil
	}
	var names []string
	for _, sn := range running {
		if strings.HasPrefix(sn, snPrefix) {
			// Reverse the session name construction to extract the qualified name.
			// SessionNameFor replaces "/" with "--"; reverse that.
			qnSanitized := sn
			// Strip the template prefix: for default template (empty), the
			// session name IS the sanitized agent name. For custom templates,
			// we need to compute the prefix from the template.
			templatePrefix := agent.SessionNameFor(cityName, "", st)
			if templatePrefix != "" && strings.HasPrefix(qnSanitized, templatePrefix) {
				qnSanitized = qnSanitized[len(templatePrefix):]
			}
			// Unsanitize: "--" → "/"
			qn := strings.ReplaceAll(qnSanitized, "--", "/")
			names = append(names, qn)
		}
	}
	return names
}

func resolvePoolSessionRefs(
	store beads.Store,
	agentName, agentDir string,
	pool config.PoolConfig,
	cityName, sessionTemplate string,
	sp runtime.Provider,
	stderr io.Writer,
) []poolSessionRef {
	template := agentName
	if agentDir != "" {
		template = agentDir + "/" + agentName
	}
	seenSessions := make(map[string]bool)
	var refs []poolSessionRef
	poolSessions, err := lookupPoolSessionNames(store, template)
	if err != nil && stderr != nil {
		fmt.Fprintf(stderr, "gc lifecycle: pool bead lookup for %s returned error (legacy discovery also runs): %v\n", template, err) //nolint:errcheck
	}
	poolInstances := make([]string, 0, len(poolSessions))
	for qualifiedInstance := range poolSessions {
		poolInstances = append(poolInstances, qualifiedInstance)
	}
	sort.Strings(poolInstances)
	for _, qualifiedInstance := range poolInstances {
		sessionName := poolSessions[qualifiedInstance]
		if sessionName == "" || seenSessions[sessionName] {
			continue
		}
		seenSessions[sessionName] = true
		refs = append(refs, poolSessionRef{
			qualifiedInstance: qualifiedInstance,
			sessionName:       sessionName,
		})
	}
	for _, qualifiedInstance := range discoverPoolInstances(agentName, agentDir, pool, cityName, sessionTemplate, sp) {
		sessionName := lookupSessionNameOrLegacy(store, cityName, qualifiedInstance, sessionTemplate)
		if sessionName == "" || seenSessions[sessionName] {
			continue
		}
		seenSessions[sessionName] = true
		refs = append(refs, poolSessionRef{
			qualifiedInstance: qualifiedInstance,
			sessionName:       sessionName,
		})
	}
	return refs
}
