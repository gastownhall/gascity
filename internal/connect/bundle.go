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

// DecodeBundle parses a bundle. The schema version and contract are checked
// before the body, so a bundle from a newer controller is refused with
// CodeBundleVersion or CodeContractUnsupported even when its shape changed.
// The result is validated; every error is a *Refusal.
func DecodeBundle(data []byte) (Bundle, error) {
	var header struct {
		V        *int `json:"v"`
		Contract *int `json:"contract"`
	}
	if err := json.Unmarshal(data, &header); err != nil {
		return Bundle{}, &Refusal{Code: CodeBundleInvalid, Detail: "bundle is not a JSON object with integer v and contract", Err: err}
	}
	if header.V == nil {
		return Bundle{}, refuse(CodeBundleInvalid, "bundle has no v")
	}
	if *header.V != BundleVersion {
		return Bundle{}, refuse(CodeBundleVersion, fmt.Sprintf("bundle schema v%d; this gc decodes v%d", *header.V, BundleVersion))
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

// validateCertificatesPEM requires one or more CERTIFICATE blocks that parse
// as X.509 and nothing else but whitespace.
func validateCertificatesPEM(data []byte) error {
	rest := data
	count := 0
	for {
		var block *pem.Block
		block, rest = pem.Decode(rest)
		if block == nil {
			break
		}
		if block.Type != "CERTIFICATE" {
			return fmt.Errorf("PEM block %d is %q, not CERTIFICATE", count+1, block.Type)
		}
		if _, err := x509.ParseCertificate(block.Bytes); err != nil {
			return fmt.Errorf("PEM block %d: %w", count+1, err)
		}
		count++
	}
	if count == 0 {
		return fmt.Errorf("no PEM certificate")
	}
	if len(bytes.TrimSpace(rest)) != 0 {
		return fmt.Errorf("trailing data after PEM certificate %d", count)
	}
	return nil
}
