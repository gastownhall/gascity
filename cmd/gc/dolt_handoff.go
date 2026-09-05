package main

// The handoff commands in this file are deliberately hidden from the normal
// gc surface.  They form the small, typed protocol consumed by bd when a
// legacy GC-managed Dolt process is transferred to the beads owner.  Keep all
// process discovery and signaling behind the existing managed-Dolt helpers;
// this adapter must never guess at, or signal, an unknown PID.

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/gastownhall/gascity/internal/beads/contract"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/fsys"
)

const handoffProtocolSchemaVersion = 1

type handoffProtocolEndpoint struct {
	Host   string `json:"host"`
	Port   int    `json:"port"`
	Socket string `json:"socket"`
}

type handoffProtocolIdentity struct {
	CityRoot       string                  `json:"city_root"`
	ScopeRoot      string                  `json:"scope_root"`
	Database       string                  `json:"database"`
	Workspace      string                  `json:"workspace"`
	Endpoint       handoffProtocolEndpoint `json:"endpoint"`
	DataDir        string                  `json:"data_dir"`
	ConfigFile     string                  `json:"config_file"`
	PID            int                     `json:"pid"`
	StartIdentity  string                  `json:"start_identity"`
	StartTimeTicks int64                   `json:"start_time_ticks"`
	PortHolderPID  int                     `json:"port_holder_pid"`
}

type handoffProtocolResponse struct {
	SchemaVersion int                     `json:"schema_version"`
	Operation     string                  `json:"operation"`
	Result        string                  `json:"result"`
	Owner         string                  `json:"owner"`
	Mutates       bool                    `json:"mutates"`
	Identity      handoffProtocolIdentity `json:"identity"`
	IdentityToken string                  `json:"identity_token"`
	ErrorCode     string                  `json:"error_code"`
}

type handoffProtocolError struct {
	Code string
	Err  error
}

func (e *handoffProtocolError) Error() string {
	if e == nil || e.Err == nil {
		return e.Code
	}
	return e.Err.Error()
}

func (e *handoffProtocolError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Err
}

func handoffErr(code string, err error) error {
	return &handoffProtocolError{Code: code, Err: err}
}

func handoffErrorCode(err error) string {
	var coded *handoffProtocolError
	if errors.As(err, &coded) && strings.TrimSpace(coded.Code) != "" {
		return coded.Code
	}
	return "provider_unavailable"
}

func newDoltHandoffCommands(stdout, _ io.Writer) []*cobra.Command {
	var (
		cityRoot, scopeRoot, database, workspace string
		host, socket, identityToken              string
		port                                     int
		jsonOutput                               bool
	)
	newCommand := func(operation string, mutates bool) *cobra.Command {
		cmd := &cobra.Command{
			Use:    operation,
			Short:  "Internal ownership handoff protocol",
			Hidden: true,
			Annotations: map[string]string{
				jsonRawProtocolAnnotation: "handoff-v1",
			},
			Args: cobra.NoArgs,
			RunE: func(_ *cobra.Command, _ []string) error {
				request := handoffProtocolRequest{
					CityRoot: cityRoot, ScopeRoot: scopeRoot, Database: database, Workspace: workspace,
					Endpoint: handoffProtocolEndpoint{Host: host, Port: port, Socket: socket},
				}
				response, err := runHandoffProtocol(operation, mutates, request, identityToken)
				if writeErr := json.NewEncoder(stdout).Encode(response); writeErr != nil {
					return writeErr
				}
				if err != nil {
					return errExit
				}
				return nil
			},
		}
		cmd.Flags().StringVar(&cityRoot, "city", "", "canonical city root")
		cmd.Flags().StringVar(&scopeRoot, "scope-root", "", "canonical Dolt scope root")
		cmd.Flags().StringVar(&database, "database", "", "database identity")
		cmd.Flags().StringVar(&workspace, "workspace", "", "workspace identity")
		cmd.Flags().StringVar(&host, "host", "", "loopback Dolt host")
		cmd.Flags().IntVar(&port, "port", 0, "Dolt listener port")
		cmd.Flags().StringVar(&socket, "socket", "", "Unix socket endpoint")
		cmd.Flags().StringVar(&identityToken, "identity-token", "", "inspect identity token")
		cmd.Flags().BoolVar(&jsonOutput, "json", false, "emit the machine-readable handoff response")
		return cmd
	}
	return []*cobra.Command{newCommand("handoff-inspect", false), newCommand("handoff-stop", true)}
}

