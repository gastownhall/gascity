package connect

import (
	"bytes"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"net"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"time"
)

// BundleVersion is the bundle schema this binary encodes and the newest it
// decodes.
const BundleVersion = 1

// Contract is the worker-site contract this binary implements. Contract 1
// means Plane=Worker semantics, workspaces attached over http,
// journal-neutral boxes, the per-box bearer, and one ledger per served
// storage binding the box uses (DES-CN D16).
const Contract = 1

// ScopeCity is the scope of the ledger serving the reserved work binding:
// the city root's workspace. Every bundle and descriptor has exactly one.
const ScopeCity = "city"

// ScopeBindingPrefix prefixes the scope of a ledger serving a
// [storage.bindings.<name>] binding: "binding:<name>".
const ScopeBindingPrefix = "binding:"

// WorkBinding is the reserved storage binding the city ledger serves.
const WorkBinding = "work"

// Auth wait bounds for a ledger's auth_wait_s: how long `gc connect` retries
// a 401 from that ledger while its token file catches up (DES-CN D15).
const (
	// DefaultAuthWaitSeconds applies when auth_wait_s is 0 or absent.
	DefaultAuthWaitSeconds = 5
	// MaxAuthWaitSeconds is the largest auth_wait_s a bundle may carry.
	MaxAuthWaitSeconds = 120
)

// Storage class names, as config.StorageClasses' toml keys spell them.
const (
	classWork      = "work"
	classGraph     = "graph"
	classSessions  = "sessions"
	classMessaging = "messaging"
	classOrders    = "orders"
	classNudges    = "nudges"
)

// StorageClasses returns the six storage classes in config order. A
// contract-1 bundle and descriptor assign each to exactly one ledger.
func StorageClasses() []string {
	return []string{classWork, classGraph, classSessions, classMessaging, classOrders, classNudges}
}

// BindingScope returns the ledger scope of the named binding:
// "binding:<name>". It does not validate the name.
func BindingScope(name string) string {
	return ScopeBindingPrefix + name
}

// ScopeBinding returns the storage binding a ledger scope serves: the
// reserved work binding for ScopeCity, <name> for a valid "binding:<name>".
// It reports false for any other scope, including "binding:work".
func ScopeBinding(scope string) (binding string, ok bool) {
	if scope == ScopeCity {
		return WorkBinding, true
	}
	name, found := strings.CutPrefix(scope, ScopeBindingPrefix)
	if !found || !validBindingName(name) {
		return "", false
	}
	return name, true
}

var storageIdentifierRe = regexp.MustCompile(`^[A-Za-z0-9._-]+$`)

// validStorageIdentifier accepts the storage identifier charset
// (internal/config's validateStorageIdentifier) except an all-dots value,
// which would name a directory other than the binding's own. It matches the
// predicate beadsworkspace.WorkspaceRoot applies to a config_ref, so a bundle
// never names a workspace the box's provider refuses to open.
func validStorageIdentifier(s string) bool {
	return storageIdentifierRe.MatchString(s) && strings.Trim(s, ".") != ""
}

// validBindingName accepts a non-reserved binding name.
func validBindingName(name string) bool {
	return validStorageIdentifier(name) && name != WorkBinding
}

// Bundle is the secret document a controller mints for one session
// incarnation and a box consumes with `gc connect`. Unknown JSON fields are
// ignored within a schema version, so additive optional fields need no bump.
type Bundle struct {
	V        int           `json:"v"`
	Contract int           `json:"contract"`
	City     string        `json:"city"`
	MintedAt time.Time     `json:"minted_at"` // diagnostics only; never compared
	Session  BundleSession `json:"session"`
	Bearer   string        `json:"bearer"`
	Ledgers  []Ledger      `json:"ledgers"`
}

// BundleSession identifies the session incarnation the bundle was minted for.
type BundleSession struct {
	ID                  string `json:"id"`
	Generation          int    `json:"generation"`
	InstanceTokenSHA256 string `json:"instance_token_sha256"`
}

