package connect

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"
)

const testDigest = "9f2c0000000000000000000000000000000000000000000000000000000000ab"

func readTestdata(t *testing.T, name string) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatalf("reading testdata %s: %v", name, err)
	}
	return data
}

func validBundle(t *testing.T) Bundle {
	t.Helper()
	return Bundle{
		V:        BundleVersion,
		Contract: Contract,
		City:     "maintainer-city",
		MintedAt: time.Date(2026, time.October, 13, 12, 0, 0, 0, time.UTC),
		Session: BundleSession{
			ID:                  "mc-abc12",
			Generation:          7,
			InstanceTokenSHA256: testDigest,
		},
		Bearer: mustMint(t, "maintainer-city", "mc-abc12", 7),
		Ledgers: []Ledger{
			{
				Scope:     ScopeCity,
				Classes:   []string{"work"},
				URL:       "https://ctl.example.ts.net:7491",
				ProjectID: "proj-7d2e",
				CAPEM:     string(readTestdata(t, "ca.pem")),
			},
			infraLedger(),
		},
	}
}

// infraLedger is maintainer-city's infra binding served beside remote
// Postgres and rendered by a sidecar (DES-CN D15, D16).
func infraLedger() Ledger {
	return Ledger{
		Scope:           BindingScope("infra"),
		Classes:         []string{"graph", "sessions", "messaging", "orders", "nudges"},
		ConfigRef:       "infra",
		URL:             "https://infra.example.ts.net:7492",
		ProjectID:       "proj-91c0",
		AuthWaitSeconds: 20,
	}
}

// cityOnlyBundle is a bundle whose one city ledger serves every class.
func cityOnlyBundle(t *testing.T) Bundle {
	t.Helper()
	b := validBundle(t)
	b.Ledgers = b.Ledgers[:1]
	b.Ledgers[0].Classes = StorageClasses()
	return b
}

func requireCode(t *testing.T, err error, want Code) {
	t.Helper()
	if err == nil {
		t.Fatalf("got nil error, want %s", want)
	}
	var refusal *Refusal
	if !errors.As(err, &refusal) {
		t.Fatalf("error %v (%T) is not a *Refusal", err, err)
	}
	if refusal.Code != want {
		t.Fatalf("code = %s (%v), want %s", refusal.Code, err, want)
	}
}

func TestEncodeBundleGolden(t *testing.T) {
	got, err := EncodeBundle(validBundle(t))
	if err != nil {
		t.Fatalf("EncodeBundle: %v", err)
	}
	var indented bytes.Buffer
	if err := json.Indent(&indented, got, "", "  "); err != nil {
		t.Fatalf("indenting: %v", err)
	}
	indented.WriteByte('\n')
	want := readTestdata(t, "bundle_v1.golden.json")
	if !bytes.Equal(indented.Bytes(), want) {
		t.Fatalf("bundle JSON drifted from testdata/bundle_v1.golden.json:\n%s", indented.Bytes())
	}
}

func TestDecodeBundleGoldenRoundTrip(t *testing.T) {
	b, err := DecodeBundle(readTestdata(t, "bundle_v1.golden.json"))
	if err != nil {
		t.Fatalf("DecodeBundle(golden): %v", err)
	}
	want := validBundle(t)
	if b.City != want.City || b.Session != want.Session || b.Bearer != want.Bearer ||
		!b.MintedAt.Equal(want.MintedAt) || !reflect.DeepEqual(b.Ledgers, want.Ledgers) {
		t.Fatalf("decoded bundle = %+v, want %+v", b, want)
	}
}

func TestDecodeBundleIgnoresUnknownFields(t *testing.T) {
	data, err := EncodeBundle(validBundle(t))
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatal(err)
	}
	m["future_optional"] = map[string]any{"x": 1}
	extended, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := DecodeBundle(extended); err != nil {
		t.Fatalf("DecodeBundle with an additive field: %v", err)
	}
}