type handoffProtocolRequest struct {
	CityRoot  string
	ScopeRoot string
	Database  string
	Workspace string
	Endpoint  handoffProtocolEndpoint
}

func runHandoffProtocol(operation string, mutates bool, request handoffProtocolRequest, token string) (handoffProtocolResponse, error) {
	response := handoffProtocolResponse{SchemaVersion: handoffProtocolSchemaVersion, Operation: operation, Result: "refused", Owner: "legacy-gc", Mutates: false}
	if err := validateHandoffProtocolRequest(request); err != nil {
		response.ErrorCode = handoffErrorCode(err)
		return response, err
	}
	lock, layout, err := openHandoffLifecycleLock(request.CityRoot)
	if err != nil {
		response.ErrorCode = handoffErrorCode(err)
		return response, err
	}
	locked, err := tryManagedDoltLifecycleLock(lock)
	if err != nil {
		releaseManagedDoltLifecycleLock(lock)
		err = handoffErr("lifecycle_busy", err)
		response.ErrorCode = handoffErrorCode(err)
		return response, err
	}
	if !locked {
		releaseManagedDoltLifecycleLock(lock)
		err = handoffErr("lifecycle_busy", errors.New("managed dolt lifecycle is busy"))
		response.ErrorCode = handoffErrorCode(err)
		return response, err
	}
	defer releaseManagedDoltLifecycleLock(lock)

	identity, err := inspectHandoffIdentity(request, layout)
	if err != nil {
		response.ErrorCode = handoffErrorCode(err)
		return response, err
	}
	identityToken := handoffIdentityToken(identity)
	if operation == "handoff-inspect" {
		response.Result = "eligible"
		response.Mutates = false
		response.Identity = identity
		response.IdentityToken = identityToken
		return response, nil
	}
	if operation != "handoff-stop" || !mutates {
		err = handoffErr("protocol_version", errors.New("unsupported handoff operation"))
		response.ErrorCode = handoffErrorCode(err)
		return response, err
	}
	if err := validateIdentityTokenValue(token); err != nil {
		response.ErrorCode = handoffErrorCode(err)
		return response, err
	}
	if token != identityToken {
		err = handoffErr("identity_changed", errors.New("handoff identity token does not match current managed process"))
		response.ErrorCode = handoffErrorCode(err)
		return response, err
	}
	// Re-read the identity immediately before delegating to the general stop
	// helper.  The helper performs its own ownership check, but this second
	// token comparison closes the handoff's check/use window and ensures a
	// reused PID or changed listener cannot be signaled under an old token.
	latest, err := inspectHandoffIdentity(request, layout)
	if err != nil {
		response.ErrorCode = handoffErrorCode(err)
		return response, err
	}
	if latestToken := handoffIdentityToken(latest); latestToken != identityToken {
		err = handoffErr("identity_changed", errors.New("managed process identity changed before stop"))
		response.ErrorCode = handoffErrorCode(err)
		return response, err
	}
	stopReport, err := stopManagedDoltProcessWithExpectedIdentity(request.CityRoot, strconv.Itoa(request.Endpoint.Port), true, &identity)
	// Stop may have signaled the process (or committed runtime cleanup) before
	// a later safety gate failed. Preserve that phase in the response so the
	// caller never mistakes a failed, partially-applied stop for a read-only
	// refusal and can reconcile the endpoint before retrying.
	response.Mutates = stopReport.Mutated
	if err != nil {
		code := "stop_failed"
		if strings.Contains(strings.ToLower(err.Error()), "data dir") || strings.Contains(strings.ToLower(err.Error()), "lock") {
			code = "data_lock_held"
		}
		err = handoffErr(code, err)
		response.ErrorCode = handoffErrorCode(err)
		return response, err
	}
	if err := verifyHandoffStopComplete(request, layout, identity); err != nil {
		response.ErrorCode = handoffErrorCode(err)
		return response, err
	}
	response.Result = "stopped"
	response.Mutates = true
	response.Identity = identity
	response.IdentityToken = identityToken
	return response, nil
}

