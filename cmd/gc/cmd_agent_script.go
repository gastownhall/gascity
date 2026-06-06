package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"
)

const agentScriptActionTimeout = 5 * time.Minute

func newAgentScriptCmd(stdout, stderr io.Writer) *cobra.Command {
	var scriptPath string
	cmd := &cobra.Command{
		Use:   "agent-script --script <path>",
		Short: "Run a deterministic YAML agent script",
		Long: `Run a deterministic YAML agent script for examples and demos.

The runner probes gc hook once, selects the matching turn, and executes the
configured actions. It is intentionally small and generic: role behavior stays
in the YAML script.

Status: experimental. Gas City owns this runner so repository examples can be
tested without external helper binaries; the YAML action surface may change
until a stable SDK boundary exists.

For k8s-backed agent-script agents, set lifecycle = "one_shot" in the agent
config so the runtime treats a clean script exit as expected work completion
instead of startup death.`,
		RunE: func(_ *cobra.Command, _ []string) error {
			code := runAgentScript(scriptPath, stdout, stderr)
			return exitForCode(code)
		},
	}
	cmd.Flags().StringVar(&scriptPath, "script", "", "agent script YAML file")
	_ = cmd.MarkFlagRequired("script")
	return cmd
}

type agentScriptDocument struct {
	Description   string              `yaml:"description"`
	SchemaVersion string              `yaml:"schema_version"`
	Setup         []agentScriptAction `yaml:"setup"`
	Turns         []agentScriptTurn   `yaml:"turns"`
}

type agentScriptTurn struct {
	When map[string]any      `yaml:"when"`
	Do   []agentScriptAction `yaml:"do"`
}

type agentScriptAction map[string]any

type agentScriptBead struct {
	ID       string
	Title    string
	Metadata map[string]string
}

