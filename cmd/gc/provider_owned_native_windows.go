//go:build windows

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// This file is gc's native form of the provider-owned arm of the bundled
// gc-beads-bd provider: examples/bd/assets/scripts/gc-beads-bd.sh from its
// GC_BEADS_PROVIDER_OWNED=1 dispatch down. Windows cannot exec that POSIX
// script, so on Windows this runs in its place. The script stays the
// contract: each function names the one it mirrors, a change to either
// belongs in both, and TestNativeBdProviderMirrorsTheScript fails when the
// script's side changes.

// builtinProviderOwnedOp returns gc's native form of the bundled gc-beads-bd
// provider when script is this city's entrypoint for it, and nil for any
// other script.
//
// Only the city's own entrypoints qualify: a script configured anywhere else
// is a custom provider, and runs as itself.
func builtinProviderOwnedOp(script string, environ []string) func(context.Context, []string, io.Writer, []string) error {
	cityPath := nativeEnv(environ).get("GC_CITY_PATH")
	if cityPath == "" {
		return nil
	}
	if !samePath(script, gcBeadsBdScriptPath(cityPath)) && !samePath(script, legacySystemPacksGcBeadsBdScriptPath(cityPath)) {
		return nil
	}
	return runNativeBdProviderOwnedOp
}

// runNativeBdProviderOwnedOp runs one op as gc-beads-bd.sh does, with environ
// as the script's environment and stderr as its standard error.
//
// A failure bd reports comes back as bd's own *exec.ExitError, exactly the
// status the script would have exited with. A refusal the script makes before
// any bd runs comes back as a nativeProviderExit carrying the script's status.
func runNativeBdProviderOwnedOp(ctx context.Context, environ []string, stderr io.Writer, args []string) error {
	processes := &providerOpProcessSet{}
	p := &nativeBdProvider{ctx: ctx, env: nativeEnv(environ), stderr: stderr, processes: processes}
	defer func() {
		processes.finish(ctx.Err() != nil)
	}()
	if p.env.get("GC_DOLT") == "skip" {
		return nativeProviderExit(2)
	}
	op := ""
	var rest []string
	if len(args) > 0 {
		op, rest = args[0], args[1:]
	}
	if p.env.get("GC_CITY_PATH") == "" {
		return p.die("GC_CITY_PATH not set")
	}
	if p.env.get("GC_BEADS_PROVIDER_OWNED") != "1" {
		return p.die("gc-beads-bd: only provider-owned operations run natively; the gc-managed Dolt lifecycle is unavailable on this platform")
	}
	switch op {
	case "init":
		return p.init(rest)
	case "start", "ensure-ready", "health", "probe", "recover", "stop", "shutdown":
		return p.lifecycle(op)
	default:
		return nativeProviderExit(2)
	}
}

// nativeProviderExit is the status gc-beads-bd.sh exits with when it refuses
// an op itself: 1 for die, 2 for "not needed", 127 for a bd it cannot find.
// Its text is what that exit reads as.
type nativeProviderExit int

func (e nativeProviderExit) Error() string { return "exit status " + strconv.Itoa(int(e)) }

type nativeBdProvider struct {
	ctx       context.Context
	env       nativeEnv
	stderr    io.Writer
	processes *providerOpProcessSet
}

// die mirrors the script's die.
func (p *nativeBdProvider) die(msg string) error {
	fmt.Fprintln(p.stderr, msg) //nolint:errcheck // best-effort diagnostics, as the script's echo
	return nativeProviderExit(1)
}

