package connect

import (
	"bytes"
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"strings"

	"github.com/BurntSushi/toml"
)

// DescriptorSchema is the `.gc/control-plane.toml` schema this binary
// encodes and decodes.
const DescriptorSchema = 1

// PlaneWorker is the only plane a descriptor may declare.
const PlaneWorker = "worker"

// CredentialFilePrefix is the only credential source a descriptor may name.
const CredentialFilePrefix = "file:"

// ErrDescriptorInvalid is wrapped by every DecodeDescriptor and
// EncodeDescriptor error.
var ErrDescriptorInvalid = errors.New("invalid control-plane descriptor")

// Descriptor is the decoded `.gc/control-plane.toml`: the marker that makes a
// city directory a worker site. It carries no timestamps and no secrets; the
// credential is a reference to the bearer file, never the bearer.
type Descriptor struct {
	Schema     int                `toml:"schema"`
	Plane      string             `toml:"plane"`
	City       string             `toml:"city"`
	SessionID  string             `toml:"session_id"`
	Generation int                `toml:"generation"`
	Contract   int                `toml:"contract"`
	Credential string             `toml:"credential"`
	Ledgers    []DescriptorLedger `toml:"ledger"`
}

// DescriptorLedger is one attached ledger, the storage classes it serves,
// and the workspaces bound to it.
type DescriptorLedger struct {
	Scope          string   `toml:"scope"`
	Classes        []string `toml:"classes"`
	URL            string   `toml:"url"`
	ProjectID      string   `toml:"project_id"`
	CAFile         string   `toml:"ca_file,omitempty"`
	AllowPlaintext bool     `toml:"allow_plaintext"`
	Workspaces     []string `toml:"workspaces"`
}

// rawDescriptor mirrors Descriptor with pointers so a missing key is told
// apart from a zero value.
type rawDescriptor struct {
	Schema     *int        `toml:"schema"`
	Plane      *string     `toml:"plane"`
	City       *string     `toml:"city"`
	SessionID  *string     `toml:"session_id"`
	Generation *int        `toml:"generation"`
	Contract   *int        `toml:"contract"`
	Credential *string     `toml:"credential"`
	Ledgers    []rawLedger `toml:"ledger"`
}

type rawLedger struct {
	Scope          *string   `toml:"scope"`
	Classes        *[]string `toml:"classes"`
	URL            *string   `toml:"url"`
	ProjectID      *string   `toml:"project_id"`
	CAFile         *string   `toml:"ca_file"`
	AllowPlaintext *bool     `toml:"allow_plaintext"`
	Workspaces     *[]string `toml:"workspaces"`
}

// descriptorTopKeys and descriptorLedgerKeys are the exact-case keys a
// descriptor may carry. They are checked against md.Keys() because the TOML
// decoder matches struct fields case-insensitively and md.Undecoded() does
// not report a key it matched that way.
var (
	descriptorTopKeys = map[string]bool{
		"schema": true, "plane": true, "city": true, "session_id": true,
		"generation": true, "contract": true, "credential": true, descriptorLedgerTable: true,
	}
	descriptorLedgerKeys = map[string]bool{
		"scope": true, "classes": true, "url": true, "project_id": true, "ca_file": true,
		"allow_plaintext": true, "workspaces": true,
	}
)

const descriptorLedgerTable = "ledger"

// checkDescriptorKeys refuses any key path that is not an exact-case member
// of the descriptor schema.
func checkDescriptorKeys(keys []toml.Key) error {
	for _, k := range keys {
		ok := false
		switch len(k) {
		case 1:
			ok = descriptorTopKeys[k[0]]
		case 2:
			ok = k[0] == descriptorLedgerTable && descriptorLedgerKeys[k[1]]
		}
		if !ok {
			return descriptorErr("unknown key or table %q", k.String())
		}
	}
	return nil
}

func descriptorErr(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrDescriptorInvalid, fmt.Sprintf(format, args...))
}

