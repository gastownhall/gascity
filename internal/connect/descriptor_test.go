package connect

import (
	"bytes"
	"errors"
	"reflect"
	"strings"
	"testing"
)

func validDescriptor() Descriptor {
	return Descriptor{
		Schema:     DescriptorSchema,
		Plane:      PlaneWorker,
		City:       "maintainer-city",
		SessionID:  "mc-abc12",
		Generation: 7,
		Contract:   Contract,
		Credential: "file:/home/agent/.config/gascity/connect/maintainer-city/mc-abc12/bearer",
		Ledgers: []DescriptorLedger{{
			Scope:          ScopeCity,
			URL:            "https://ctl.example.ts.net:7491",
			ProjectID:      "proj-7d2e",
			CAFile:         "/home/agent/.config/gascity/connect/maintainer-city/mc-abc12/ca-city.pem",
			AllowPlaintext: false,
			Workspaces:     []string{"/data/cities/mc", "/data/cities/mc/rigs/app"},
		}},
	}
}

func TestEncodeDescriptorGolden(t *testing.T) {
	got, err := EncodeDescriptor(validDescriptor())
	if err != nil {
		t.Fatalf("EncodeDescriptor: %v", err)
	}
	want := readTestdata(t, "control-plane.golden.toml")
	if !bytes.Equal(got, want) {
		t.Fatalf("descriptor drifted from testdata/control-plane.golden.toml:\n%s", got)
	}
}

func TestDescriptorDeterministicRoundTrip(t *testing.T) {
	first, err := EncodeDescriptor(validDescriptor())
	if err != nil {
		t.Fatalf("EncodeDescriptor: %v", err)
	}
	decoded, err := DecodeDescriptor(first)
	if err != nil {
		t.Fatalf("DecodeDescriptor: %v", err)
	}
	if !reflect.DeepEqual(decoded, validDescriptor()) {
		t.Fatalf("round trip = %+v, want %+v", decoded, validDescriptor())
	}
	second, err := EncodeDescriptor(decoded)
	if err != nil {
		t.Fatalf("EncodeDescriptor(decoded): %v", err)
	}
	if !bytes.Equal(first, second) {
		t.Fatalf("encoding is not deterministic:\n%s\n---\n%s", first, second)
	}

	shuffled := validDescriptor()
	shuffled.Ledgers[0].Workspaces = []string{"/data/cities/mc/rigs/app", "/data/cities/mc"}
	third, err := EncodeDescriptor(shuffled)
	if err != nil {
		t.Fatalf("EncodeDescriptor(shuffled): %v", err)
	}
	if !bytes.Equal(first, third) {
		t.Fatalf("workspace order leaked into the encoding:\n%s", third)
	}
	if shuffled.Ledgers[0].Workspaces[0] != "/data/cities/mc/rigs/app" {
		t.Fatal("EncodeDescriptor mutated its argument")
	}
}

func TestDescriptorEncodingHasNoSecretsOrTimestamps(t *testing.T) {
	got, err := EncodeDescriptor(validDescriptor())
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{BearerPrefix, SentinelPrefix, "minted", "_at", "time"} {
		if strings.Contains(string(got), forbidden) {
			t.Errorf("descriptor contains %q:\n%s", forbidden, got)
		}
	}
}

func TestDecodeDescriptorOptionalCAFile(t *testing.T) {
	d := validDescriptor()
	d.Ledgers[0].CAFile = ""
	data, err := EncodeDescriptor(d)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "ca_file") {
		t.Fatalf("empty ca_file was encoded:\n%s", data)
	}
	got, err := DecodeDescriptor(data)
	if err != nil {
		t.Fatalf("DecodeDescriptor without ca_file: %v", err)
	}
	if !reflect.DeepEqual(got, d) {
		t.Fatalf("got %+v, want %+v", got, d)
	}
}