// openHandoffLifecycleLock acquires a handle to the existing managed lock
// without creating any runtime files. Inspect is a read-only protocol and may
// be called against a fresh or incomplete city; a refusal must not turn that
// probe into an implicit managed-runtime initialization.
func openHandoffLifecycleLock(cityRoot string) (*os.File, managedDoltRuntimeLayout, error) {
	layout, err := resolveCanonicalManagedDoltRuntimeLayout(cityRoot)
	if err != nil {
		return nil, managedDoltRuntimeLayout{}, handoffErr("invalid_request", err)
	}
	lock, err := os.OpenFile(layout.LockFile, os.O_RDWR, 0)
	if err == nil {
		return lock, layout, nil
	}
	if os.IsNotExist(err) {
		if _, stateErr := readDoltRuntimeStateFile(layout.StateFile); stateErr != nil {
			return nil, layout, handoffErr("state_missing", fmt.Errorf("read managed dolt runtime state: %w", stateErr))
		}
	}
	return nil, layout, handoffErr("lifecycle_busy", fmt.Errorf("open existing managed dolt lifecycle lock: %w", err))
}

// verifyHandoffStopComplete is the postcondition for a successful transfer.
// The generic managed stop helper can legitimately return after discovering no
// controllable process (for example, a crash with a released data lock). That
// is safe for cleanup, but handoff must additionally prove that the captured
// server is gone and that no foreign process replaced its listener before the
// success response was emitted.
func verifyHandoffStopComplete(request handoffProtocolRequest, layout managedDoltRuntimeLayout, expected handoffProtocolIdentity) error {
	state, err := readDoltRuntimeStateFile(layout.StateFile)
	if err != nil {
		return handoffErr("stop_failed", fmt.Errorf("read post-stop runtime state: %w", err))
	}
	if state.Running && state.PID > 0 {
		return handoffErr("stop_failed", fmt.Errorf("managed dolt pid %d remains marked running", state.PID))
	}
	if pidAlive(expected.PID) {
		if !managedDoltPIDStartIdentityMatches(expected.PID, uint64(expected.StartTimeTicks), expected.StartIdentity) {
			return handoffErr("identity_changed", fmt.Errorf("pid %d was reused during stop", expected.PID))
		}
		return handoffErr("stop_failed", fmt.Errorf("managed dolt pid %d remains alive after stop", expected.PID))
	}
	holder := findPortHolderPID(strconv.Itoa(request.Endpoint.Port))
	if holder > 0 {
		if holder == expected.PID {
			return handoffErr("stop_failed", fmt.Errorf("managed dolt pid %d still holds port %d", holder, request.Endpoint.Port))
		}
		return handoffErr("port_conflict", fmt.Errorf("port %d was claimed by pid %d during stop", request.Endpoint.Port, holder))
	}
	if holder == 0 && handoffDoltEndpointReachable(request.Endpoint) {
		return handoffErr("port_conflict", fmt.Errorf("port %d remains reachable but its holder is unknown", request.Endpoint.Port))
	}
	return nil
}

func validateHandoffProtocolRequest(request handoffProtocolRequest) error {
	// Managed GC currently owns TCP listeners only.  Keep this refusal at the
	// request boundary: accepting a Unix-socket endpoint here would let the
	// protocol acquire the lifecycle lock before inspect/stop discover that the
	// rest of the implementation is port-based (request.Endpoint.Port == 0).
	// The socket field remains part of the provider-neutral request shape so a
	// caller can receive a stable unsupported_scope result rather than mistaking
	// the endpoint for a supported handoff.
	if strings.TrimSpace(request.Endpoint.Socket) != "" {
		return handoffErr("unsupported_scope", errors.New("unix socket handoff is not supported by managed GC runtime"))
	}
	if strings.TrimSpace(request.CityRoot) == "" || strings.TrimSpace(request.ScopeRoot) == "" ||
		strings.TrimSpace(request.Database) == "" || strings.TrimSpace(request.Workspace) == "" {
		return handoffErr("invalid_request", errors.New("city, scope-root, database, and workspace are required"))
	}
	for name, path := range map[string]string{"city root": request.CityRoot, "scope root": request.ScopeRoot} {
		if !filepath.IsAbs(path) || filepath.Clean(path) != path {
			return handoffErr("invalid_request", fmt.Errorf("%s must be absolute and canonical", name))
		}
		resolved, err := filepath.EvalSymlinks(path)
		if err != nil || filepath.Clean(resolved) != path {
			return handoffErr("invalid_request", fmt.Errorf("%s must not be symlinked", name))
		}
		info, err := os.Stat(path)
		if err != nil || !info.IsDir() {
			return handoffErr("invalid_request", fmt.Errorf("%s must be an existing directory", name))
		}
	}
	rel, err := filepath.Rel(request.CityRoot, request.ScopeRoot)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return handoffErr("unsupported_scope", errors.New("scope root is outside city root"))
	}
	e := request.Endpoint
	if e.Port <= 0 || e.Port > 65535 || (strings.TrimSpace(e.Host) != "127.0.0.1" && strings.TrimSpace(e.Host) != "localhost" && strings.TrimSpace(e.Host) != "::1") {
		return handoffErr("invalid_request", errors.New("endpoint must be loopback host and valid port"))
	}
	return nil
}

