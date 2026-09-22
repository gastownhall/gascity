// Package proxyendpoint reads and validates the endpoint record bd publishes
// for a proxied-server workspace: `<root>/proxy.pid`, written by bd's db-proxy
// once its Dolt child is ready and removed when the proxy exits.
//
// It is a READ contract. gc never writes, rotates or removes any file under a
// bd proxy root, never reads `proxy.secret` and never speaks bd's IDENT control
// protocol: those are bd-internal, and the epic's hard constraint is that bd
// owns the proxy topology and lifecycle outright. What gc needs from the record
// is narrower — which loopback port a bd-supervised Dolt is reachable on, which
// process generation published it, and whether that generation is still the one
// running — and every one of those answers is derivable from files bd documents
// plus the process table.
//
// The schema mirrors beads v1.3.0 internal/storage/dbproxy/pidfile/pidfile.go
// (schema 2). Nothing in gc writes these files, so the risk this package
// carries is misreading one: a field read wrongly is a field gc would use to
// dial the wrong process.
package proxyendpoint

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/gastownhall/gascity/internal/beads/contract"
	"github.com/gastownhall/gascity/internal/fsys"
	"github.com/gastownhall/gascity/internal/pathutil"
)

// File and field constants of bd's proxied-server layout. They are named here
// once so a spelling change in bd is one edit against one failing test, rather
// than a literal repeated across gc.
const (
	// PIDFileName is the proxy's own liveness record inside the proxy root.
	PIDFileName = "proxy.pid"
	// ConfigFileName is the sql-server config bd renders in the proxy root and
	// hands its Dolt child with --config.
	ConfigFileName = "config.yaml"
	// SidecarFileName is the per-scope client info bd writes under .beads/ when
	// it initializes a proxied workspace.
	SidecarFileName = "proxied_server_client_info.json"
	// RecordKind is the `kind` a proxy's own record carries. bd writes
	// "dolt-backend" for its Dolt child's record, which is a different file.
	RecordKind = "db-proxy"
	// SchemaV2 is the lowest record schema this package understands.
	SchemaV2 = 2
	// ChildVerb is the bd subcommand the proxy supervisor process runs.
	ChildVerb = "db-proxy-child"
	// RootFlag names the proxy root in the supervisor's argv.
	RootFlag = "--root"
	// IdleTimeoutFlag carries the supervisor's effective idle window.
	IdleTimeoutFlag = "--idle-timeout"
	// DefaultRootDirName is the directory name bd's Dolt data dir defaults to
	// inside a scope's .beads directory.
	DefaultRootDirName = "dolt"
	// MetadataFileName is bd's per-scope metadata document, which can move the
	// scope's Dolt data directory with dolt_data_dir.
	MetadataFileName = "metadata.json"
	// RootPathEnv is bd's own override for the proxy root, read ahead of the
	// sidecar so gc resolves the root the way bd does.
	RootPathEnv = "BEADS_PROXIED_SERVER_ROOT_PATH"
	// DoltDataDirEnv is bd's override for a scope's Dolt data directory, read
	// ahead of metadata.json's dolt_data_dir.
	DoltDataDirEnv = "BEADS_DOLT_DATA_DIR"
	// SharedServerModeEnv turns on bd's shared-server mode, in which every
	// project on the host serves out of one Dolt root.
	SharedServerModeEnv = "BEADS_DOLT_SHARED_SERVER"
	// SharedServerDirEnv relocates the shared-server directory bd would
	// otherwise put under the user's home.
	SharedServerDirEnv = "BEADS_SHARED_SERVER_DIR"
	// SharedServerDirName and SharedServerBeadsDirName spell the default
	// shared-server location, ~/.beads/shared-server.
	SharedServerBeadsDirName = ".beads"
	SharedServerDirName      = "shared-server"
)

// Record is bd's proxy.pid document. Every field bd writes is decoded, even the
// ones gc does not act on: `upstream_id` and `control_port` are what a future
// cross-check would need, and a record gc can round-trip is a record gc can
// show it understood.
type Record struct {
	PID         int    `json:"pid"`
	Port        int    `json:"port"`
	UpstreamID  string `json:"upstream_id,omitempty"`
	Schema      int    `json:"schema,omitempty"`
	Kind        string `json:"kind,omitempty"`
	Birth       string `json:"birth,omitempty"`
	RootID      string `json:"root_id,omitempty"`
	ControlPort int    `json:"control_port,omitempty"`
}