// Ledger is one ledger server the box attaches its workspaces to: the city
// ledger (the reserved work binding) or one served storage binding (DES-CN
// D16).
type Ledger struct {
	Scope string `json:"scope"` // ScopeCity or BindingScope(name)
	// Classes are the storage classes this ledger serves.
	Classes []string `json:"classes"`
	// ConfigRef names a binding ledger's workspace directory,
	// <city>/.gc/storage/<config_ref>; binding scopes only.
	ConfigRef      string `json:"config_ref,omitempty"`
	URL            string `json:"url"`
	ProjectID      string `json:"project_id"`
	CAPEM          string `json:"ca_pem,omitempty"`
	AllowPlaintext bool   `json:"allow_plaintext,omitempty"`
	RequireJournal bool   `json:"require_journal,omitempty"`
	// AuthWaitSeconds is how long `gc connect` retries a 401 from this
	// ledger; 0 means DefaultAuthWaitSeconds.
	AuthWaitSeconds int `json:"auth_wait_s,omitempty"`
}

// AuthWait returns the ledger's 401 retry window.
func (l Ledger) AuthWait() time.Duration {
	if l.AuthWaitSeconds == 0 {
		return DefaultAuthWaitSeconds * time.Second
	}
	return time.Duration(l.AuthWaitSeconds) * time.Second
}

var sha256HexRe = regexp.MustCompile(`^[0-9a-f]{64}$`)

// EncodeBundle validates b and marshals it to JSON.
func EncodeBundle(b Bundle) ([]byte, error) {
	if err := b.Validate(); err != nil {
		return nil, err
	}
	data, err := json.Marshal(b)
	if err != nil {
		return nil, &Refusal{Code: CodeBundleInvalid, Detail: "marshaling bundle", Err: err}
	}
	return data, nil
}

// DecodeBundle parses a bundle. The schema version is decoded alone and
// checked first, then the contract, then the body, so a bundle from a newer
// controller is refused with CodeBundleVersion or CodeContractUnsupported
// even when its shape, including the contract's type, changed.
// The result is validated; every error is a *Refusal.
func DecodeBundle(data []byte) (Bundle, error) {
	var version struct {
		V *int `json:"v"`
	}
	if err := json.Unmarshal(data, &version); err != nil {
		return Bundle{}, &Refusal{Code: CodeBundleInvalid, Detail: "bundle is not a JSON object with an integer v", Err: err}
	}
	if version.V == nil {
		return Bundle{}, refuse(CodeBundleInvalid, "bundle has no v")
	}
	if *version.V != BundleVersion {
		return Bundle{}, refuse(CodeBundleVersion, fmt.Sprintf("bundle schema v%d; this gc decodes v%d", *version.V, BundleVersion))
	}
	var header struct {
		Contract *int `json:"contract"`
	}
	if err := json.Unmarshal(data, &header); err != nil {
		return Bundle{}, &Refusal{Code: CodeBundleInvalid, Detail: "bundle contract is not an integer", Err: err}
	}
	if header.Contract == nil {
		return Bundle{}, refuse(CodeBundleInvalid, "bundle has no contract")
	}
	if *header.Contract != Contract {
		return Bundle{}, refuse(CodeContractUnsupported, fmt.Sprintf("bundle asks for contract %d; this gc implements %d", *header.Contract, Contract))
	}
	var b Bundle
	if err := json.Unmarshal(data, &b); err != nil {
		return Bundle{}, &Refusal{Code: CodeBundleInvalid, Detail: "decoding bundle", Err: err}
	}
	if err := b.Validate(); err != nil {
		return Bundle{}, err
	}
	return b, nil
}