func inspectHandoffIdentity(request handoffProtocolRequest, layout managedDoltRuntimeLayout) (handoffProtocolIdentity, error) {
	if err := validateHandoffPersistedScope(request); err != nil {
		return handoffProtocolIdentity{}, err
	}
	state, err := readDoltRuntimeStateFile(layout.StateFile)
	if err != nil {
		if os.IsNotExist(err) {
			return handoffProtocolIdentity{}, handoffErr("state_missing", errors.New("managed dolt runtime state is missing"))
		}
		return handoffProtocolIdentity{}, handoffErr("state_missing", fmt.Errorf("read managed dolt runtime state: %w", err))
	}
	if !state.Running || state.PID <= 0 {
		return handoffProtocolIdentity{}, handoffErr("process_missing", errors.New("managed dolt process is not running"))
	}
	if !samePath(state.DataDir, layout.DataDir) {
		return handoffProtocolIdentity{}, handoffErr("identity_changed", errors.New("managed dolt data directory differs from runtime layout"))
	}
	if request.Endpoint.Socket != "" {
		return handoffProtocolIdentity{}, handoffErr("unsupported_scope", errors.New("unix socket handoff is not supported by managed GC runtime"))
	}
	if state.Port != request.Endpoint.Port {
		return handoffProtocolIdentity{}, handoffErr("identity_changed", errors.New("managed dolt port differs from request"))
	}
	if !pidAlive(state.PID) {
		return handoffProtocolIdentity{}, handoffErr("process_missing", errors.New("managed dolt process is not alive"))
	}
	owned, deleted := inspectManagedDoltOwnership(state.PID, layout)
	// The generic inspector intentionally accepts a matching cwd/data-dir as
	// useful cleanup evidence. Handoff is a destructive ownership transfer,
	// so require the stronger production launch shape as well: a Dolt
	// sql-server carrying this exact managed config path. A random listener
	// started from the data directory must never become handoff-eligible.
	if owned && !managedDoltHandoffProcessOwned(state.PID, layout) {
		owned = false
	}
	if deleted {
		return handoffProtocolIdentity{}, handoffErr("process_unowned", errors.New("managed dolt process holds deleted data inodes"))
	}
	if !owned {
		return handoffProtocolIdentity{}, handoffErr("process_unowned", errors.New("managed dolt process identity could not be proven"))
	}
	holder := findPortHolderPID(strconv.Itoa(state.Port))
	if holder <= 0 {
		return handoffProtocolIdentity{}, handoffErr("port_conflict", errors.New("managed dolt listener ownership could not be proven"))
	}
	if holder != state.PID {
		return handoffProtocolIdentity{}, handoffErr("port_conflict", fmt.Errorf("port %d is held by pid %d", state.Port, holder))
	}
	if !handoffDoltEndpointReachable(request.Endpoint) {
		return handoffProtocolIdentity{}, handoffErr("endpoint_unreachable", errors.New("managed dolt endpoint is not reachable"))
	}
	ticks := readProcStartTimeTicks(state.PID)
	startIdentity := readProcStartIdentity(state.PID)
	if ticks == 0 && startIdentity == "" {
		return handoffProtocolIdentity{}, handoffErr("process_unowned", errors.New("managed dolt process start identity is unavailable"))
	}
	identity := handoffProtocolIdentity{
		CityRoot: request.CityRoot, ScopeRoot: request.ScopeRoot, Database: request.Database, Workspace: request.Workspace,
		Endpoint: request.Endpoint, DataDir: normalizePathForCompare(layout.DataDir), ConfigFile: normalizePathForCompare(layout.ConfigFile), PID: state.PID,
		StartIdentity: startIdentity, StartTimeTicks: int64(ticks), PortHolderPID: holder,
	}
	if identity.ConfigFile == "" || identity.DataDir == "" {
		return handoffProtocolIdentity{}, handoffErr("protocol_version", errors.New("managed dolt identity paths are unavailable"))
	}
	return identity, nil
}

