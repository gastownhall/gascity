package connect

import (
	"bytes"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"net/url"
	"regexp"
	"strings"
	"time"
)

// BundleVersion is the bundle schema this binary encodes and the newest it
// decodes.
const BundleVersion = 1

// Contract is the worker-site contract this binary implements. Contract 1
// means Plane=Worker semantics, workspaces attached over http,
// journal-neutral boxes, and the per-box bearer.
const Contract = 1

// ScopeCity is the only ledger scope contract 1 allows.
const ScopeCity = "city"

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

// Ledger is one ledger server the box attaches its workspaces to.
type Ledger struct {
	Scope          string `json:"scope"` // "city" in contract 1
	URL            string `json:"url"`
	ProjectID      string `json:"project_id"`
	CAPEM          string `json:"ca_pem,omitempty"`
	AllowPlaintext bool   `json:"allow_plaintext,omitempty"`
	RequireJournal bool   `json:"require_journal,omitempty"`
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
	if len(b.Ledgers) != 1 {
		return refuse(CodeBundleInvalid, fmt.Sprintf("contract %d requires exactly one ledger, got %d", Contract, len(b.Ledgers)))
	}
	for i, l := range b.Ledgers {
		if err := l.validate(); err != nil {
			return refuse(CodeBundleInvalid, fmt.Sprintf("ledgers[%d]: %v", i, err))
		}
	}
	return nil
}

func (l Ledger) validate() error {
	if l.Scope != ScopeCity {
		return fmt.Errorf("scope %q is not %q", l.Scope, ScopeCity)
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