func TestDecodeDescriptorStrictRefusals(t *testing.T) {
	golden := string(readTestdata(t, "control-plane.golden.toml"))
	replace := func(old, repl string) string {
		t.Helper()
		if !strings.Contains(golden, old) {
			t.Fatalf("golden lacks %q", old)
		}
		return strings.Replace(golden, old, repl, 1)
	}
	drop := func(line string) string { return replace(line+"\n", "") }
	ledgerAt := strings.Index(golden, "[[ledger]]")
	cases := map[string]string{
		"empty":                    "",
		"not toml":                 "schema = = 1",
		"unknown top-level key":    "extra = 1\n" + golden,
		"unknown ledger key":       golden + "extra = true\n",
		"unknown table":            golden + "\n[extra]\nx = 1\n",
		"unknown array table":      golden + "\n[[other]]\nx = 1\n",
		"duplicate key":            "schema = 1\n" + golden,
		"missing schema":           drop("schema = 1"),
		"missing plane":            drop(`plane = "worker"`),
		"missing city":             drop(`city = "maintainer-city"`),
		"missing session_id":       drop(`session_id = "mc-abc12"`),
		"missing generation":       drop("generation = 7"),
		"missing contract":         drop("contract = 1"),
		"missing credential":       drop(`credential = "file:/home/agent/.config/gascity/connect/maintainer-city/mc-abc12/bearer"`),
		"missing ledger":           golden[:ledgerAt],
		"missing ledger scope":     drop(`scope = "city"`),
		"missing ledger url":       drop(`url = "https://ctl.example.ts.net:7491"`),
		"missing ledger project":   drop(`project_id = "proj-7d2e"`),
		"missing allow_plaintext":  drop("allow_plaintext = false"),
		"missing workspaces":       drop(`workspaces = ["/data/cities/mc", "/data/cities/mc/rigs/app"]`),
		"schema 2":                 replace("schema = 1", "schema = 2"),
		"schema 0":                 replace("schema = 1", "schema = 0"),
		"plane local":              replace(`plane = "worker"`, `plane = "local"`),
		"contract 2":               replace("contract = 1", "contract = 2"),
		"env credential":           replace(`credential = "file:`, `credential = "env:X`),
		"command credential":       replace(`credential = "file:`, `credential = "command:`),
		"bare credential":          replace(`credential = "file:`, `credential = "`),
		"relative credential path": replace(`credential = "file:/`, `credential = "file:`),
		"schema as string":         replace("schema = 1", `schema = "1"`),
		"generation as string":     replace("generation = 7", `generation = "7"`),
		"generation as float":      replace("generation = 7", "generation = 7.0"),
		"plane as int":             replace(`plane = "worker"`, "plane = 1"),
		"allow_plaintext as str":   replace("allow_plaintext = false", `allow_plaintext = "false"`),
		"workspaces as string":     replace(`workspaces = ["/data/cities/mc", "/data/cities/mc/rigs/app"]`, `workspaces = "/data/cities/mc"`),
		"ledger as table":          replace("[[ledger]]", "[ledger]"),
		"zero generation":          replace("generation = 7", "generation = 0"),
		"bad session id":           replace(`session_id = "mc-abc12"`, `session_id = "mc abc"`),
		"empty city":               replace(`city = "maintainer-city"`, `city = ""`),
		"traversal city":           replace(`city = "maintainer-city"`, `city = "../../etc"`),
		"slash city":               replace(`city = "maintainer-city"`, `city = "/"`),
		"control-character city":   replace(`city = "maintainer-city"`, `city = "mc\u0007"`),
		"rig scope":                replace(`scope = "city"`, `scope = "rig"`),
		"empty project":            replace(`project_id = "proj-7d2e"`, `project_id = ""`),
		"plaintext url":            replace(`url = "https://`, `url = "http://`),
		"empty ca_file":            replace(`ca_file = "/home/agent/.config/gascity/connect/maintainer-city/mc-abc12/ca-city.pem"`, `ca_file = ""`),
		"relative ca_file":         replace(`ca_file = "/home`, `ca_file = "home`),
		"empty workspaces":         replace(`workspaces = ["/data/cities/mc", "/data/cities/mc/rigs/app"]`, "workspaces = []"),
		"relative workspace":       replace(`"/data/cities/mc/rigs/app"`, `"rigs/app"`),
		"unclean workspace":        replace(`"/data/cities/mc/rigs/app"`, `"/data/cities/mc/rigs/../rigs/app"`),
		"duplicate workspace":      replace(`"/data/cities/mc/rigs/app"`, `"/data/cities/mc"`),
		"two ledgers":              golden + "\n" + golden[ledgerAt:],
		"case-variant key":         replace(`plane = "worker"`, `PLANE = "worker"`),
		"case-variant schema":      replace("schema = 1", "Schema = 1"),
		"case-variant ledger key":  replace(`ca_file = "/home`, `CA_FILE = "/home`),
		"case-variant table":       replace("[[ledger]]", "[[Ledger]]"),
		"case-variant beside key":  replace("generation = 7", "generation = 7\nGeneration = 9"),
		"case-variant session id":  replace(`session_id = "mc-abc12"`, `session_id = "mc-abc12"`+"\n"+`Session_ID = "mc-other"`),
		"case-variant ledger pair": golden + "\n" + strings.Replace(golden[ledgerAt:], "[[ledger]]", "[[Ledger]]", 1),
	}
	for name, input := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := DecodeDescriptor([]byte(input))
			if err == nil {
				t.Fatalf("DecodeDescriptor accepted:\n%s", input)
			}
			if !errors.Is(err, ErrDescriptorInvalid) {
				t.Fatalf("error %v does not wrap ErrDescriptorInvalid", err)
			}
		})
	}
}

func TestEncodeDescriptorRefusesInvalid(t *testing.T) {
	d := validDescriptor()
	d.Credential = "env:GC_BEARER"
	if _, err := EncodeDescriptor(d); !errors.Is(err, ErrDescriptorInvalid) {
		t.Fatalf("EncodeDescriptor(env credential) = %v, want ErrDescriptorInvalid", err)
	}
}

func TestValidCityName(t *testing.T) {
	for _, name := range []string{"maintainer-city", "mc", "A.b_c-9", "9city", "a.."} {
		if !ValidCityName(name) {
			t.Errorf("ValidCityName(%q) = false, want true", name)
		}
	}
	for _, name := range []string{"", ".", "..", "../../etc", "/", "a/b", "a\\b", ".hidden", "-x", "_x", "a b", "mc\n", "mc\x00", "cité"} {
		if ValidCityName(name) {
			t.Errorf("ValidCityName(%q) = true, want false", name)
		}
	}
}