// init mirrors op_provider_owned_init.
func (p *nativeBdProvider) init(args []string) error {
	arg := func(i int) string {
		if i < len(args) {
			return args[i]
		}
		return ""
	}
	dir, prefix, database := arg(0), arg(1), arg(2)
	if dir == "" || prefix == "" {
		return p.die("usage: gc-beads-bd init <dir> <prefix> [dolt_database]")
	}
	transport, target := p.env.get("GC_BEADS_TRANSPORT"), p.env.get("GC_BEADS_TARGET")
	var bdArgs []string
	switch transport + ":" + target {
	case "direct:local", "direct:external":
		bdArgs = []string{"init", "--init-if-missing", "--quiet", "--server"}
		if target == "external" {
			bdArgs = append(bdArgs, "--external")
			if socket := p.env.get("BEADS_DOLT_SERVER_SOCKET"); socket != "" {
				bdArgs = append(bdArgs, "--server-socket", socket)
			} else {
				host, port := p.env.get("GC_DOLT_HOST"), p.env.get("GC_DOLT_PORT")
				if host == "" || port == "" {
					return p.die("direct-external init requires GC_DOLT_HOST and GC_DOLT_PORT")
				}
				bdArgs = append(bdArgs, "--server-host", host, "--server-port", port)
			}
		}
	case "proxied:local", "proxied:external":
		bdArgs = []string{"init", "--init-if-missing", "--quiet", "--proxied-server", "--proxied-server-idle-timeout", "0"}
		if target == "external" {
			if socket := p.env.get("GC_BEADS_PROXY_EXTERNAL_SOCKET"); socket != "" {
				bdArgs = append(bdArgs, "--proxied-server-external-socket-path", socket)
			} else {
				host, port := p.env.get("GC_BEADS_PROXY_EXTERNAL_HOST"), p.env.get("GC_BEADS_PROXY_EXTERNAL_PORT")
				if host == "" || port == "" {
					return p.die("proxied-external init requires GC_BEADS_PROXY_EXTERNAL_HOST and GC_BEADS_PROXY_EXTERNAL_PORT")
				}
				bdArgs = append(bdArgs, "--proxied-server-external-host", host, "--proxied-server-external-port", port)
			}
		}
	default:
		return p.die(fmt.Sprintf("invalid provider-owned beads transport/target: %s/%s", transport, target))
	}
	bdArgs = append(bdArgs, "-p", prefix, "--skip-hooks", "--skip-agents")
	if database != "" {
		bdArgs = append(bdArgs, "--database", database)
	}
	bdArgs = append(bdArgs, dir)
	// The script's `GC_BEADS_PROVIDER_INIT=1 run_provider_owned_bd`: the marker
	// belongs to this one bd run.
	initRun := &nativeBdProvider{ctx: p.ctx, env: p.env.with("GC_BEADS_PROVIDER_INIT", "1"), stderr: p.stderr, processes: p.processes}
	return initRun.runBd(dir, nil, p.stderr, bdArgs...)
}

// lifecycle mirrors op_provider_owned_lifecycle.
func (p *nativeBdProvider) lifecycle(op string) error {
	beadsDir := p.env.get("BEADS_DIR")
	if beadsDir == "" {
		return p.die("provider-owned beads operation requires BEADS_DIR")
	}
	dir := posixDirname(beadsDir)
	if !isDirectory(dir) || !isDirectory(filepath.Join(dir, ".beads")) {
		if op == "stop" || op == "shutdown" {
			fmt.Fprintf(p.stderr, "scope %s has no beads store; nothing to retire\n", dir) //nolint:errcheck // informational, as in the script
			return nil
		}
	}
	transport, err := p.transport(dir)
	if err != nil {
		return err
	}
	p.env = p.env.with("GC_BEADS_TRANSPORT", transport)
	switch transport {
	case "direct":
		local := p.scopeIsLocal(dir)
		switch op {
		case "start", "ensure-ready", "health", "probe":
			return p.runBd(dir, nil, p.stderr, "ping")
		case "recover":
			if local {
				if err := p.retireLocalDolt(dir); err != nil {
					return err
				}
			}
			return p.runBd(dir, nil, p.stderr, "ping")
		case "stop", "shutdown":
			if local {
				return p.retireLocalDolt(dir)
			}
			return nil
		}
	case "proxied":
		switch op {
		case "start", "ensure-ready", "health", "probe":
			return p.runBd(dir, nil, p.stderr, "ping")
		case "recover":
			if p.sharesCityProxyRoot(dir) {
				fmt.Fprintf(p.stderr, "scope %s shares the city proxy root; recovering by ping only\n", dir) //nolint:errcheck // informational, as in the script
			} else if err := p.retireLocalDolt(dir); err != nil {
				return err
			}
			return p.runBd(dir, nil, p.stderr, "ping")
		case "stop", "shutdown":
			return p.retireLocalDolt(dir)
		}
	}
	return nativeProviderExit(2)
}