// validateHandoffPersistedScope proves that the request names a scope and
// database owned by this city. Request fields are untrusted input: they must
// be matched against canonical config/metadata/identity files before any
// process is considered eligible for transfer.
func validateHandoffPersistedScope(request handoffProtocolRequest) error {
	fs := fsys.OSFS{}
	cityRoot := normalizePathForCompare(request.CityRoot)
	scopeRoot := normalizePathForCompare(request.ScopeRoot)
	cityConfig := filepath.Join(cityRoot, "city.toml")
	if _, err := fs.Stat(cityConfig); err != nil {
		if os.IsNotExist(err) {
			return handoffErr("state_missing", fmt.Errorf("city config %s is missing", cityConfig))
		}
		return handoffErr("state_missing", fmt.Errorf("stat city config %s: %w", cityConfig, err))
	}
	cfg, err := config.Load(fs, cityConfig)
	if err != nil {
		return handoffErr("state_missing", fmt.Errorf("load city config: %w", err))
	}
	resolveRigPaths(cityRoot, cfg.Rigs)
	cityScope := samePath(scopeRoot, cityRoot)
	rigScope := false
	for _, rig := range cfg.Rigs {
		if strings.TrimSpace(rig.Path) != "" && samePath(scopeRoot, normalizePathForCompare(rig.Path)) {
			rigScope = true
			break
		}
	}
	if !cityScope && !rigScope {
		return handoffErr("unsupported_scope", errors.New("scope root is not the city root or a declared rig"))
	}
	resolved, err := contract.ResolveScopeConfigState(fs, cityRoot, scopeRoot, "")
	if err != nil {
		return handoffErr("unsupported_scope", fmt.Errorf("resolve scope config: %w", err))
	}
	if resolved.Kind != contract.ScopeConfigAuthoritative {
		return handoffErr("state_missing", errors.New("scope config is not authoritative"))
	}
	state := resolved.State
	if cityScope {
		if state.EndpointOrigin != contract.EndpointOriginManagedCity {
			return handoffErr("unsupported_scope", fmt.Errorf("city endpoint origin %q is not managed_city", state.EndpointOrigin))
		}
	} else {
		if state.EndpointOrigin != contract.EndpointOriginInheritedCity {
			return handoffErr("unsupported_scope", fmt.Errorf("rig endpoint origin %q is not inherited_city", state.EndpointOrigin))
		}
		cityResolved, cityErr := contract.ResolveScopeConfigState(fs, cityRoot, cityRoot, "")
		if cityErr != nil || cityResolved.Kind != contract.ScopeConfigAuthoritative || cityResolved.State.EndpointOrigin != contract.EndpointOriginManagedCity {
			return handoffErr("unsupported_scope", errors.New("inherited rig does not resolve to a managed city endpoint"))
		}
	}
	if !handoffDoltModeTransferable(state.DoltMode) {
		return handoffErr("unsupported_scope", fmt.Errorf("dolt mode %q is not transferable", state.DoltMode))
	}
	metadataPath := filepath.Join(scopeRoot, ".beads", "metadata.json")
	metadata, ok, err := contract.LoadMetadataState(fs, metadataPath)
	if err != nil {
		if backend, backendOK, backendErr := contract.ReadMetadataBackend(fs, metadataPath); backendErr == nil && backendOK {
			backend = strings.ToLower(strings.TrimSpace(backend))
			if backend == "legacy" {
				database, databaseOK, databaseErr := contract.ReadDoltDatabase(fs, metadataPath)
				if databaseErr != nil {
					return handoffErr("state_missing", fmt.Errorf("read legacy metadata database: %w", databaseErr))
				}
				mode, _, _ := contract.ReadDoltMode(fs, metadataPath)
				metadata = contract.MetadataState{Backend: backend, DoltDatabase: database, DoltMode: mode}
				ok = databaseOK
				err = nil
			}
			if err != nil && backend != "" && backend != "dolt" && backend != "bd" {
				return handoffErr("unsupported_scope", fmt.Errorf("backend %q is not direct Dolt", backend))
			}
		}
		if err != nil {
			return handoffErr("state_missing", fmt.Errorf("load metadata: %w", err))
		}
	}
	if !ok {
		return handoffErr("state_missing", errors.New("scope metadata is missing"))
	}
	backend := strings.ToLower(strings.TrimSpace(metadata.Backend))
	if backend == "doltlite" {
		return handoffErr("unsupported_scope", errors.New("doltlite backend cannot be handed off"))
	}
	if backend != "" && backend != "legacy" && backend != "dolt" && backend != "bd" {
		return handoffErr("unsupported_scope", fmt.Errorf("backend %q is not direct Dolt", metadata.Backend))
	}
	if !handoffDoltModeTransferable(metadata.DoltMode) {
		return handoffErr("unsupported_scope", fmt.Errorf("metadata dolt mode %q is not transferable", metadata.DoltMode))
	}
	database := strings.TrimSpace(metadata.DoltDatabase)
	if database == "" {
		return handoffErr("state_missing", errors.New("metadata dolt_database is missing"))
	}
	if database != strings.TrimSpace(request.Database) {
		return handoffErr("identity_changed", errors.New("requested database differs from persisted Dolt database"))
	}

	l1ID, l1OK, err := contract.ReadProjectIdentity(fs, scopeRoot)
	if err != nil {
		return handoffErr("state_missing", fmt.Errorf("read project identity: %w", err))
	}
	l2ID, err := readHandoffMetadataProjectID(metadataPath)
	if err != nil {
		return handoffErr("state_missing", fmt.Errorf("read metadata project identity: %w", err))
	}
	if l1OK && l2ID != "" && l1ID != l2ID {
		return handoffErr("identity_changed", errors.New("project identity differs between identity.toml and metadata.json"))
	}
	trustedID := l1ID
	if !l1OK {
		trustedID = l2ID
	}
	if strings.TrimSpace(trustedID) == "" {
		return handoffErr("state_missing", errors.New("project identity is missing"))
	}
	if trustedID != strings.TrimSpace(request.Workspace) {
		return handoffErr("identity_changed", errors.New("requested workspace differs from persisted project identity"))
	}
	return nil
}

