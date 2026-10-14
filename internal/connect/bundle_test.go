package connect

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
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
		Ledgers: []Ledger{{
			Scope:     ScopeCity,
			URL:       "https://ctl.example.ts.net:7491",
			ProjectID: "proj-7d2e",
			CAPEM:     string(readTestdata(t, "ca.pem")),
		}},
	}
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
		!b.MintedAt.Equal(want.MintedAt) || len(b.Ledgers) != 1 || b.Ledgers[0] != want.Ledgers[0] {
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
		{"two ledgers", func(b *Bundle) { b.Ledgers = append(b.Ledgers, b.Ledgers[0]) }, CodeBundleInvalid},
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