// DecodeDescriptor strictly decodes `.gc/control-plane.toml`. It refuses
// unknown keys or tables, missing fields, wrong types, a schema other than
// DescriptorSchema, a plane other than PlaneWorker, an unknown contract, and
// a credential that is not a `file:` reference.
func DecodeDescriptor(data []byte) (Descriptor, error) {
	var raw rawDescriptor
	md, err := toml.NewDecoder(bytes.NewReader(data)).Decode(&raw)
	if err != nil {
		return Descriptor{}, fmt.Errorf("%w: %w", ErrDescriptorInvalid, err)
	}
	if err := checkDescriptorKeys(md.Keys()); err != nil {
		return Descriptor{}, err
	}
	if undecoded := md.Undecoded(); len(undecoded) > 0 {
		keys := make([]string, len(undecoded))
		for i, k := range undecoded {
			keys[i] = k.String()
		}
		return Descriptor{}, descriptorErr("unknown keys or tables: %s", strings.Join(keys, ", "))
	}
	missing := func(name string) error { return descriptorErr("missing %s", name) }
	switch {
	case raw.Schema == nil:
		return Descriptor{}, missing("schema")
	case raw.Plane == nil:
		return Descriptor{}, missing("plane")
	case raw.City == nil:
		return Descriptor{}, missing("city")
	case raw.SessionID == nil:
		return Descriptor{}, missing("session_id")
	case raw.Generation == nil:
		return Descriptor{}, missing("generation")
	case raw.Contract == nil:
		return Descriptor{}, missing("contract")
	case raw.Credential == nil:
		return Descriptor{}, missing("credential")
	}
	d := Descriptor{
		Schema:     *raw.Schema,
		Plane:      *raw.Plane,
		City:       *raw.City,
		SessionID:  *raw.SessionID,
		Generation: *raw.Generation,
		Contract:   *raw.Contract,
		Credential: *raw.Credential,
	}
	for i, rl := range raw.Ledgers {
		field := func(name string) error { return missing(fmt.Sprintf("ledger[%d].%s", i, name)) }
		switch {
		case rl.Scope == nil:
			return Descriptor{}, field("scope")
		case rl.Classes == nil:
			return Descriptor{}, field("classes")
		case rl.URL == nil:
			return Descriptor{}, field("url")
		case rl.ProjectID == nil:
			return Descriptor{}, field("project_id")
		case rl.AllowPlaintext == nil:
			return Descriptor{}, field("allow_plaintext")
		case rl.Workspaces == nil:
			return Descriptor{}, field("workspaces")
		}
		l := DescriptorLedger{
			Scope:          *rl.Scope,
			Classes:        *rl.Classes,
			URL:            *rl.URL,
			ProjectID:      *rl.ProjectID,
			AllowPlaintext: *rl.AllowPlaintext,
			Workspaces:     *rl.Workspaces,
		}
		if rl.CAFile != nil {
			if *rl.CAFile == "" {
				return Descriptor{}, descriptorErr("ledger[%d].ca_file is empty", i)
			}
			l.CAFile = *rl.CAFile
		}
		d.Ledgers = append(d.Ledgers, l)
	}
	if err := d.Validate(); err != nil {
		return Descriptor{}, err
	}
	return d, nil
}

// EncodeDescriptor validates d and renders it deterministically: keys in a
// fixed order; the city ledger first, then binding ledgers by scope; each
// ledger's classes in StorageClasses() order and its workspaces sorted. The
// same descriptor always yields the same bytes whatever order its ledgers,
// classes and workspaces were found in.
func EncodeDescriptor(d Descriptor) ([]byte, error) {
	if err := d.Validate(); err != nil {
		return nil, err
	}
	d.Ledgers = canonicalLedgers(d.Ledgers)
	var buf bytes.Buffer
	enc := toml.NewEncoder(&buf)
	enc.Indent = ""
	if err := enc.Encode(d); err != nil {
		return nil, fmt.Errorf("%w: encoding: %w", ErrDescriptorInvalid, err)
	}
	return buf.Bytes(), nil
}