// Sentinel outcomes of reading and validating a record. They are sentinels
// rather than message text because every caller branches on them: doctor
// reports them, and the admission path in the later slices decides between
// "retry", "escalate through a bd verb" and "refuse" on exactly this split.
var (
	// ErrNoProxy reports that the root holds no record at all. bd removes the
	// record on an orderly proxy exit, so this is the ordinary stopped state,
	// not a fault.
	ErrNoProxy = errors.New("proxyendpoint: no proxy record")
	// ErrMalformed reports a record that is present but not decodable.
	ErrMalformed = errors.New("proxyendpoint: malformed proxy record")
	// ErrLegacyProxy reports a pre-schema-2 record, which carries no birth
	// token and therefore cannot identify a process generation.
	ErrLegacyProxy = errors.New("proxyendpoint: legacy proxy record schema")
	// ErrNotOurs reports a record that does not describe this root's proxy —
	// a copied or symlinked record, or one whose fields are out of range.
	ErrNotOurs = errors.New("proxyendpoint: proxy record is not ours")
)

// FieldError names the record field that failed validation, so a refusal says
// which field disagreed instead of only that one did. Callers match the class
// with errors.Is and the field with errors.As.
type FieldError struct {
	// Field is the JSON field name as bd writes it.
	Field string
	// Want and Got are rendered only when set. Got is deliberately left empty
	// for the birth token, which carries the host's boot id.
	Want string
	Got  string
	// Class is the sentinel this error belongs to.
	Class error
}

// Error renders the field and, when they are set, the two values.
func (e *FieldError) Error() string {
	switch {
	case e.Want != "" && e.Got != "":
		return fmt.Sprintf("%v: field %s = %s, want %s", e.Class, e.Field, e.Got, e.Want)
	case e.Got != "":
		return fmt.Sprintf("%v: field %s = %s", e.Class, e.Field, e.Got)
	case e.Want != "":
		return fmt.Sprintf("%v: field %s, want %s", e.Class, e.Field, e.Want)
	default:
		return fmt.Sprintf("%v: field %s", e.Class, e.Field)
	}
}

// Unwrap exposes the sentinel class so errors.Is(err, ErrNotOurs) works.
func (e *FieldError) Unwrap() error { return e.Class }

// PIDPath returns the record path inside a proxy root.
func PIDPath(root string) string { return filepath.Join(root, PIDFileName) }

// ProviderRoot resolves the directory bd roots a proxied scope's proxy at, with
// bd's own precedence (beads internal/doltserver/physical_root.go
// ResolveProxiedServerRootPath): BEADS_PROXIED_SERVER_ROOT_PATH, then the
// sidecar's root_path, then the scope's Dolt data directory.
//
// The environment arm is bd's, not an invention here: an operator who exports it
// moves the root for every bd command in that shell, and a reader that ignored it
// would look for the record in a directory nothing writes. A RELATIVE value joins
// the scope's .beads directory, which is where bd joins it — not the reader's
// working directory, which bd never consults.
//
// The last arm is not a fallback for exotic layouts; it is the NORMAL path for
// every scope gc creates. gc's provider script runs `bd init --proxied-server
// --proxied-server-idle-timeout 0` with no --proxied-server-root-path, bd
// persists only the flag value as root_path, and so a gc-owned scope's sidecar
// carries none and bd roots the proxy wherever DoltDataDir says.
func ProviderRoot(scopeRoot string) (string, error) {
	beadsDir := filepath.Join(pathutil.NormalizePathForCompare(scopeRoot), ".beads")
	// The value is used RAW, the way bd uses it (beads
	// internal/doltserver/physical_root.go ResolveProxiedServerRootPath): a
	// reader that trimmed it would resolve a different directory than the proxy
	// serves, and parity is the whole contract of this resolver.
	if env := os.Getenv(RootPathEnv); env != "" {
		return resolveUnderBeadsDir(beadsDir, env), nil
	}
	sidecar, err := ReadSidecar(beadsDir)
	if err != nil {
		return "", err
	}
	if root := sidecar.ResolvedRootPath(beadsDir); root != "" {
		return root, nil
	}
	return DoltDataDir(beadsDir)
}

