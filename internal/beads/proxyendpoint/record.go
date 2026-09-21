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
	"os"
	"path/filepath"
	"strings"

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
	// DefaultRootDirName is where bd roots a proxied scope's proxy when
	// neither the environment nor the sidecar overrides it.
	DefaultRootDirName = "dolt"
	// RootPathEnv is bd's own override for the proxy root, read ahead of the
	// sidecar so gc resolves the root the way bd does.
	RootPathEnv = "BEADS_PROXIED_SERVER_ROOT_PATH"
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
// bd's own precedence: BEADS_PROXIED_SERVER_ROOT_PATH, then the sidecar's
// root_path, then the documented default <scope>/.beads/dolt.
//
// The environment arm is bd's (cmd/bd/proxied_server.go), not an invention
// here: an operator who exports it moves the root for every bd command in that
// shell, and a reader that ignored it would look for the record in a directory
// nothing writes.
func ProviderRoot(scopeRoot string) (string, error) {
	beadsDir := filepath.Join(pathutil.NormalizePathForCompare(scopeRoot), ".beads")
	if env := strings.TrimSpace(os.Getenv(RootPathEnv)); env != "" {
		return filepath.Clean(env), nil
	}
	sidecar, err := ReadSidecar(beadsDir)
	if err != nil {
		return "", err
	}
	if root := sidecar.ResolvedRootPath(beadsDir); root != "" {
		return root, nil
	}
	return filepath.Join(beadsDir, DefaultRootDirName), nil
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