func TestDecodeBundleVersionAndContractRefusals(t *testing.T) {
	cases := []struct {
		name string
		body string
		want Code
	}{
		{"not json", `not json`, CodeBundleInvalid},
		{"array", `[]`, CodeBundleInvalid},
		{"no v", `{"contract":1}`, CodeBundleInvalid},
		{"string v", `{"v":"1","contract":1}`, CodeBundleInvalid},
		{"v0", `{"v":0,"contract":1}`, CodeBundleVersion},
		{"v2 with a changed shape", `{"v":2,"contract":1,"ledgers":"elsewhere"}`, CodeBundleVersion},
		{"v2 with a changed contract type", `{"v":2,"contract":"two"}`, CodeBundleVersion},
		{"v2 with an object contract", `{"v":2,"contract":{"id":2}}`, CodeBundleVersion},
		{"no contract", `{"v":1}`, CodeBundleInvalid},
		{"string contract", `{"v":1,"contract":"1"}`, CodeBundleInvalid},
		{"contract 2", `{"v":1,"contract":2}`, CodeContractUnsupported},
		{"contract 0", `{"v":1,"contract":0}`, CodeContractUnsupported},
		{"v1 contract 1 wrong body type", `{"v":1,"contract":1,"session":"x"}`, CodeBundleInvalid},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := DecodeBundle([]byte(tc.body))
			requireCode(t, err, tc.want)
		})
	}
}

func TestBundleValidateRefusals(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*Bundle)
		want   Code
	}{
		{"v2", func(b *Bundle) { b.V = 2 }, CodeBundleVersion},
		{"contract 2", func(b *Bundle) { b.Contract = 2 }, CodeContractUnsupported},
		{"empty city", func(b *Bundle) { b.City = "" }, CodeBundleInvalid},
		{"traversal city", func(b *Bundle) { b.City = "../../etc" }, CodeBundleInvalid},
		{"dot-dot city", func(b *Bundle) { b.City = ".." }, CodeBundleInvalid},
		{"slash city", func(b *Bundle) { b.City = "a/b" }, CodeBundleInvalid},
		{"control-character city", func(b *Bundle) { b.City = "mc\n" }, CodeBundleInvalid},
		{"empty session id", func(b *Bundle) { b.Session.ID = "" }, CodeBundleInvalid},
		{"session id charset", func(b *Bundle) { b.Session.ID = "mc abc" }, CodeBundleInvalid},
		{"zero generation", func(b *Bundle) { b.Session.Generation = 0 }, CodeBundleInvalid},
		{"empty digest", func(b *Bundle) { b.Session.InstanceTokenSHA256 = "" }, CodeBundleInvalid},
		{"short digest", func(b *Bundle) { b.Session.InstanceTokenSHA256 = testDigest[:63] }, CodeBundleInvalid},
		{"uppercase digest", func(b *Bundle) { b.Session.InstanceTokenSHA256 = strings.ToUpper(testDigest) }, CodeBundleInvalid},
		{"non-hex digest", func(b *Bundle) { b.Session.InstanceTokenSHA256 = "g" + testDigest[1:] }, CodeBundleInvalid},
		{"empty bearer", func(b *Bundle) { b.Bearer = "" }, CodeBundleInvalid},
		{"malformed bearer", func(b *Bundle) { b.Bearer = "gcb1.mc-abc12.7.short" }, CodeBundleInvalid},
		{"sentinel as bearer", func(b *Bundle) { b.Bearer = mustSentinel(t, testKey(), "maintainer-city") }, CodeBundleInvalid},
		{"bearer for another session", func(b *Bundle) { b.Bearer = mustMint(t, "maintainer-city", "mc-other", 7) }, CodeBundleInvalid},
		{"bearer for another generation", func(b *Bundle) { b.Bearer = mustMint(t, "maintainer-city", "mc-abc12", 8) }, CodeBundleInvalid},
		{"no ledgers", func(b *Bundle) { b.Ledgers = nil }, CodeBundleInvalid},
		{"duplicate city ledger", func(b *Bundle) { b.Ledgers = append(b.Ledgers, b.Ledgers[0]) }, CodeBundleInvalid},
		{"rig scope", func(b *Bundle) { b.Ledgers[0].Scope = "rig" }, CodeBundleInvalid},
		{"empty scope", func(b *Bundle) { b.Ledgers[0].Scope = "" }, CodeBundleInvalid},
		{"empty project", func(b *Bundle) { b.Ledgers[0].ProjectID = "" }, CodeBundleInvalid},
		{"bad PEM", func(b *Bundle) { b.Ledgers[0].CAPEM = "not a certificate" }, CodeBundleInvalid},
		{"PEM with a corrupt certificate", func(b *Bundle) {
			b.Ledgers[0].CAPEM = "-----BEGIN CERTIFICATE-----\nAAAA\n-----END CERTIFICATE-----\n"
		}, CodeBundleInvalid},
		{"PEM private key", func(b *Bundle) {
			b.Ledgers[0].CAPEM = "-----BEGIN PRIVATE KEY-----\nAAAA\n-----END PRIVATE KEY-----\n"
		}, CodeBundleInvalid},
		{"PEM with trailing garbage", func(b *Bundle) { b.Ledgers[0].CAPEM += "garbage\n" }, CodeBundleInvalid},
		{"PEM with leading garbage", func(b *Bundle) { b.Ledgers[0].CAPEM = "garbage\n" + b.Ledgers[0].CAPEM }, CodeBundleInvalid},
		{"PEM with garbage between blocks", func(b *Bundle) {
			b.Ledgers[0].CAPEM = b.Ledgers[0].CAPEM + "garbage\n" + b.Ledgers[0].CAPEM
		}, CodeBundleInvalid},
		{"PEM after a broken BEGIN line", func(b *Bundle) {
			b.Ledgers[0].CAPEM = "-----BEGIN CERTIFICATE-----\n" + b.Ledgers[0].CAPEM
		}, CodeBundleInvalid},
		{"insecure URL", func(b *Bundle) { b.Ledgers[0].URL = "http://ctl.example.ts.net:7491" }, CodeBundleInvalid},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			b := validBundle(t)
			b.Ledgers = append([]Ledger(nil), b.Ledgers...)
			tc.mutate(&b)
			requireCode(t, b.Validate(), tc.want)
			_, err := EncodeBundle(b)
			requireCode(t, err, tc.want)
		})
	}
}