// DoltDataDir resolves a scope's Dolt data directory the way bd's own
// side-effect-free resolver does (beads internal/doltserver/physical_root.go
// DoltDirPath -> projectDoltDirPath, configfile.Config.DatabasePath): the
// shared-server root when shared-server mode is on, then BEADS_DOLT_DATA_DIR,
// then metadata.json's dolt_data_dir, then <scope>/.beads/dolt. Absolute values
// stand as written and relative ones join the scope's .beads directory.
//
// The metadata arm is the one gc's own migration depends on. `gc beads city
// migrate proxied` writes dolt_data_dir on every rig precisely so bd roots the
// rig's proxy at the CITY's data dir — a rig that shares its city's proxy root
// is the shape the pool key is built to express — and a reader that answered
// <rig>/.beads/dolt for such a rig would look for proxy.pid in a directory bd
// never publishes into, and report no_record for every shared-root rig gc
// created.
//
// Two arms of bd's resolution are deliberately not mirrored, and both fail
// closed (gc resolves a root bd does not serve, finds no record, and refuses
// rather than dialing something else): shared-server mode declared in
// config.yaml as dolt.shared-server rather than in the environment, and the
// legacy absolute `database` key, which is the removed SQLite backend's file
// path and has no meaning for a scope bd serves over a proxy.
func DoltDataDir(beadsDir string) (string, error) {
	if sharedServerMode() {
		// bd mirrors ResolveDoltDir here: an unresolvable shared directory (no
		// home) falls through to the per-project resolution below.
		if dir, ok := sharedDoltDir(); ok {
			return dir, nil
		}
	}
	// Raw again, and for the same reason: bd's projectDoltDirPath takes
	// os.Getenv("BEADS_DOLT_DATA_DIR") as written, so BEADS_DOLT_DATA_DIR=" data"
	// roots bd at "<.beads>/ data". Trimming it here would have gc resolve
	// "<.beads>/data" and report no_record for a proxy that is serving fine.
	if env := os.Getenv(DoltDataDirEnv); env != "" {
		return resolveUnderBeadsDir(beadsDir, env), nil
	}
	recorded, ok, err := contract.ReadMetadataDoltDataDir(fsys.OSFS{}, filepath.Join(beadsDir, MetadataFileName))
	if err != nil {
		return "", fmt.Errorf("read %s: %w", filepath.Join(beadsDir, MetadataFileName), err)
	}
	// bd's Config.GetDoltDataDir/DatabasePath take the recorded value as written
	// too.
	if ok && recorded != "" {
		return resolveUnderBeadsDir(beadsDir, recorded), nil
	}
	return filepath.Join(beadsDir, DefaultRootDirName), nil
}

// resolveUnderBeadsDir resolves one of bd's path settings: absolute as written,
// relative against the scope's .beads directory.
//
// bd resolves every one of them this way (envOrAbsJoin in
// internal/storage/domain/fs/context.go, and Config.DatabasePath), and the
// difference is not cosmetic: a reader that joined a relative value to its own
// working directory would name a path that depends on where gc was invoked from.
func resolveUnderBeadsDir(beadsDir, path string) string {
	if filepath.IsAbs(path) {
		return filepath.Clean(path)
	}
	return filepath.Join(beadsDir, path)
}

// sharedServerMode reports whether the environment turns on bd's shared-server
// mode, in which every project on the host serves out of one Dolt root.
//
// Only the environment arm of bd's IsSharedServerMode is mirrored; the
// config.yaml dolt.shared-server arm is bd's own layered config and is not read
// here. See DoltDataDir for why the omission fails closed.
func sharedServerMode() bool {
	// bd's IsSharedServerMode compares the raw value, so
	// BEADS_DOLT_SHARED_SERVER=" 1" leaves shared-server mode OFF for bd; a trim
	// here would turn it on for gc alone and resolve a root bd does not serve.
	value := os.Getenv(SharedServerModeEnv)
	return value == "1" || strings.EqualFold(value, "true")
}