// Validate checks the bundle's structural rules and returns a *Refusal on
// the first violation. Details never echo the bearer or a URL.
func (b Bundle) Validate() error {
	if b.V != BundleVersion {
		return refuse(CodeBundleVersion, fmt.Sprintf("bundle schema v%d; this gc decodes v%d", b.V, BundleVersion))
	}
	if b.Contract != Contract {
		return refuse(CodeContractUnsupported, fmt.Sprintf("bundle asks for contract %d; this gc implements %d", b.Contract, Contract))
	}
	if b.City == "" {
		return refuse(CodeBundleInvalid, "city is empty")
	}
	if !ValidCityName(b.City) {
		return refuse(CodeBundleInvalid, "city is not [a-zA-Z0-9][a-zA-Z0-9._-]*")
	}
	if b.Session.ID == "" {
		return refuse(CodeBundleInvalid, "session.id is empty")
	}
	if !ValidSessionID(b.Session.ID) {
		return refuse(CodeBundleInvalid, "session.id is not [A-Za-z0-9][A-Za-z0-9._-]{0,127}")
	}
	if b.Session.Generation < 1 {
		return refuse(CodeBundleInvalid, fmt.Sprintf("session.generation %d is below 1", b.Session.Generation))
	}
	if !sha256HexRe.MatchString(b.Session.InstanceTokenSHA256) {
		return refuse(CodeBundleInvalid, "session.instance_token_sha256 is not 64 lowercase hex characters")
	}
	if b.Bearer == "" {
		return refuse(CodeBundleInvalid, "bearer is empty")
	}
	id, gen, ok := ParseBearer(b.Bearer)
	if !ok {
		return refuse(CodeBundleInvalid, "bearer is not a gcb1 bearer")
	}
	if id != b.Session.ID || gen != b.Session.Generation {
		return refuse(CodeBundleInvalid, "bearer does not match session.id and session.generation")
	}
	for i, l := range b.Ledgers {
		if err := l.validate(); err != nil {
			return refuse(CodeBundleInvalid, fmt.Sprintf("ledgers[%d]: %v", i, err))
		}
	}
	shapes := make([]ledgerShape, len(b.Ledgers))
	for i, l := range b.Ledgers {
		shapes[i] = ledgerShape{scope: l.Scope, classes: l.Classes, url: l.URL, projectID: l.ProjectID, configRef: l.ConfigRef}
	}
	if err := validateLedgerSet("ledgers", shapes); err != nil {
		return refuse(CodeBundleInvalid, err.Error())
	}
	return nil
}

func (l Ledger) validate() error {
	if err := validateScope(l.Scope); err != nil {
		return err
	}
	switch {
	case l.Scope == ScopeCity && l.ConfigRef != "":
		return fmt.Errorf("config_ref is set on the %q ledger", ScopeCity)
	case l.Scope != ScopeCity && l.ConfigRef == "":
		return fmt.Errorf("config_ref is empty on a binding ledger")
	case l.Scope != ScopeCity && !validStorageIdentifier(l.ConfigRef):
		return fmt.Errorf("config_ref is not [A-Za-z0-9._-]+ or is only dots")
	}
	if err := validateClasses(l.Classes); err != nil {
		return err
	}
	if l.AuthWaitSeconds < 0 || l.AuthWaitSeconds > MaxAuthWaitSeconds {
		return fmt.Errorf("auth_wait_s %d is not 0 or 1 to %d", l.AuthWaitSeconds, MaxAuthWaitSeconds)
	}
	if l.ProjectID == "" {
		return fmt.Errorf("project_id is empty")
	}
	if err := ValidateLedgerURL(l.URL, l.AllowPlaintext); err != nil {
		return err
	}
	if l.CAPEM != "" {
		if err := validateCertificatesPEM([]byte(l.CAPEM)); err != nil {
			return fmt.Errorf("ca_pem: %w", err)
		}
	}
	return nil
}