// transport mirrors provider_owned_transport.
func (p *nativeBdProvider) transport(dir string) (string, error) {
	switch transport := p.env.get("GC_BEADS_TRANSPORT"); transport {
	case "direct", "proxied":
		return transport, nil
	case "":
		if p.scopeIsProxied(dir) {
			return "proxied", nil
		}
		return "direct", nil
	default:
		return "", p.die("invalid provider-owned beads transport: " + transport)
	}
}

// providerOwnedBdScrubbedEnv is what run_provider_owned_bd unsets for every bd
// run except a direct-external init: a completed binding owns its endpoint.
var providerOwnedBdScrubbedEnv = []string{
	"GC_DOLT", "GC_DOLT_HOST", "GC_DOLT_PORT", "GC_DOLT_USER", "GC_DOLT_PASSWORD",
	"GC_DOLT_DATA_DIR", "GC_DOLT_LOG_FILE", "GC_DOLT_STATE_FILE", "GC_DOLT_PID_FILE", "GC_DOLT_LOCK_FILE", "GC_DOLT_CONFIG_FILE",
	"BEADS_DOLT_SERVER_HOST", "BEADS_DOLT_SERVER_PORT", "BEADS_DOLT_SERVER_SOCKET",
	"BEADS_DOLT_AUTO_START",
}

// runBd mirrors run_provider_owned_bd: bd runs from the scope, against the
// scope's store. Its standard output goes to stdout and its standard error to
// stderr; a nil writer discards, as gc discards the script's standard output.
func (p *nativeBdProvider) runBd(dir string, stdout, stderr io.Writer, args ...string) error {
	if info, err := os.Stat(dir); err != nil || !info.IsDir() {
		reason := "Not a directory"
		if errors.Is(err, fs.ErrNotExist) {
			reason = "No such file or directory"
		} else if err != nil {
			reason = err.Error()
		}
		fmt.Fprintf(stderr, "cd: %s: %s\n", dir, reason) //nolint:errcheck // best-effort diagnostics, as the shell's
		return nativeProviderExit(1)
	}
	env := p.env.with("BEADS_DIR", filepath.Join(dir, ".beads"))
	if env.get("GC_BEADS_PROVIDER_INIT") != "1" || env.get("GC_BEADS_TARGET") == "local" {
		env = env.without(providerOwnedBdScrubbedEnv...)
	}
	if env.get("GC_BEADS_TRANSPORT") == "proxied" {
		env = env.with("BEADS_DOLT_PROXIED_SERVER", "1")
	} else {
		env = env.without("BEADS_DOLT_PROXIED_SERVER")
	}
	traceNativeBdArgv(env, dir, args)
	bin, err := env.lookBd(dir)
	if err != nil {
		fmt.Fprintln(stderr, err) //nolint:errcheck // best-effort diagnostics, as the shell's
		return nativeProviderExit(127)
	}
	cmd := exec.CommandContext(p.ctx, bin, args...)
	cmd.Dir = dir
	cmd.Env = env
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	cmd.WaitDelay = 2 * time.Second
	return p.processes.run(cmd)
}

// retireLocalDolt mirrors provider_owned_retire_local_dolt. gc stop must be
// re-runnable, so "there was nothing to stop" is success.
func (p *nativeBdProvider) retireLocalDolt(dir string) error {
	var out bytes.Buffer
	err := p.runBd(dir, &out, &out, "dolt", "stop")
	if err == nil {
		return nil
	}
	if p.ctx.Err() != nil {
		// The script dies with its bd here; it does not read a killed
		// child's partial output as an answer.
		return err
	}
	text := strings.TrimRight(out.String(), "\n")
	if strings.Contains(text, "not running") || strings.Contains(text, "no server") {
		return nil
	}
	fmt.Fprintln(p.stderr, text) //nolint:errcheck // best-effort diagnostics, as the script's printf
	return err
}