// sharedDoltDir is the Dolt root of bd's shared server, without creating it.
// Resolution must not have side effects: merely asking where a scope's data
// lives cannot be allowed to create a ~/.beads tree.
func sharedDoltDir() (string, bool) {
	// bd's SharedServerPath reads BEADS_SHARED_SERVER_DIR raw.
	if dir := os.Getenv(SharedServerDirEnv); dir != "" {
		return filepath.Join(dir, DefaultRootDirName), true
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", false
	}
	return filepath.Join(home, SharedServerBeadsDirName, SharedServerDirName, DefaultRootDirName), true
}

// Read decodes the record in root. An absent record is ErrNoProxy and an
// undecodable one is ErrMalformed; both wrap the path so the read that failed
// is still visible in the message.
func Read(root string) (Record, error) {
	var rec Record
	data, err := os.ReadFile(PIDPath(root)) // #nosec G304 -- root is a resolved bd proxy root
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return rec, fmt.Errorf("%w at %s", ErrNoProxy, PIDPath(root))
		}
		return rec, fmt.Errorf("read %s: %w", PIDPath(root), err)
	}
	if err := json.Unmarshal(data, &rec); err != nil {
		return rec, fmt.Errorf("%w at %s: %w", ErrMalformed, PIDPath(root), err)
	}
	return rec, nil
}

// Ownership is the narrow subset of a record that answers a different question
// from Read's: not "may gc dial this endpoint" but "is this process bd's, so
// that killing it would kill bd's proxy".
//
// It carries two fields because two fields are all that question needs. The
// proof of ownership is the recorded PID's argv naming bd's supervisor verb and
// this root, not anything else in the document, so every other field is
// something a reader can afford not to understand.
type Ownership struct {
	// PID is the process the record names.
	PID int
	// Kind separates the proxy's own record from the dolt-backend record bd
	// writes beside it.
	Kind string
}

// ReadOwnership decodes only pid and kind, tolerating everything else the
// document contains.
//
// Leniency here is a safety property, not convenience. encoding/json fails a
// whole decode on a type mismatch in ANY tagged field, so a proxy.pid from a
// newer bd — birth promoted to an object, schema written as a string, a field
// nobody here has heard of — would make a strict reader report "no record" and
// the reaper would then kill a live bd proxy's Dolt child. Killing a proxy is
// irreversible and dialing one is not, which is why the two questions fail in
// opposite directions: admission refuses whatever it cannot prove (Read and
// Validate stay strict), and protection protects whatever it cannot rule out.
//
// What it will NOT do is invent a pid or a kind. Without a pid there is no
// process to check an argv against, and without a readable kind gc cannot tell
// the proxy's record from its Dolt child's, so both must be present and
// readable; a numeric pid written as a string is read, because a process that is
// bd's proxy does not stop being it when its record changes spelling.
func ReadOwnership(root string) (Ownership, error) {
	var own Ownership
	data, err := os.ReadFile(PIDPath(root)) // #nosec G304 -- root is a resolved bd proxy root
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return own, fmt.Errorf("%w at %s", ErrNoProxy, PIDPath(root))
		}
		return own, fmt.Errorf("read %s: %w", PIDPath(root), err)
	}
	// A map, not a struct: an unknown or retyped field lands in it instead of
	// failing the decode.
	fields := map[string]any{}
	if err := json.Unmarshal(data, &fields); err != nil {
		return own, fmt.Errorf("%w at %s: %w", ErrMalformed, PIDPath(root), err)
	}
	kind, ok := fields["kind"].(string)
	if !ok {
		return own, &FieldError{Field: "kind", Class: ErrMalformed, Want: "a string"}
	}
	own.Kind = kind
	pid, ok := ownershipPID(fields["pid"])
	if !ok {
		return own, &FieldError{Field: "pid", Class: ErrMalformed, Want: "a process id"}
	}
	own.PID = pid
	return own, nil
}

// ownershipPID reads a pid out of a decoded JSON value, accepting the number bd
// writes today and a numeric string a later version might.
func ownershipPID(value any) (int, bool) {
	switch v := value.(type) {
	case float64:
		if v != math.Trunc(v) || v < math.MinInt32 || v > math.MaxInt32 {
			return 0, false
		}
		return int(v), true
	case string:
		pid, err := strconv.Atoi(strings.TrimSpace(v))
		if err != nil {
			return 0, false
		}
		return pid, true
	default:
		return 0, false
	}
}