func TestBundleValidateAccepts(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*Bundle)
	}{
		{"no CA", func(b *Bundle) { b.Ledgers[0].CAPEM = "" }},
		{"CA chain of two", func(b *Bundle) { b.Ledgers[0].CAPEM += b.Ledgers[0].CAPEM }},
		{"CA with surrounding whitespace", func(b *Bundle) {
			b.Ledgers[0].CAPEM = "\n \t\r\n" + b.Ledgers[0].CAPEM + "\n\n" + b.Ledgers[0].CAPEM + "  \n"
		}},
		{"dotted session id", func(b *Bundle) {
			b.Session.ID = "mc.abc.12"
			b.Bearer = mustMint(t, "maintainer-city", "mc.abc.12", 7)
		}},
		{"require journal", func(b *Bundle) { b.Ledgers[0].RequireJournal = true }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			b := validBundle(t)
			tc.mutate(&b)
			if err := b.Validate(); err != nil {
				t.Fatalf("Validate: %v", err)
			}
		})
	}
}

func TestValidateLedgerURLTable(t *testing.T) {
	cases := []struct {
		url            string
		allowPlaintext bool
		ok             bool
	}{
		{"https://ctl.example.ts.net:7491", false, true},
		{"https://ctl.example.ts.net", false, true},
		{"https://ctl.example.ts.net/bd", false, true},
		{"https://10.0.0.5:7491", false, true},
		{"https://[fd7a::1]:7491", false, true},
		{"http://127.0.0.1:7491", false, true},
		{"http://127.9.9.9:7491", false, false},
		{"http://127.9.9.9:7491", true, true},
		{"http://127.0.0.2:7491", false, false},
		{"http://[::ffff:127.0.0.1]:7491", false, false},
		{"http://[::ffff:127.0.0.1]:7491", true, true},
		{"http://[0:0:0:0:0:0:0:1]:7491", false, false},
		{"http://[0:0:0:0:0:0:0:1]:7491", true, true},
		{"http://0.0.0.0:7491", false, false},
		{"http://LocalHost:7491", false, true},
		{"http://[::1]:7491", false, true},
		{"http://localhost:7491", false, true},
		{"http://LOCALHOST:7491", false, true},
		{"http://ctl.example.ts.net:7491", false, false},
		{"http://100.64.0.7:7491", false, false},
		{"http://ctl.example.ts.net:7491", true, true},
		{"http://100.64.0.7:7491", true, true},
		{"https://user@ctl.example.ts.net", false, false},
		{"https://user:pass@ctl.example.ts.net", false, false},
		{"http://user@127.0.0.1", true, false},
		{"https://ctl.example.ts.net/?token=x", false, false},
		{"https://ctl.example.ts.net/?", false, false},
		{"https://ctl.example.ts.net/#frag", false, false},
		{"https://ctl.example.ts.net/#", false, false},
		{"https://", false, false},
		{"https://:7491", false, false},
		{"http:///path", true, false},
		{"", false, false},
		{"ctl.example.ts.net:7491", false, false},
		{"ftp://ctl.example.ts.net", true, false},
		{"unix:///run/bd.sock", true, false},
		{"https:ctl.example.ts.net", false, false},
		{"https://ctl.example.ts.net:7491/%zz", false, false},
	}
	for _, tc := range cases {
		err := ValidateLedgerURL(tc.url, tc.allowPlaintext)
		if (err == nil) != tc.ok {
			t.Errorf("ValidateLedgerURL(%q, allow_plaintext=%v) = %v, want ok=%v", tc.url, tc.allowPlaintext, err, tc.ok)
		}
	}
}