// scopeBackendIsDolt mirrors scope_backend_is_dolt.
func (p *nativeBdProvider) scopeBackendIsDolt(scope string) bool {
	metadata := filepath.Join(scope, ".beads", "metadata.json")
	if backend := normalizeDoltModeText(readMetadataStringField(metadata, "backend")); backend != "" {
		return backend == "dolt"
	}
	switch normalizeDoltModeText(readMetadataStringField(metadata, "database")) {
	case "doltlite":
		return false
	case "dolt":
		return true
	}
	configured := p.env.get("GC_BEADS_BACKEND")
	if configured == "" {
		configured = p.env.get("BEADS_BACKEND")
	}
	if configured == "" {
		configured = "dolt"
	}
	switch normalizeDoltModeText(configured) {
	case "", "dolt":
		return true
	}
	return false
}

var configDoltModeKey = regexp.MustCompile(`^[[:space:]]*dolt\.mode:[[:space:]]*`)

// scopeIsProxied mirrors scope_is_proxied.
func (p *nativeBdProvider) scopeIsProxied(scope string) bool {
	if !p.scopeBackendIsDolt(scope) {
		return false
	}
	metadata := filepath.Join(scope, ".beads", "metadata.json")
	if mode := normalizeDoltModeText(readMetadataStringField(metadata, "dolt_mode")); mode != "" {
		return mode == "proxied-server"
	}
	config := filepath.Join(scope, ".beads", "config.yaml")
	if isRegularFile(config) {
		if value, ok := firstLineAfter(config, configDoltModeKey); ok && normalizeDoltModeText(value) != "" {
			return false
		}
	}
	return p.env.get("BEADS_DOLT_PROXIED_SERVER") == "1"
}

var (
	sidecarExternalKey     = regexp.MustCompile(`"external"[[:space:]]*:`)
	metadataServerBinding  = regexp.MustCompile(`"dolt_server_(host|socket)"[[:space:]]*:[[:space:]]*"[^"]`)
	originManagedCity      = regexp.MustCompile(`^[[:space:]]*gc\.endpoint_origin:[[:space:]]*managed_city[[:space:]]*$`)
	originCityCanonical    = regexp.MustCompile(`^[[:space:]]*gc\.endpoint_origin:[[:space:]]*city_canonical[[:space:]]*$`)
	originExplicit         = regexp.MustCompile(`^[[:space:]]*gc\.endpoint_origin:[[:space:]]*explicit[[:space:]]*$`)
	doltAutoStartTrue      = regexp.MustCompile(`^[[:space:]]*dolt\.auto-start:[[:space:]]*true[[:space:]]*$`)
	configDoltEndpointKeys = regexp.MustCompile(`^[[:space:]]*dolt\.(host|port|socket):`)
)

// scopeIsLocal mirrors provider_owned_scope_is_local.
func (p *nativeBdProvider) scopeIsLocal(dir string) bool {
	switch p.env.get("GC_BEADS_TARGET") {
	case "local":
		return true
	case "external":
		return false
	}
	config := filepath.Join(dir, ".beads", "config.yaml")
	if !isRegularFile(config) {
		return false
	}
	if p.scopeIsProxied(dir) {
		sidecar := filepath.Join(dir, ".beads", "proxied_server_client_info.json")
		return !(isRegularFile(sidecar) && fileHasLineMatching(sidecar, sidecarExternalKey))
	}
	metadata := filepath.Join(dir, ".beads", "metadata.json")
	if isRegularFile(metadata) && fileHasLineMatching(metadata, metadataServerBinding) {
		return false
	}
	if fileHasLineMatching(config, originManagedCity) {
		return true
	}
	if fileHasLineMatching(config, originCityCanonical) {
		return fileHasLineMatching(config, doltAutoStartTrue)
	}
	if fileHasLineMatching(config, originExplicit) || fileHasLineMatching(config, configDoltEndpointKeys) {
		return false
	}
	return true
}