// ValidateLedgerURL checks a ledger URL against the connect URL rules: https;
// or http to a loopback host; or http anywhere when allowPlaintext is set (a
// tailnet path). Loopback mirrors bd's insecure-credential guard
// (configfile.IsLocalHostString), which sends a credential over plain http
// only to the literal hosts localhost, 127.0.0.1 and ::1; any other http host,
// including the rest of 127/8 and IPv4-mapped or expanded loopback forms,
// needs allowPlaintext. The host must be non-empty, and userinfo, a query,
// and a fragment are refused. Errors never echo the URL, which may hold
// userinfo.
func ValidateLedgerURL(raw string, allowPlaintext bool) error {
	if raw == "" {
		return fmt.Errorf("url is empty")
	}
	if strings.ContainsAny(raw, "?#") {
		return fmt.Errorf("url carries a query or fragment")
	}
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("url does not parse")
	}
	switch {
	case u.Opaque != "":
		return fmt.Errorf("url is not hierarchical")
	case u.User != nil:
		return fmt.Errorf("url carries userinfo")
	case u.RawQuery != "" || u.ForceQuery:
		return fmt.Errorf("url carries a query")
	case u.Fragment != "":
		return fmt.Errorf("url carries a fragment")
	case u.Hostname() == "":
		return fmt.Errorf("url has no host")
	}
	switch u.Scheme {
	case "https":
		return nil
	case "http":
		if allowPlaintext || isLoopbackHost(u.Hostname()) {
			return nil
		}
		return fmt.Errorf("url is http to a non-loopback host without allow_plaintext")
	default:
		return fmt.Errorf("url scheme %q is not https or http", u.Scheme)
	}
}

// isLoopbackHost reports whether bd treats host as local for an http
// credential: exactly localhost (any case), 127.0.0.1 or ::1.
func isLoopbackHost(host string) bool {
	switch strings.ToLower(host) {
	case "localhost", "127.0.0.1", "::1":
		return true
	}
	return false
}

// validateScope accepts ScopeCity or a valid "binding:<name>". An
// unrecognized scope is not echoed: it is untrusted bundle text and may hold
// a URL with userinfo.
func validateScope(scope string) error {
	if _, ok := ScopeBinding(scope); !ok {
		return fmt.Errorf("scope is neither %q nor %s<name> with a non-reserved [A-Za-z0-9._-]+ name that is not only dots", ScopeCity, ScopeBindingPrefix)
	}
	return nil
}

// validateClasses checks one ledger's class list: non-empty, every entry a
// storage class, none twice. An unknown class is named by index, not echoed.
func validateClasses(classes []string) error {
	if len(classes) == 0 {
		return fmt.Errorf("classes is empty")
	}
	known := StorageClasses()
	seen := make(map[string]bool, len(classes))
	for i, c := range classes {
		if !slices.Contains(known, c) {
			return fmt.Errorf("classes[%d] is not a storage class", i)
		}
		if seen[c] {
			return fmt.Errorf("class %q is listed twice", c)
		}
		seen[c] = true
	}
	return nil
}

// ledgerShape is the part of a bundle or descriptor ledger the cross-ledger
// rules read.
type ledgerShape struct {
	scope     string
	classes   []string
	url       string
	projectID string
	// configRef is a bundle binding ledger's config_ref; the descriptor,
	// which names workspaces by path instead, leaves it empty.
	configRef string
}