func TestValidateLedgerURLNeverEchoesUserinfo(t *testing.T) {
	err := ValidateLedgerURL("https://user:hunter2@ctl.example.ts.net", false)
	if err == nil || strings.Contains(err.Error(), "hunter2") {
		t.Fatalf("error %v must refuse without echoing the password", err)
	}
	b := validBundle(t)
	b.Ledgers[0].URL = "https://user:hunter2@ctl.example.ts.net"
	if err := b.Validate(); err == nil || strings.Contains(err.Error(), "hunter2") {
		t.Fatalf("Validate error %v must refuse without echoing the password", err)
	}
}

func TestBundleRefusalsNeverEchoTheBearer(t *testing.T) {
	b := validBundle(t)
	other := mustMint(t, "maintainer-city", "mc-other", 7)
	b.Bearer = other
	err := b.Validate()
	if err == nil || strings.Contains(err.Error(), other[strings.LastIndexByte(other, '.')+1:]) {
		t.Fatalf("Validate error %v must refuse without echoing the bearer MAC", err)
	}
}

// leakSentinels are fixture fragments no refusal may carry: ledger hosts and
// a userinfo password. Refusals name ledgers by index instead.
var leakSentinels = []string{"example.ts.net", "127.0.0.1", "hunter2", "https://", "http://"}

// requireNoLeak fails when err carries a ledger URL, host or password.
func requireNoLeak(t *testing.T, err error, ledgerURLs []string) {
	t.Helper()
	for _, s := range append(ledgerURLs, leakSentinels...) {
		if s != "" && strings.Contains(err.Error(), s) {
			t.Fatalf("refusal %v echoes %q", err, s)
		}
	}
}