// sharesCityProxyRoot mirrors provider_owned_scope_shares_city_proxy_root.
func (p *nativeBdProvider) sharesCityProxyRoot(dir string) bool {
	cityPath := p.env.get("GC_CITY_PATH")
	if cityPath == "" {
		return false
	}
	scopeDir, ok := physicalDirectory(dir)
	if !ok {
		return false
	}
	cityDir, ok := physicalDirectory(cityPath)
	if !ok || scopeDir == cityDir {
		return false
	}
	scopeRoot := providerOwnedProxyRoot(dir)
	if scopeRoot == "" {
		return false
	}
	cityRoot := providerOwnedProxyRoot(cityPath)
	return cityRoot != "" && scopeRoot == cityRoot
}

// providerOwnedProxyRoot mirrors provider_owned_proxy_root, but reads
// root_path as the JSON it is. bd writes the sidecar with encoding/json, so a
// Windows root arrives with every backslash escaped, and the script's sed
// would keep them doubled.
func providerOwnedProxyRoot(dir string) string {
	sidecar := filepath.Join(dir, ".beads", "proxied_server_client_info.json")
	if !isRegularFile(sidecar) {
		return ""
	}
	data, err := os.ReadFile(sidecar)
	if err != nil {
		return ""
	}
	var binding struct {
		RootPath string `json:"root_path"`
	}
	if json.Unmarshal(data, &binding) != nil || binding.RootPath == "" {
		return ""
	}
	root := binding.RootPath
	if !filepath.IsAbs(root) {
		// bd resolves a relative root_path against BEADS_DIR, not the scope.
		// Joined without cleaning, as the script joins it: resolving ".."
		// is physicalDirectory's job.
		sep := string(filepath.Separator)
		root = dir + sep + ".beads" + sep + root
	}
	if physical, ok := physicalDirectory(root); ok {
		return physical
	}
	return root
}

// physicalDirectory is `cd path && pwd -P`: the absolute path of an existing
// directory with every link resolved.
func physicalDirectory(path string) (string, bool) {
	if !isDirectory(path) {
		return "", false
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", false
	}
	resolved, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return "", false
	}
	return resolved, true
}

// readMetadataStringField mirrors read_metadata_string_field's jq branch,
// `jq -r '.[$key] // empty'`: the top-level value, empty when it is missing,
// null or false, and nothing at all for a file jq cannot parse.
func readMetadataStringField(path, key string) string {
	if !isRegularFile(path) {
		return ""
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	var fields map[string]json.RawMessage
	if json.Unmarshal(data, &fields) != nil {
		return ""
	}
	raw, ok := fields[key]
	if !ok {
		return ""
	}
	var value any
	if json.Unmarshal(raw, &value) != nil {
		return ""
	}
	switch v := value.(type) {
	case nil:
		return ""
	case bool:
		if !v {
			return ""
		}
		return "true"
	case string:
		return v
	default:
		return strings.TrimSpace(string(raw))
	}
}

var yamlCommentTail = regexp.MustCompile(`[[:space:]]+#.*$`)

// normalizeDoltModeText mirrors normalize_dolt_mode. sed works a line at a
// time, trimming each line and dropping a " # comment" tail from it; command
// substitution drops the trailing newlines; tr lowercases what is left.
func normalizeDoltModeText(value string) string {
	lines := strings.Split(value, "\n")
	for i, line := range lines {
		line = trimPOSIXSpace(line)
		line = yamlCommentTail.ReplaceAllString(line, "")
		lines[i] = trimPOSIXSpace(line)
	}
	return strings.ToLower(strings.TrimRight(strings.Join(lines, "\n"), "\n"))
}

// trimPOSIXSpace trims the characters of the POSIX [[:space:]] class.
func trimPOSIXSpace(s string) string {
	return strings.Trim(s, " \t\n\v\f\r")
}

// fileHasLineMatching is `grep -Eq pattern path`: grep matches one line at a
// time, so a pattern never spans a line break.
func fileHasLineMatching(path string, pattern *regexp.Regexp) bool {
	data, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	for _, line := range strings.Split(string(data), "\n") {
		if pattern.MatchString(line) {
			return true
		}
	}
	return false
}

// firstLineAfter is `sed -n 's/<prefix>//p' path | head -1`: what follows
// prefix on the first line that starts with it.
func firstLineAfter(path string, prefix *regexp.Regexp) (string, bool) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", false
	}
	for _, line := range strings.Split(string(data), "\n") {
		if loc := prefix.FindStringIndex(line); loc != nil {
			return line[loc[1]:], true
		}
	}
	return "", false
}