// Validate checks the descriptor's values. Every error wraps
// ErrDescriptorInvalid.
func (d Descriptor) Validate() error {
	switch {
	case d.Schema != DescriptorSchema:
		return descriptorErr("schema %d is not %d", d.Schema, DescriptorSchema)
	case d.Plane != PlaneWorker:
		return descriptorErr("plane %q is not %q", d.Plane, PlaneWorker)
	case d.Contract != Contract:
		return descriptorErr("contract %d is unknown; this gc implements %d", d.Contract, Contract)
	case d.City == "":
		return descriptorErr("city is empty")
	case !ValidCityName(d.City):
		return descriptorErr("city is not [a-zA-Z0-9][a-zA-Z0-9._-]*")
	case !ValidSessionID(d.SessionID):
		return descriptorErr("session_id is not [A-Za-z0-9][A-Za-z0-9._-]{0,127}")
	case d.Generation < 1:
		return descriptorErr("generation %d is below 1", d.Generation)
	}
	path, ok := strings.CutPrefix(d.Credential, CredentialFilePrefix)
	if !ok {
		return descriptorErr("credential is not a %s reference", CredentialFilePrefix)
	}
	if !cleanAbs(path) {
		return descriptorErr("credential path is not a clean absolute path")
	}
	for i, l := range d.Ledgers {
		if err := l.validate(); err != nil {
			return descriptorErr("ledger[%d]: %v", i, err)
		}
	}
	shapes := make([]ledgerShape, len(d.Ledgers))
	for i, l := range d.Ledgers {
		shapes[i] = ledgerShape{scope: l.Scope, classes: l.Classes, url: l.URL, projectID: l.ProjectID}
	}
	if err := validateLedgerSet("ledger", shapes); err != nil {
		return descriptorErr("%v", err)
	}
	workspaceAt := map[string]int{}
	for i, l := range d.Ledgers {
		for _, w := range l.Workspaces {
			if j, dup := workspaceAt[w]; dup {
				return descriptorErr("ledger[%d]: workspace %q is also under ledger[%d]", i, w, j)
			}
			workspaceAt[w] = i
		}
	}
	return nil
}

// canonicalLedgers returns copies of ledgers in encode order: the city
// ledger first, then binding ledgers by scope, each with its classes in
// StorageClasses() order and its workspaces sorted.
func canonicalLedgers(ledgers []DescriptorLedger) []DescriptorLedger {
	out := make([]DescriptorLedger, len(ledgers))
	classOrder := StorageClasses()
	for i, l := range ledgers {
		l.Classes = slices.Clone(l.Classes)
		slices.SortFunc(l.Classes, func(a, b string) int {
			return slices.Index(classOrder, a) - slices.Index(classOrder, b)
		})
		l.Workspaces = slices.Sorted(slices.Values(l.Workspaces))
		out[i] = l
	}
	slices.SortFunc(out, func(a, b DescriptorLedger) int {
		switch {
		case a.Scope == b.Scope:
			return 0
		case a.Scope == ScopeCity:
			return -1
		case b.Scope == ScopeCity:
			return 1
		}
		return strings.Compare(a.Scope, b.Scope)
	})
	return out
}

func (l DescriptorLedger) validate() error {
	if err := validateScope(l.Scope); err != nil {
		return err
	}
	if err := validateClasses(l.Classes); err != nil {
		return err
	}
	if l.ProjectID == "" {
		return fmt.Errorf("project_id is empty")
	}
	if err := ValidateLedgerURL(l.URL, l.AllowPlaintext); err != nil {
		return err
	}
	if l.CAFile != "" && !cleanAbs(l.CAFile) {
		return fmt.Errorf("ca_file is not a clean absolute path")
	}
	if len(l.Workspaces) == 0 {
		return fmt.Errorf("workspaces is empty")
	}
	seen := make(map[string]bool, len(l.Workspaces))
	for _, w := range l.Workspaces {
		if !cleanAbs(w) {
			return fmt.Errorf("workspace %q is not a clean absolute path", w)
		}
		if seen[w] {
			return fmt.Errorf("workspace %q is listed twice", w)
		}
		seen[w] = true
	}
	return nil
}

func cleanAbs(p string) bool {
	return p != "" && filepath.IsAbs(p) && filepath.Clean(p) == p
}