// RootID is bd's workspace identity for a proxy root: the SHA-256 of the root's
// symlink-resolved absolute path (beads dbproxy/identity.RootID).
//
// It is path-derived, which is the whole reason the guard tick in the later
// slices re-reads it: a directory recreated at the same path yields the same
// RootID as the one that was moved away, so equality proves the spelling of the
// path and not the identity of the data behind it.
func RootID(root string) (string, error) {
	abs, err := filepath.Abs(root)
	if err != nil {
		return "", fmt.Errorf("absolute proxy root path: %w", err)
	}
	resolved, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return "", fmt.Errorf("resolve proxy root path: %w", err)
	}
	sum := sha256.Sum256([]byte(resolved))
	return hex.EncodeToString(sum[:]), nil
}

// Validate checks every field bd's own ValidateV2 checks, plus the root
// identity bd checks separately when it adopts a proxy: the record's root_id
// must equal the id gc computes for the root it read the record from.
//
// The root_id arm is what makes a copied record harmless. bd refuses a foreign
// record because its IDENT reply disagrees; gc has no IDENT, so the recomputed
// id is the whole proof — and a record carrying no root_id at all is refused
// rather than trusted, because "absent" and "mine" are not the same claim.
func Validate(rec Record, root string) error {
	if rec.Schema < SchemaV2 {
		return &FieldError{Field: "schema", Class: ErrLegacyProxy, Got: fmt.Sprint(rec.Schema), Want: fmt.Sprintf(">= %d", SchemaV2)}
	}
	if rec.Kind != RecordKind {
		return &FieldError{Field: "kind", Class: ErrNotOurs, Got: rec.Kind, Want: RecordKind}
	}
	if rec.PID <= 0 {
		return &FieldError{Field: "pid", Class: ErrNotOurs, Got: fmt.Sprint(rec.PID), Want: "> 0"}
	}
	if !validPort(rec.Port) {
		return &FieldError{Field: "port", Class: ErrNotOurs, Got: fmt.Sprint(rec.Port), Want: "1..65535"}
	}
	if rec.ControlPort != 0 && !validPort(rec.ControlPort) {
		return &FieldError{Field: "control_port", Class: ErrNotOurs, Got: fmt.Sprint(rec.ControlPort), Want: "0 or 1..65535"}
	}
	if rec.Birth == "" {
		return &FieldError{Field: "birth", Class: ErrNotOurs, Want: "a non-empty process birth token"}
	}
	want, err := RootID(root)
	if err != nil {
		return fmt.Errorf("identify proxy root %s: %w", root, err)
	}
	if rec.RootID != want {
		// The ids are hex digests of paths, not secrets, but a pair of 64-hex
		// strings in an operator-facing line is noise; the prefix separates
		// "different root" from "same root", which is the whole question.
		return &FieldError{Field: "root_id", Class: ErrNotOurs, Got: ShortID(rec.RootID), Want: ShortID(want)}
	}
	return nil
}

// validPort reports whether p is a usable TCP port, matching bd's own range
// check on the record.
func validPort(p int) bool { return p >= 1 && p <= 65535 }

// ShortDigest renders a short, stable fingerprint of an opaque token.
//
// It exists for the birth token, which is the thing that distinguishes one proxy
// generation from the next and also embeds the host's boot id — a value with no
// place in a log line or a doctor payload. Truncating the token itself would
// spend most of the budget on its constant "linux-v1:" prefix; hashing spends
// all of it on the part that differs.
func ShortDigest(token string) string {
	if strings.TrimSpace(token) == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(token))
	return ShortID(hex.EncodeToString(sum[:]))
}

// ShortID renders a hex digest for a message or a diagnostic: the first twelve
// characters, enough to distinguish two values and short enough to read.
func ShortID(id string) string {
	id = strings.TrimSpace(id)
	if id == "" {
		return ""
	}
	if len(id) <= 12 {
		return id
	}
	return id[:12]
}