// isRegularFile is `[ -f path ]`.
func isRegularFile(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.Mode().IsRegular()
}

// isDirectory is `[ -d path ]`.
func isDirectory(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.IsDir()
}

// posixDirname is dirname(1), for which a trailing separator does not name an
// empty last element.
func posixDirname(path string) string {
	volume := len(filepath.VolumeName(path))
	for len(path) > volume+1 && os.IsPathSeparator(path[len(path)-1]) {
		path = path[:len(path)-1]
	}
	return filepath.Dir(path)
}

// traceNativeBdArgv mirrors trace_bd_argv: one line-format record per bd run
// when GC_BD_TRACE names a file and the JSONL trace has not claimed tracing.
// The pid is gc's, since natively no script process exists.
func traceNativeBdArgv(env nativeEnv, dir string, args []string) {
	if env.get("GC_BD_TRACE_JSON") != "" {
		return
	}
	path := env.get("GC_BD_TRACE")
	if path == "" {
		return
	}
	text := strings.Join(args, " ")
	if strings.Contains(text, "\n") {
		text = strings.NewReplacer("\n", " ", "\r", " ").Replace(text)
	}
	subcommand := "unknown"
	if len(args) > 0 && args[0] != "" {
		subcommand = args[0]
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return
	}
	defer f.Close() //nolint:errcheck // best-effort trace log
	_, _ = fmt.Fprintf(f, "%s source=provider-script subcommand=%s pid=%d dir=%s args=%s\n",
		time.Now().UTC().Format("2006-01-02T15:04:05Z"), subcommand, os.Getpid(), dir, text)
}

// nativeEnv is a child process's environment as Windows reads it: names
// compare without case, and the last definition of a name wins.
type nativeEnv []string

func nativeEnvName(entry string) string {
	// A Windows per-drive entry such as "=C:=C:\dir" is named "=C:".
	start := 0
	if strings.HasPrefix(entry, "=") {
		start = 1
	}
	if i := strings.IndexByte(entry[start:], '='); i >= 0 {
		return entry[:start+i]
	}
	return entry
}

func (e nativeEnv) get(name string) string {
	for i := len(e) - 1; i >= 0; i-- {
		key := nativeEnvName(e[i])
		if strings.EqualFold(key, name) {
			if len(key) < len(e[i]) {
				return e[i][len(key)+1:]
			}
			return ""
		}
	}
	return ""
}

func (e nativeEnv) without(names ...string) nativeEnv {
	out := make(nativeEnv, 0, len(e))
	for _, entry := range e {
		key := nativeEnvName(entry)
		dropped := false
		for _, name := range names {
			if strings.EqualFold(key, name) {
				dropped = true
				break
			}
		}
		if !dropped {
			out = append(out, entry)
		}
	}
	return out
}

func (e nativeEnv) with(name, value string) nativeEnv {
	return append(e.without(name), name+"="+value)
}

// lookBd resolves "${BD_BIN:-bd}" as the script's shell would: a name with a
// separator is a path from the scope, and a bare name is searched on this
// environment's PATH rather than gc's own. Relative PATH entries are skipped,
// as os/exec skips them.
func (e nativeEnv) lookBd(dir string) (string, error) {
	name := e.get("BD_BIN")
	if name == "" {
		name = "bd"
	}
	if strings.ContainsRune(name, '/') || strings.ContainsRune(name, filepath.Separator) {
		path := name
		if !filepath.IsAbs(path) {
			path = filepath.Join(dir, path)
		}
		return exec.LookPath(path)
	}
	for _, entry := range filepath.SplitList(e.get("PATH")) {
		if entry == "" || !filepath.IsAbs(entry) {
			continue
		}
		if found, err := exec.LookPath(filepath.Join(entry, name)); err == nil {
			return found, nil
		}
	}
	return "", fmt.Errorf("%s: executable file not found in PATH", name)
}