// validateLedgerSet applies the cross-ledger rules (DES-CN §3.2, §3.6) to
// ledgers that each passed their own checks: at least one ledger and exactly
// one city ledger; unique scopes, URLs (compared by ledgerURLKey), project
// ids and non-empty config_refs; every storage class on exactly one ledger,
// with work on the city ledger. Errors name ledgers by index and never echo
// a URL.
func validateLedgerSet(field string, ledgers []ledgerShape) error {
	if len(ledgers) == 0 {
		return fmt.Errorf("no ledgers")
	}
	scopeAt := map[string]int{}
	urlAt := map[string]int{}
	projectAt := map[string]int{}
	configRefAt := map[string]int{}
	classAt := map[string]int{}
	for i, l := range ledgers {
		if j, dup := scopeAt[l.scope]; dup {
			return fmt.Errorf("%s[%d]: scope %q duplicates %s[%d]", field, i, l.scope, field, j)
		}
		scopeAt[l.scope] = i
		key := ledgerURLKey(l.url)
		if j, dup := urlAt[key]; dup {
			return fmt.Errorf("%s[%d]: url duplicates %s[%d]'s", field, i, field, j)
		}
		urlAt[key] = i
		if j, dup := projectAt[l.projectID]; dup {
			return fmt.Errorf("%s[%d]: project_id duplicates %s[%d]'s", field, i, field, j)
		}
		projectAt[l.projectID] = i
		if l.configRef != "" {
			if j, dup := configRefAt[l.configRef]; dup {
				return fmt.Errorf("%s[%d]: config_ref duplicates %s[%d]'s", field, i, field, j)
			}
			configRefAt[l.configRef] = i
		}
		for _, c := range l.classes {
			if j, dup := classAt[c]; dup {
				return fmt.Errorf("%s[%d]: class %q is also on %s[%d]", field, i, c, field, j)
			}
			classAt[c] = i
		}
	}
	city, ok := scopeAt[ScopeCity]
	if !ok {
		return fmt.Errorf("no %q ledger", ScopeCity)
	}
	if at, ok := classAt[classWork]; ok && at != city {
		return fmt.Errorf("%s[%d]: class %q is not on the %q ledger", field, at, classWork, ScopeCity)
	}
	for _, c := range StorageClasses() {
		if _, ok := classAt[c]; !ok {
			return fmt.Errorf("class %q is on no ledger", c)
		}
	}
	return nil
}

// ledgerURLKey returns the form two ledger URLs are compared in: scheme and
// host lowercased, the scheme's default port made explicit, and trailing
// slashes trimmed from the path, which is kept so path-mounted ledgers on
// one host stay distinct. raw has passed ValidateLedgerURL.
func ledgerURLKey(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return raw
	}
	scheme := strings.ToLower(u.Scheme)
	port := u.Port()
	if port == "" {
		port = defaultPorts[scheme]
	}
	return scheme + "://" + net.JoinHostPort(strings.ToLower(u.Hostname()), port) + strings.TrimRight(u.EscapedPath(), "/")
}

// defaultPorts are the ports a ledger URL without one dials.
var defaultPorts = map[string]string{"https": "443", "http": "80"}

// pemBegin opens every PEM block.
var pemBegin = []byte("-----BEGIN ")

// validateCertificatesPEM requires one or more CERTIFICATE blocks that parse
// as X.509, separated and surrounded by nothing but whitespace. pem.Decode
// skips text before a block, including a broken BEGIN line, so each block
// must start right after the whitespace and consume exactly one BEGIN line.
func validateCertificatesPEM(data []byte) error {
	rest := bytes.TrimLeft(data, pemWhitespace)
	count := 0
	for len(rest) > 0 {
		if !bytes.HasPrefix(rest, pemBegin) {
			return fmt.Errorf("non-PEM data before or after PEM certificate %d", count)
		}
		block, after := pem.Decode(rest)
		if block == nil {
			return fmt.Errorf("PEM block %d does not decode", count+1)
		}
		if consumed := rest[:len(rest)-len(after)]; bytes.Count(consumed, pemBegin) != 1 {
			return fmt.Errorf("PEM block %d follows a broken BEGIN line", count+1)
		}
		if block.Type != "CERTIFICATE" {
			return fmt.Errorf("PEM block %d is %q, not CERTIFICATE", count+1, block.Type)
		}
		if _, err := x509.ParseCertificate(block.Bytes); err != nil {
			return fmt.Errorf("PEM block %d: %w", count+1, err)
		}
		count++
		rest = bytes.TrimLeft(after, pemWhitespace)
	}
	if count == 0 {
		return fmt.Errorf("no PEM certificate")
	}
	return nil
}

const pemWhitespace = " \t\r\n"