// TestBundleLedgerRules is DES-CN §10 CN-1c's bundle-ledger table. Each
// refused row names the refusal fragment that pins the rule it exercises.
func TestBundleLedgerRules(t *testing.T) {
	second := func(b *Bundle) *Ledger { return &b.Ledgers[1] }
	const leakyValue = "https://u:hunter2@leak.example.ts.net"
	refused := []struct {
		name   string
		mutate func(*Bundle)
		want   string
	}{
		{"no ledgers", func(b *Bundle) { b.Ledgers = nil }, "no ledgers"},
		{"two city ledgers", func(b *Bundle) {
			second(b).Scope, second(b).ConfigRef = ScopeCity, ""
		}, `ledgers[1]: scope "city" duplicates ledgers[0]`},
		{"no city ledger", func(b *Bundle) {
			b.Ledgers[0] = infraLedger()
			b.Ledgers[0].Scope, b.Ledgers[0].ConfigRef = BindingScope("work2"), "work2"
			b.Ledgers[0].URL, b.Ledgers[0].ProjectID = "https://w.example.ts.net", "proj-w"
			b.Ledgers[0].Classes = []string{"work"}
		}, `no "city" ledger`},
		{"duplicate binding scope", func(b *Bundle) {
			dup := infraLedger()
			dup.ConfigRef = "infra2"
			dup.URL, dup.ProjectID, dup.Classes = "https://other.example.ts.net", "proj-other", []string{"nudges"}
			second(b).Classes = []string{"graph", "sessions", "messaging", "orders"}
			b.Ledgers = append(b.Ledgers, dup)
		}, `ledgers[2]: scope "binding:infra" duplicates ledgers[1]`},
		{"duplicate config_ref", func(b *Bundle) {
			dup := infraLedger()
			dup.Scope = BindingScope("infra2")
			dup.URL, dup.ProjectID, dup.Classes = "https://other.example.ts.net", "proj-other", []string{"nudges"}
			second(b).Classes = []string{"graph", "sessions", "messaging", "orders"}
			b.Ledgers = append(b.Ledgers, dup)
		}, "ledgers[2]: config_ref duplicates ledgers[1]'s"},
		{"empty binding name", func(b *Bundle) { second(b).Scope = "binding:" }, "ledgers[1]: scope is neither"},
		{"reserved binding name", func(b *Bundle) { second(b).Scope = "binding:work" }, "ledgers[1]: scope is neither"},
		{"dot-dot binding name", func(b *Bundle) { second(b).Scope = "binding:.." }, "ledgers[1]: scope is neither"},
		{"dot binding name", func(b *Bundle) { second(b).Scope = "binding:." }, "ledgers[1]: scope is neither"},
		{"all-dots binding name", func(b *Bundle) { second(b).Scope = "binding:..." }, "ledgers[1]: scope is neither"},
		{"slash in binding name", func(b *Bundle) { second(b).Scope = "binding:a/b" }, "ledgers[1]: scope is neither"},
		{"rig scope", func(b *Bundle) { second(b).Scope = "rig:app" }, "ledgers[1]: scope is neither"},
		{"bare binding", func(b *Bundle) { second(b).Scope = "infra" }, "ledgers[1]: scope is neither"},
		{"scope holding a URL", func(b *Bundle) { second(b).Scope = leakyValue }, "ledgers[1]: scope is neither"},
		{"config_ref missing on a binding", func(b *Bundle) { second(b).ConfigRef = "" }, "ledgers[1]: config_ref is empty"},
		{"config_ref on city", func(b *Bundle) { b.Ledgers[0].ConfigRef = "work" }, `ledgers[0]: config_ref is set on the "city" ledger`},
		{"config_ref dot", func(b *Bundle) { second(b).ConfigRef = "." }, "ledgers[1]: config_ref is not"},
		{"config_ref dot-dot", func(b *Bundle) { second(b).ConfigRef = ".." }, "ledgers[1]: config_ref is not"},
		{"config_ref all dots", func(b *Bundle) { second(b).ConfigRef = "..." }, "ledgers[1]: config_ref is not"},
		{"config_ref with a slash", func(b *Bundle) { second(b).ConfigRef = "a/b" }, "ledgers[1]: config_ref is not"},
		{"empty classes on a binding beside a full city", func(b *Bundle) {
			b.Ledgers[0].Classes = StorageClasses()
			second(b).Classes = nil
		}, "ledgers[1]: classes is empty"},
		{"empty class list on city", func(b *Bundle) {
			b.Ledgers[0].Classes = []string{}
			second(b).Classes = StorageClasses()
		}, "ledgers[0]: classes is empty"},
		{"unknown class", func(b *Bundle) { second(b).Classes = append(second(b).Classes, "mail") }, "ledgers[1]: classes[5] is not a storage class"},
		{"class holding a URL", func(b *Bundle) { second(b).Classes = []string{leakyValue} }, "ledgers[1]: classes[0] is not a storage class"},
		{"class twice in one ledger", func(b *Bundle) { second(b).Classes = append(second(b).Classes, "graph") }, `ledgers[1]: class "graph" is listed twice`},
		{"class in two ledgers", func(b *Bundle) { b.Ledgers[0].Classes = []string{"work", "graph"} }, `ledgers[1]: class "graph" is also on ledgers[0]`},
		{"work off the city ledger", func(b *Bundle) {
			b.Ledgers[0].Classes = []string{"graph"}
			second(b).Classes = []string{"work", "sessions", "messaging", "orders", "nudges"}
		}, `ledgers[1]: class "work" is not on the "city" ledger`},
		{"class missing from the union", func(b *Bundle) { second(b).Classes = []string{"graph", "sessions", "messaging", "orders"} }, `class "nudges" is on no ledger`},
		{"duplicate url", func(b *Bundle) { second(b).URL = b.Ledgers[0].URL }, "ledgers[1]: url duplicates ledgers[0]'s"},
		{"duplicate url modulo spelling", func(b *Bundle) { second(b).URL = "HTTPS://CTL.Example.ts.net:7491/" }, "ledgers[1]: url duplicates ledgers[0]'s"},
		{"duplicate url modulo default port", func(b *Bundle) {
			b.Ledgers[0].URL, second(b).URL = "https://ctl.example.ts.net/bd", "https://ctl.example.ts.net:443/bd/"
		}, "ledgers[1]: url duplicates ledgers[0]'s"},
		{"duplicate project id", func(b *Bundle) { second(b).ProjectID = b.Ledgers[0].ProjectID }, "ledgers[1]: project_id duplicates ledgers[0]'s"},
		{"auth_wait_s -1", func(b *Bundle) { second(b).AuthWaitSeconds = -1 }, "ledgers[1]: auth_wait_s -1"},
		{"auth_wait_s 121", func(b *Bundle) { second(b).AuthWaitSeconds = MaxAuthWaitSeconds + 1 }, "ledgers[1]: auth_wait_s 121"},
		{"second ledger plaintext url", func(b *Bundle) { second(b).URL = "http://infra.example.ts.net:7492" }, "ledgers[1]: url is http"},
		{"second ledger userinfo url", func(b *Bundle) { second(b).URL = leakyValue }, "ledgers[1]: url carries userinfo"},
		{"second ledger empty project", func(b *Bundle) { second(b).ProjectID = "" }, "ledgers[1]: project_id is empty"},
		{"second ledger bad PEM", func(b *Bundle) { second(b).CAPEM = "not a certificate" }, "ledgers[1]: ca_pem"},
	}
	for _, tc := range refused {
		t.Run(tc.name, func(t *testing.T) {
			b := validBundle(t)
			tc.mutate(&b)
			err := b.Validate()
			requireCode(t, err, CodeBundleInvalid)
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("refusal %v does not contain %q", err, tc.want)
			}
			urls := make([]string, len(b.Ledgers))
			for i, l := range b.Ledgers {
				urls[i] = l.URL
			}
			requireNoLeak(t, err, urls)
		})
	}

	accepted := []struct {
		name   string
		bundle func(*testing.T) Bundle
	}{
		{"one city ledger with every class", cityOnlyBundle},
		{"city plus binding:infra", validBundle},
		{"three ledgers", func(t *testing.T) Bundle {
			b := validBundle(t)
			b.Ledgers[1].Classes = []string{"graph", "sessions", "messaging", "orders"}
			b.Ledgers = append(b.Ledgers, Ledger{
				Scope: BindingScope("queue.v2"), Classes: []string{"nudges"}, ConfigRef: "queue.v2",
				URL: "http://127.0.0.1:7493", ProjectID: "proj-q", AuthWaitSeconds: MaxAuthWaitSeconds,
			})
			return b
		}},
		{"path-mounted ledgers on one host", func(t *testing.T) Bundle {
			b := validBundle(t)
			b.Ledgers[0].URL, b.Ledgers[1].URL = "https://edge.example.ts.net/city", "https://edge.example.ts.net/infra"
			return b
		}},
		{"dotted binding name and config_ref", func(t *testing.T) Bundle {
			b := validBundle(t)
			b.Ledgers[1].Scope, b.Ledgers[1].ConfigRef = BindingScope("..infra"), "infra.."
			return b
		}},
		{"binding ledger first", func(t *testing.T) Bundle {
			b := validBundle(t)
			slices.Reverse(b.Ledgers)
			return b
		}},
		{"auth_wait_s 1", func(t *testing.T) Bundle {
			b := validBundle(t)
			b.Ledgers[0].AuthWaitSeconds = 1
			return b
		}},
	}
	for _, tc := range accepted {
		t.Run(tc.name, func(t *testing.T) {
			b := tc.bundle(t)
			if err := b.Validate(); err != nil {
				t.Fatalf("Validate: %v", err)
			}
			data, err := EncodeBundle(b)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := DecodeBundle(data); err != nil {
				t.Fatalf("DecodeBundle: %v", err)
			}
		})
	}
}