func (b *agentScriptBead) UnmarshalJSON(data []byte) error {
	var raw struct {
		ID       string                     `json:"id"`
		Title    string                     `json:"title"`
		Metadata map[string]json.RawMessage `json:"metadata"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	b.ID = raw.ID
	b.Title = raw.Title
	b.Metadata = make(map[string]string, len(raw.Metadata))
	for key, value := range raw.Metadata {
		var s string
		if err := json.Unmarshal(value, &s); err == nil {
			b.Metadata[key] = s
			continue
		}
		b.Metadata[key] = strings.TrimSpace(string(value))
	}
	return nil
}

type agentScriptContext struct {
	bead  agentScriptBead
	rig   string
	alias string
}

type agentScriptExecutor struct {
	stdout     io.Writer
	stderr     io.Writer
	runCommand func(name string, args ...string) error
	runShell   func(command string, env []string) error
	// runCommandInStore runs a command in a specific working dir + env so a
	// cross-store-eligible runner's bd write lands in the bead's own store
	// (vp-kvp stage iii write half). Nil in tests/legacy paths → falls back to
	// runCommand, leaving rig-agent behavior byte-for-byte unchanged.
	runCommandInStore func(dir string, env []string, name string, args ...string) error
	// resolveBeadStore returns the dir + env a bd write against beadID must use
	// (ok=true) when the current runner must claim/update across stores; ok=false
	// runs the write in the default (inherited) environment.
	resolveBeadStore func(beadID string) (dir string, env []string, ok bool)
}

// runBDForBead runs a bd subprocess against beadID, redirecting it to the bead's
// owning store when the runner is cross-store-eligible (vp-kvp stage iii write
// half). Falls back to the default environment otherwise.
func (e agentScriptExecutor) runBDForBead(beadID string, args ...string) error {
	if e.resolveBeadStore != nil && e.runCommandInStore != nil {
		if dir, env, ok := e.resolveBeadStore(strings.TrimSpace(beadID)); ok {
			return e.runCommandInStore(dir, env, "bd", args...)
		}
	}
	return e.runCommand("bd", args...)
}

func runAgentScript(scriptPath string, stdout, stderr io.Writer) int {
	executor := agentScriptExecutor{
		stdout: stdout,
		stderr: stderr,
		runCommand: func(name string, args ...string) error {
			return runAgentScriptCommand(stdout, stderr, name, args...)
		},
		runCommandInStore: func(dir string, env []string, name string, args ...string) error {
			return runAgentScriptCommandInStore(stdout, stderr, dir, env, name, args...)
		},
		resolveBeadStore: agentScriptCrossStoreBeadEnv,
	}
	executor.runShell = executor.runShellCommand
	return runAgentScriptWithRuntime(scriptPath, stdout, stderr, executor, agentScriptHookBead)
}

func runAgentScriptWithRuntime(
	scriptPath string,
	stdout, stderr io.Writer,
	executor agentScriptExecutor,
	hookBead func(io.Writer) (agentScriptBead, bool, error),
) int {
	script, err := loadAgentScriptDocument(scriptPath)
	if err != nil {
		fmt.Fprintf(stderr, "gc agent-script: %v\n", err) //nolint:errcheck
		return 1
	}
	if err := validateAgentScript(script); err != nil {
		fmt.Fprintf(stderr, "gc agent-script: %v\n", err) //nolint:errcheck
		return 1
	}
	if executor.stdout == nil {
		executor.stdout = stdout
	}
	if executor.stderr == nil {
		executor.stderr = stderr
	}
	if executor.runCommand == nil {
		executor.runCommand = func(name string, args ...string) error {
			return runAgentScriptCommand(stdout, stderr, name, args...)
		}
	}
	if executor.runShell == nil {
		executor.runShell = executor.runShellCommand
	}

	ctx := agentScriptContext{
		rig:   agentScriptRig(),
		alias: agentScriptAlias(),
	}
	for _, action := range script.Setup {
		if exitCode, err := executor.runAction(action, ctx); err != nil {
			fmt.Fprintf(stderr, "gc agent-script: setup: %v\n", err) //nolint:errcheck
			return 1
		} else if exitCode != nil {
			return *exitCode
		}
	}

	bead, hasWork, err := hookBead(stderr)
	if err != nil {
		fmt.Fprintf(stderr, "gc agent-script: hook: %v\n", err) //nolint:errcheck
		return 1
	}
	hookState := "empty"
	if hasWork {
		if err := validateAgentScriptBeadContext(bead); err != nil {
			fmt.Fprintf(stderr, "gc agent-script: hook bead: %v\n", err) //nolint:errcheck
			return 1
		}
		hookState = "has_work"
		ctx.bead = bead
	}
	turn, ok := findAgentScriptTurn(script, hookState)
	if !ok {
		fmt.Fprintf(stderr, "gc agent-script: no turn for hook state %q\n", hookState) //nolint:errcheck
		return 1
	}
	for _, action := range turn.Do {
		exitCode, err := executor.runAction(action, ctx)
		if err != nil {
			fmt.Fprintf(stderr, "gc agent-script: %v\n", err) //nolint:errcheck
			return 1
		}
		if exitCode != nil {
			return *exitCode
		}
	}
	return 0
}

func loadAgentScriptDocument(path string) (agentScriptDocument, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return agentScriptDocument{}, fmt.Errorf("reading %s: %w", path, err)
	}
	var script agentScriptDocument
	if err := yaml.Unmarshal(data, &script); err != nil {
		return agentScriptDocument{}, fmt.Errorf("parsing %s: %w", path, err)
	}
	return script, nil
}

func validateAgentScript(script agentScriptDocument) error {
	switch script.SchemaVersion {
	case "v1":
	case "":
		return errors.New("agent script missing schema_version")
	default:
		return fmt.Errorf("unsupported agent script schema_version %q", script.SchemaVersion)
	}
	for i, action := range script.Setup {
		if err := validateAgentScriptSetupAction(i, action); err != nil {
			return err
		}
	}
	for i, turn := range script.Turns {
		if len(turn.When) != 1 {
			return fmt.Errorf("turn %d when must contain exactly hook", i)
		}
		hook, ok := turn.When["hook"].(string)
		if !ok {
			return fmt.Errorf("turn %d when hook is %T, want string", i, turn.When["hook"])
		}
		switch hook {
		case "has_work", "empty":
		default:
			return fmt.Errorf("turn %d has unsupported hook state %q", i, hook)
		}
	}
	return nil
}

func validateAgentScriptSetupAction(index int, action agentScriptAction) error {
	for name, arg := range action {
		switch name {
		case "bd_claim", "bd_update", "mail_send":
			return fmt.Errorf("setup action %d cannot use %s before hook work exists", index, name)
		case "shell":
			command, ok := arg.(string)
			if ok {
				if placeholder := unsafeAgentScriptShellPlaceholder(command); placeholder != "" {
					return fmt.Errorf("setup action %d shell cannot interpolate %s; read runner identity from GC_SCRIPT_* env", index, placeholder)
				}
			}
		}
	}
	if placeholder := agentScriptActionBeadPlaceholder(action); placeholder != "" {
		return fmt.Errorf("setup action %d cannot reference bead context placeholder %s", index, placeholder)
	}
	return nil
}

func findAgentScriptTurn(script agentScriptDocument, hookState string) (agentScriptTurn, bool) {
	for _, turn := range script.Turns {
		if turn.When["hook"] == hookState {
			return turn, true
		}
	}
	return agentScriptTurn{}, false
}

func (e agentScriptExecutor) runAction(action agentScriptAction, ctx agentScriptContext) (*int, error) {
	if len(action) != 1 {
		return nil, fmt.Errorf("action must have exactly one key, got %d", len(action))
	}
	for name, arg := range action {
		switch name {
		case "bd_claim":
			id, err := agentScriptStringArg(name, arg, ctx)
			if err != nil {
				return nil, err
			}
			args := []string{"update", id, "--claim"}
			if actor := agentScriptClaimActor(); actor != "" {
				args = append(args, "--actor", actor)
			}
			return nil, e.runBDForBead(id, args...)
		case "bd_update":
			args, err := agentScriptBDUpdateArgs(arg, ctx)
			if err != nil {
				return nil, err
			}
			beadID := ""
			if len(args) >= 2 {
				beadID = args[1]
			}
			return nil, e.runBDForBead(beadID, args...)
		case "exit":
			code, err := agentScriptIntArg(name, arg)
			if err != nil {
				return nil, err
			}
			return &code, nil
		case "log":
			msg, err := agentScriptStringArg(name, arg, ctx)
			if err != nil {
				return nil, err
			}
			fmt.Fprintln(e.stdout, msg) //nolint:errcheck
			return nil, nil
		case "mail_send":
			args, err := agentScriptMailSendArgs(arg, ctx)
			if err != nil {
				return nil, err
			}
			return nil, e.runCommand("gc", args...)
		case "shell":
			command, ok := arg.(string)
			if !ok {
				return nil, fmt.Errorf("%s action is %T, want string", name, arg)
			}
			if placeholder := unsafeAgentScriptShellPlaceholder(command); placeholder != "" {
				return nil, fmt.Errorf("shell actions must read bead, rig, and alias data from GC_SCRIPT_* env, not interpolate %s", placeholder)
			}
			env, err := agentScriptShellEnv(os.Environ(), ctx)
			if err != nil {
				return nil, err
			}
			return nil, e.runShell(command, env)
		case "sleep_ms":
			ms, err := agentScriptIntArg(name, arg)
			if err != nil {
				return nil, err
			}
			time.Sleep(time.Duration(ms) * time.Millisecond)
			return nil, nil
		default:
			return nil, fmt.Errorf("unknown action %q", name)
		}
	}
	return nil, errors.New("empty action")
}

func agentScriptBDUpdateArgs(arg any, ctx agentScriptContext) ([]string, error) {
	m, err := agentScriptMapArg("bd_update", arg)
	if err != nil {
		return nil, err
	}
	id, err := agentScriptRequiredStringField(m, "id", ctx)
	if err != nil {
		return nil, err
	}
	args := []string{"update", id}
	assignee, err := agentScriptOptionalStringField(m, "assignee", ctx)
	if err != nil {
		return nil, err
	}
	if assignee != "" {
		args = append(args, "--assignee", assignee)
	}
	status, err := agentScriptOptionalStringField(m, "status", ctx)
	if err != nil {
		return nil, err
	}
	if status != "" {
		args = append(args, "--status", status)
	}
	notes, err := agentScriptOptionalStringField(m, "notes", ctx)
	if err != nil {
		return nil, err
	}
	if notes != "" {
		args = append(args, "--notes", notes)
	}
	if rawMetadata, ok := m["metadata"]; ok {
		metadata, err := agentScriptMapArg("bd_update metadata", rawMetadata)
		if err != nil {
			return nil, err
		}
		keys := make([]string, 0, len(metadata))
		for key := range metadata {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		for _, key := range keys {
			value, err := agentScriptStringValue("metadata "+strconv.Quote(key), metadata[key], ctx)
			if err != nil {
				return nil, err
			}
			args = append(args, "--set-metadata", key+"="+value)
		}
	}
	return args, nil
}

func agentScriptMailSendArgs(arg any, ctx agentScriptContext) ([]string, error) {
	m, err := agentScriptMapArg("mail_send", arg)
	if err != nil {
		return nil, err
	}
	to, err := agentScriptOptionalStringField(m, "to", ctx)
	if err != nil {
		return nil, err
	}
	subject, err := agentScriptOptionalStringField(m, "subject", ctx)
	if err != nil {
		return nil, err
	}
	body, err := agentScriptOptionalStringField(m, "body", ctx)
	if err != nil {
		return nil, err
	}
	args := []string{"mail", "send"}
	if to == "" || to == "all" {
		args = append(args, "--all")
	} else {
		args = append(args, "--to", to)
	}
	if subject != "" {
		args = append(args, "-s", subject)
	}
	if body != "" {
		args = append(args, "-m", body)
	}
	return args, nil
}

func agentScriptMapArg(name string, arg any) (map[string]any, error) {
	switch v := arg.(type) {
	case map[string]any:
		return v, nil
	case agentScriptAction:
		return map[string]any(v), nil
	default:
		return nil, fmt.Errorf("%s action is %T, want mapping", name, arg)
	}
}

func agentScriptRequiredStringField(m map[string]any, key string, ctx agentScriptContext) (string, error) {
	value, err := agentScriptOptionalStringField(m, key, ctx)
	if err != nil {
		return "", err
	}
	if value == "" {
		return "", fmt.Errorf("missing required field %q", key)
	}
	return value, nil
}

func agentScriptOptionalStringField(m map[string]any, key string, ctx agentScriptContext) (string, error) {
	value, ok := m[key]
	if !ok {
		return "", nil
	}
	return agentScriptStringValue("field "+strconv.Quote(key), value, ctx)
}

func agentScriptStringValue(label string, value any, ctx agentScriptContext) (string, error) {
	if value == nil {
		return "", fmt.Errorf("%s cannot be null", label)
	}
	return substituteAgentScriptString(fmt.Sprint(value), ctx)
}

func agentScriptStringArg(name string, arg any, ctx agentScriptContext) (string, error) {
	s, ok := arg.(string)
	if !ok {
		return "", fmt.Errorf("%s action is %T, want string", name, arg)
	}
	return substituteAgentScriptString(s, ctx)
}

func agentScriptIntArg(name string, arg any) (int, error) {
	switch v := arg.(type) {
	case int:
		return v, nil
	case int64:
		return int(v), nil
	case float64:
		return int(v), nil
	case string:
		i, err := strconv.Atoi(v)
		if err != nil {
			return 0, fmt.Errorf("%s action value %q is not an integer", name, v)
		}
		return i, nil
	default:
		return 0, fmt.Errorf("%s action is %T, want integer", name, arg)
	}
}

func substituteAgentScriptString(value string, ctx agentScriptContext) (string, error) {
	if strings.Contains(value, "{rig}") && ctx.rig == "" {
		return "", errors.New("placeholder {rig} resolved empty; set GC_RIG or use a rig-qualified GC_ALIAS, GC_TEMPLATE, or GC_AGENT")
	}
	if strings.Contains(value, "{alias}") && ctx.alias == "" {
		return "", errors.New("placeholder {alias} resolved empty; set GC_ALIAS, GC_SESSION_NAME, or GC_AGENT")
	}
	if key, ok, err := missingAgentScriptMetadataPlaceholder(value, ctx.bead.Metadata); err != nil {
		return "", err
	} else if ok {
		return "", fmt.Errorf("placeholder {bead.metadata.%s} resolved empty; metadata key %q is missing", key, key)
	}
	pairs := []string{
		"{bead.id}", ctx.bead.ID,
		"{bead.title}", ctx.bead.Title,
		"{rig}", ctx.rig,
		"{alias}", ctx.alias,
	}
	keys := make([]string, 0, len(ctx.bead.Metadata))
	for key := range ctx.bead.Metadata {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		pairs = append(pairs, "{bead.metadata."+key+"}", ctx.bead.Metadata[key])
	}
	return strings.NewReplacer(pairs...).Replace(value), nil
}

func missingAgentScriptMetadataPlaceholder(value string, metadata map[string]string) (string, bool, error) {
	const prefix = "{bead.metadata."
	for offset := 0; offset < len(value); {
		idx := strings.Index(value[offset:], prefix)
		if idx < 0 {
			return "", false, nil
		}
		start := offset + idx
		rest := value[start:]
		end := strings.Index(rest, "}")
		if end < 0 {
			return "", false, fmt.Errorf("placeholder %q is missing closing }", prefix)
		}
		key := rest[len(prefix):end]
		if _, ok := metadata[key]; !ok {
			return key, true, nil
		}
		offset = start + end + 1
	}
	return "", false, nil
}

func agentScriptActionBeadPlaceholder(value any) string {
	switch v := value.(type) {
	case string:
		return beadPlaceholderInString(v)
	case map[string]any:
		keys := make([]string, 0, len(v))
		for key := range v {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		for _, key := range keys {
			if placeholder := agentScriptActionBeadPlaceholder(v[key]); placeholder != "" {
				return placeholder
			}
		}
	case agentScriptAction:
		return agentScriptActionBeadPlaceholder(map[string]any(v))
	case []any:
		for _, item := range v {
			if placeholder := agentScriptActionBeadPlaceholder(item); placeholder != "" {
				return placeholder
			}
		}
	case []agentScriptAction:
		for _, item := range v {
			if placeholder := agentScriptActionBeadPlaceholder(item); placeholder != "" {
				return placeholder
			}
		}
	}
	return ""
}

func beadPlaceholderInString(value string) string {
	const beadPrefix = "{bead."
	if idx := strings.Index(value, beadPrefix); idx >= 0 {
		rest := value[idx:]
		if end := strings.Index(rest, "}"); end >= 0 {
			return rest[:end+1]
		}
		return beadPrefix
	}
	return ""
}

func unsafeAgentScriptShellPlaceholder(command string) string {
	for _, exact := range []string{"{rig}", "{alias}"} {
		if strings.Contains(command, exact) {
			return exact
		}
	}
	if placeholder := beadPlaceholderInString(command); placeholder != "" {
		return placeholder
	}
	return ""
}

func validateAgentScriptBeadContext(bead agentScriptBead) error {
	if !validAgentScriptBeadID(bead.ID) {
		return fmt.Errorf("bead id %q contains characters unsafe for shell paths or refs", bead.ID)
	}
	return validateAgentScriptShellMetadataKeys(bead.Metadata)
}

func validAgentScriptBeadID(id string) bool {
	if id == "" {
		return false
	}
	for _, r := range id {
		if unicode.IsLetter(r) || unicode.IsDigit(r) || r == '-' {
			continue
		}
		return false
	}
	return true
}

func validateAgentScriptShellMetadataKeys(metadata map[string]string) error {
	keys := make([]string, 0, len(metadata))
	for key := range metadata {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	envKeys := make(map[string]string, len(keys))
	for _, key := range keys {
		segment := envKeySegment(key)
		if segment == "" {
			return fmt.Errorf("metadata key %q cannot be exposed to shell env", key)
		}
		envKey := "GC_SCRIPT_BEAD_METADATA_" + segment
		if previous, ok := envKeys[envKey]; ok {
			return fmt.Errorf("metadata keys %q and %q collide as %s", previous, key, envKey)
		}
		envKeys[envKey] = key
	}
	return nil
}

func agentScriptShellEnv(base []string, ctx agentScriptContext) ([]string, error) {
	env := removeEnvKey(append([]string(nil), base...), "GIT_TERMINAL_PROMPT")
	env = removeEnvKeyPrefix(env, "GC_SCRIPT_")
	env = append(env,
		"GIT_TERMINAL_PROMPT=0",
		"GC_SCRIPT_BEAD_ID="+ctx.bead.ID,
		"GC_SCRIPT_BEAD_TITLE="+ctx.bead.Title,
		"GC_SCRIPT_RIG="+ctx.rig,
		"GC_SCRIPT_ALIAS="+ctx.alias,
	)
	keys := make([]string, 0, len(ctx.bead.Metadata))
	for key := range ctx.bead.Metadata {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	if err := validateAgentScriptShellMetadataKeys(ctx.bead.Metadata); err != nil {
		return nil, err
	}
	for _, key := range keys {
		segment := envKeySegment(key)
		envKey := "GC_SCRIPT_BEAD_METADATA_" + segment
		env = append(env, envKey+"="+ctx.bead.Metadata[key])
	}
	return env, nil
}

func removeEnvKeyPrefix(environ []string, prefix string) []string {
	out := make([]string, 0, len(environ))
	for _, entry := range environ {
		if !strings.HasPrefix(entry, prefix) {
			out = append(out, entry)
		}
	}
	return out
}

func envKeySegment(value string) string {
	var b strings.Builder
	lastUnderscore := false
	for _, r := range value {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			b.WriteRune(unicode.ToUpper(r))
			lastUnderscore = false
			continue
		}
		if !lastUnderscore {
			b.WriteByte('_')
			lastUnderscore = true
		}
	}
	return strings.Trim(b.String(), "_")
}

func (e agentScriptExecutor) runShellCommand(command string, env []string) error {
	ctx, cancel := context.WithTimeout(context.Background(), agentScriptActionTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "sh", "-c", command)
	cmd.WaitDelay = 2 * time.Second
	prepareProviderOpCommand(cmd)
	cmd.Env = workQueryEnvForDir(env, "")
	cmd.Stdout = e.stdout
	cmd.Stderr = e.stderr
	if err := cmd.Run(); err != nil {
		if ctx.Err() == context.DeadlineExceeded {
			return fmt.Errorf("shell action timed out after %s", agentScriptActionTimeout)
		}
		return fmt.Errorf("shell action failed: %w", err)
	}
	return nil
}

func runAgentScriptCommand(stdout, stderr io.Writer, name string, args ...string) error {
	return runAgentScriptCommandInStore(stdout, stderr, "", nil, name, args...)
}

// runAgentScriptCommandInStore runs a command like runAgentScriptCommand but, when
// dir/env are supplied, points the subprocess at a specific store so a
// cross-store claim/update lands in the bead's own store (vp-kvp stage iii). An
// empty dir / nil env preserves the inherited working dir and environment.
func runAgentScriptCommandInStore(stdout, stderr io.Writer, dir string, env []string, name string, args ...string) error {
	ctx, cancel := context.WithTimeout(context.Background(), agentScriptActionTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.WaitDelay = 2 * time.Second
	prepareProviderOpCommand(cmd)
	if dir != "" {
		cmd.Dir = dir
	}
	if env != nil {
		cmd.Env = workQueryEnvForDir(env, dir)
	}
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	if err := cmd.Run(); err != nil {
		if ctx.Err() == context.DeadlineExceeded {
			return fmt.Errorf("%s %s: timed out after %s", name, strings.Join(args, " "), agentScriptActionTimeout)
		}
		return fmt.Errorf("%s %s: %w", name, strings.Join(args, " "), err)
	}
	return nil
}

type agentScriptHookRunner func(args []string, inject bool, hookFormat string, stdout, stderr io.Writer) int

func agentScriptHookBead(stderr io.Writer) (agentScriptBead, bool, error) {
	return agentScriptHookBeadWithRunner(stderr, cmdHookWithFormat)
}

func agentScriptHookBeadWithRunner(stderr io.Writer, runHook agentScriptHookRunner) (agentScriptBead, bool, error) {
	var hookOut, hookErr bytes.Buffer
	code := runHook(nil, false, "", &hookOut, &hookErr)
	if hookErr.Len() > 0 {
		_, _ = stderr.Write(hookErr.Bytes())
	}
	output := strings.TrimSpace(hookOut.String())
	hasWork := workQueryHasReadyWork(output)
	if code != 0 {
		if !hasWork && agentScriptHookExitIsNoWork(output, hookErr.String()) {
			return agentScriptBead{}, false, nil
		}
		return agentScriptBead{}, false, errors.New("gc hook failed")
	}
	if !hasWork {
		return agentScriptBead{}, false, nil
	}
	var beads []agentScriptBead
	if err := json.Unmarshal([]byte(output), &beads); err != nil {
		var bead agentScriptBead
		if errObj := json.Unmarshal([]byte(output), &bead); errObj != nil {
			return agentScriptBead{}, false, fmt.Errorf("decoding hook output: %w", errors.Join(err, errObj))
		}
		beads = []agentScriptBead{bead}
	}
	if len(beads) == 0 {
		return agentScriptBead{}, false, nil
	}
	return beads[0], true, nil
}

func agentScriptHookExitIsNoWork(output, stderr string) bool {
	if workQueryHasReadyWork(output) {
		return false
	}
	for _, line := range strings.Split(stderr, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if !strings.Contains(strings.ToLower(line), "warning") {
			return false
		}
	}
	return true
}

func agentScriptClaimActor() string {
	for _, key := range []string{"GC_SESSION_NAME", "GC_AGENT", "GC_ALIAS", "BEADS_ACTOR"} {
		if value := strings.TrimSpace(os.Getenv(key)); value != "" {
			return value
		}
	}
	return ""
}

func agentScriptRig() string {
	if rig := strings.TrimSpace(os.Getenv("GC_RIG")); rig != "" {
		return rig
	}
	for _, key := range []string{"GC_ALIAS", "GC_TEMPLATE", "GC_AGENT"} {
		value := strings.TrimSpace(os.Getenv(key))
		if before, _, ok := strings.Cut(value, "/"); ok && before != "" {
			return before
		}
	}
	return ""
}

func agentScriptAlias() string {
	for _, key := range []string{"GC_ALIAS", "GC_SESSION_NAME", "GC_AGENT"} {
		if value := strings.TrimSpace(os.Getenv(key)); value != "" {
			return value
		}
	}
	return ""
}
