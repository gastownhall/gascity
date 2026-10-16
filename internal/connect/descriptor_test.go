package connect

import (
	"bytes"
	"errors"
	"reflect"
	"slices"
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
		Ledgers: []DescriptorLedger{
			{
				Scope:          ScopeCity,
				Classes:        []string{"work"},
				URL:            "https://ctl.example.ts.net:7491",
				ProjectID:      "proj-7d2e",
				CAFile:         "/home/agent/.config/gascity/connect/maintainer-city/mc-abc12/ca-city.pem",
				AllowPlaintext: false,
				Workspaces:     []string{"/data/cities/mc", "/data/cities/mc/rigs/app"},
			},
			{
				Scope:      BindingScope("infra"),
				Classes:    []string{"graph", "sessions", "messaging", "orders", "nudges"},
				URL:        "https://infra.example.ts.net:7492",
				ProjectID:  "proj-91c0",
				Workspaces: []string{"/data/cities/mc/.gc/storage/infra"},
			},
		},
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
	if strings.Count(golden, "[[ledger]]") != 2 {
		t.Fatal("the golden must hold the city ledger and the binding:infra ledger")
	}
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
		"duplicate ledgers":        golden + "\n" + golden[ledgerAt:],
		"missing ledger classes":   drop(`classes = ["work"]`),
		"classes as string":        replace(`classes = ["work"]`, `classes = "work"`),
		"classes as ints":          replace(`classes = ["work"]`, `classes = [1]`),
		"empty classes":            replace(`classes = ["work"]`, `classes = []`),
		"unknown class":            replace(`classes = ["work"]`, `classes = ["work", "mail"]`),
		"case-variant classes key": replace(`classes = ["work"]`, `Classes = ["work"]`),
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

// TestDescriptorLedgerRules is DES-CN §10 CN-1c's cross-ledger table for the
// descriptor.
func TestDescriptorLedgerRules(t *testing.T) {
	infra := func(d *Descriptor) *DescriptorLedger { return &d.Ledgers[1] }
	refused := []struct {
		name   string
		mutate func(*Descriptor)
		want   string
	}{
		{"no ledgers", func(d *Descriptor) { d.Ledgers = nil }, "no ledgers"},
		{"two city ledgers", func(d *Descriptor) { infra(d).Scope = ScopeCity }, `ledger[1]: scope "city" duplicates ledger[0]`},
		{"no city ledger", func(d *Descriptor) { d.Ledgers[0].Scope = BindingScope("primary") }, `no "city" ledger`},
		{"duplicate binding scope", func(d *Descriptor) {
			infra(d).Classes = []string{"graph", "sessions", "messaging", "orders"}
			d.Ledgers = append(d.Ledgers, DescriptorLedger{
				Scope: BindingScope("infra"), Classes: []string{"nudges"}, URL: "https://other.example.ts.net",
				ProjectID: "proj-other", Workspaces: []string{"/data/cities/mc/.gc/storage/other"},
			})
		}, `ledger[2]: scope "binding:infra" duplicates ledger[1]`},
		{"empty binding name", func(d *Descriptor) { infra(d).Scope = "binding:" }, "ledger[1]: scope is neither"},
		{"reserved binding name", func(d *Descriptor) { infra(d).Scope = "binding:work" }, "ledger[1]: scope is neither"},
		{"dot-dot binding name", func(d *Descriptor) { infra(d).Scope = "binding:.." }, "ledger[1]: scope is neither"},
		{"all-dots binding name", func(d *Descriptor) { infra(d).Scope = "binding:..." }, "ledger[1]: scope is neither"},
		{"slash in binding name", func(d *Descriptor) { infra(d).Scope = "binding:a/b" }, "ledger[1]: scope is neither"},
		{"bare binding", func(d *Descriptor) { infra(d).Scope = "infra" }, "ledger[1]: scope is neither"},
		{"rig scope", func(d *Descriptor) { infra(d).Scope = "rig:app" }, "ledger[1]: scope is neither"},
		{"empty classes on a binding beside a full city", func(d *Descriptor) {
			d.Ledgers[0].Classes = StorageClasses()
			infra(d).Classes = nil
		}, "ledger[1]: classes is empty"},
		{"class in two ledgers", func(d *Descriptor) { d.Ledgers[0].Classes = []string{"work", "graph"} }, `ledger[1]: class "graph" is also on ledger[0]`},
		{"work off the city ledger", func(d *Descriptor) {
			d.Ledgers[0].Classes, infra(d).Classes = []string{"graph"}, []string{"work", "sessions", "messaging", "orders", "nudges"}
		}, `ledger[1]: class "work" is not on the "city" ledger`},
		{"class missing", func(d *Descriptor) { infra(d).Classes = []string{"graph"} }, "is on no ledger"},
		{"class twice in one ledger", func(d *Descriptor) { infra(d).Classes = append(infra(d).Classes, "orders") }, `ledger[1]: class "orders" is listed twice`},
		{"duplicate url", func(d *Descriptor) { infra(d).URL = d.Ledgers[0].URL }, "ledger[1]: url duplicates ledger[0]'s"},
		{"duplicate project id", func(d *Descriptor) { infra(d).ProjectID = d.Ledgers[0].ProjectID }, "ledger[1]: project_id duplicates ledger[0]'s"},
		{"workspace under two ledgers", func(d *Descriptor) { infra(d).Workspaces = []string{"/data/cities/mc"} }, "ledger[1]: workspace"},
		{"second ledger plaintext url", func(d *Descriptor) { infra(d).URL = "http://infra.example.ts.net:7492" }, "ledger[1]: url is http"},
	}
	for _, tc := range refused {
		t.Run(tc.name, func(t *testing.T) {
			d := validDescriptor()
			tc.mutate(&d)
			err := d.Validate()
			if !errors.Is(err, ErrDescriptorInvalid) {
				t.Fatalf("Validate = %v, want ErrDescriptorInvalid", err)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("Validate = %v, want it to contain %q", err, tc.want)
			}
			if _, err := EncodeDescriptor(d); !errors.Is(err, ErrDescriptorInvalid) {
				t.Fatalf("EncodeDescriptor = %v, want ErrDescriptorInvalid", err)
			}
		})
	}
	cityOnly := validDescriptor()
	cityOnly.Ledgers = cityOnly.Ledgers[:1]
	cityOnly.Ledgers[0].Classes = StorageClasses()
	if err := cityOnly.Validate(); err != nil {
		t.Fatalf("a city-only descriptor refused: %v", err)
	}
}

// TestEncodeDescriptorCanonicalOrder: ledgers, classes and workspaces encode
// in canonical order whatever order they were found in.
func TestEncodeDescriptorCanonicalOrder(t *testing.T) {
	want, err := EncodeDescriptor(validDescriptor())
	if err != nil {
		t.Fatal(err)
	}
	d := validDescriptor()
	d.Ledgers = append(d.Ledgers, DescriptorLedger{
		Scope: BindingScope("a-queue"), Classes: []string{"nudges"}, URL: "https://q.example.ts.net",
		ProjectID: "proj-q", Workspaces: []string{"/data/cities/mc/.gc/storage/q"},
	})
	d.Ledgers[1].Classes = []string{"orders", "messaging", "graph", "sessions"}
	slices.Reverse(d.Ledgers)
	got, err := EncodeDescriptor(d)
	if err != nil {
		t.Fatal(err)
	}
	text := string(got)
	city, queue, infra := strings.Index(text, `scope = "city"`), strings.Index(text, `scope = "binding:a-queue"`), strings.Index(text, `scope = "binding:infra"`)
	if city < 0 || city > queue || queue > infra {
		t.Fatalf("ledgers not in canonical order:\n%s", text)
	}
	if !strings.Contains(text, `classes = ["graph", "sessions", "messaging", "orders"]`) {
		t.Fatalf("classes not in StorageClasses() order:\n%s", text)
	}
	if d.Ledgers[0].Scope != BindingScope("a-queue") || d.Ledgers[1].Classes[0] != "orders" {
		t.Fatal("EncodeDescriptor mutated its argument")
	}
	decoded, err := DecodeDescriptor(got)
	if err != nil {
		t.Fatal(err)
	}
	again, err := EncodeDescriptor(decoded)
	if err != nil || !bytes.Equal(again, got) {
		t.Fatalf("canonical round trip is not byte-identical (%v):\n%s", err, again)
	}

	shuffled := validDescriptor()
	slices.Reverse(shuffled.Ledgers)
	slices.Reverse(shuffled.Ledgers[0].Classes)
	if got, err := EncodeDescriptor(shuffled); err != nil || !bytes.Equal(got, want) {
		t.Fatalf("shuffled golden encodes differently (%v):\n%s", err, got)
	}
}