// TestDecodeBundleRefusesCN1aShape: a bundle shaped as CN-1a merged it, with
// no classes, is not a contract-1 bundle any more.
func TestDecodeBundleRefusesCN1aShape(t *testing.T) {
	data, err := EncodeBundle(cityOnlyBundle(t))
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatal(err)
	}
	delete(m["ledgers"].([]any)[0].(map[string]any), "classes")
	legacy, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	_, err = DecodeBundle(legacy)
	requireCode(t, err, CodeBundleInvalid)
	if !strings.Contains(err.Error(), "ledgers[0]") {
		t.Fatalf("refusal %v does not name the ledger", err)
	}
}

func TestLedgerAuthWait(t *testing.T) {
	cases := map[int]time.Duration{0: 5 * time.Second, 1: time.Second, 20: 20 * time.Second, MaxAuthWaitSeconds: 120 * time.Second}
	for secs, want := range cases {
		if got := (Ledger{AuthWaitSeconds: secs}).AuthWait(); got != want {
			t.Errorf("AuthWait(%d) = %s, want %s", secs, got, want)
		}
	}
	if DefaultAuthWaitSeconds != 5 || MaxAuthWaitSeconds != 120 {
		t.Fatal("auth wait bounds drifted from DES-CN §3.2")
	}
}

func TestScopeBinding(t *testing.T) {
	if b, ok := ScopeBinding(ScopeCity); !ok || b != "work" {
		t.Fatalf("ScopeBinding(city) = %q, %v", b, ok)
	}
	for _, name := range []string{"infra", "a", "infra.v2", "A_b-9", "..x", "x.."} {
		if got, ok := ScopeBinding(BindingScope(name)); !ok || got != name {
			t.Errorf("ScopeBinding(BindingScope(%q)) = %q, %v", name, got, ok)
		}
	}
	for _, scope := range []string{"", "binding:", "binding:work", "binding:.", "binding:..", "binding:...", "binding:a/b", "binding:a b", "rig:app", "infra", "Binding:infra", "City"} {
		if got, ok := ScopeBinding(scope); ok {
			t.Errorf("ScopeBinding(%q) = %q, true", scope, got)
		}
	}
}