func handoffDoltModeTransferable(mode string) bool {
	switch strings.ToLower(strings.TrimSpace(mode)) {
	case "", "server":
		return true
	default:
		return false
	}
}

func readHandoffMetadataProjectID(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return "", nil
		}
		return "", err
	}
	var metadata struct {
		ProjectID string `json:"project_id"`
	}
	if err := json.Unmarshal(data, &metadata); err != nil {
		return "", err
	}
	return strings.TrimSpace(metadata.ProjectID), nil
}

func handoffDoltEndpointReachable(endpoint handoffProtocolEndpoint) bool {
	host := strings.TrimSpace(endpoint.Host)
	if host == "localhost" {
		host = "127.0.0.1"
	}
	conn, err := net.DialTimeout("tcp", net.JoinHostPort(host, strconv.Itoa(endpoint.Port)), 250*time.Millisecond)
	if err != nil {
		return false
	}
	_ = conn.Close()
	return true
}

func managedDoltHandoffProcessOwned(pid int, layout managedDoltRuntimeLayout) bool {
	if pid <= 0 {
		return false
	}
	configInfo, err := os.Stat(layout.ConfigFile)
	if err != nil || !configInfo.Mode().IsRegular() {
		return false
	}
	args, err := processArgs(pid)
	if err != nil {
		return false
	}
	fields := strings.Fields(args)
	if len(fields) == 0 {
		return false
	}
	executable := filepath.Base(strings.TrimSpace(fields[0]))
	if executable != "dolt" {
		return false
	}
	hasSQLServer := false
	configMatch := false
	for i := 0; i < len(fields); i++ {
		field := fields[i]
		if field == "sql-server" {
			hasSQLServer = true
			continue
		}
		if field == "--config" && i+1 < len(fields) {
			configMatch = samePath(fields[i+1], layout.ConfigFile)
			i++
			continue
		}
		if strings.HasPrefix(field, "--config=") {
			configMatch = samePath(strings.TrimPrefix(field, "--config="), layout.ConfigFile)
		}
	}
	return hasSQLServer && configMatch
}

func handoffIdentityToken(identity handoffProtocolIdentity) string {
	b, _ := json.Marshal(identity)
	sum := sha256.Sum256(b)
	return "sha256:" + hex.EncodeToString(sum[:])
}

func validateIdentityTokenValue(token string) error {
	if len(token) != len("sha256:")+sha256.Size*2 || !strings.HasPrefix(token, "sha256:") {
		return handoffErr("identity_changed", errors.New("identity token must be sha256 encoded"))
	}
	if _, err := hex.DecodeString(token[len("sha256:"):]); err != nil {
		return handoffErr("identity_changed", errors.New("identity token is not hexadecimal"))
	}
	return nil
}
