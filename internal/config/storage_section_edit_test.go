package config

import (
	"errors"
	"strings"
	"testing"
)

func TestRemoveStorageFromCityTOMLDeletesTheSectionInPlace(t *testing.T) {
	cases := []struct {
		name, in, want string
	}{
		{
			name: "classes and bindings, comments kept",
			in: `# the city
[workspace]
name = "c" # inline

# why we split
[storage.classes]
work = "work"
graph = "infra"

[storage.bindings.infra]
provider = "sqlite-beads"
path = ".gc/store"

# about the rigs
[[rigs]]
name = "a"
path = "/x"
`,
			want: `# the city
[workspace]
name = "c" # inline

# about the rigs
[[rigs]]
name = "a"
path = "/x"
`,
		},
		{
			name: "a bare [storage] header and a quoted binding name, last in the file",
			in: `[workspace]
name = "c"

[storage]
[storage.classes]
work = "work"
[storage.bindings."my infra"]
provider = "sqlite-beads"
`,
			want: `[workspace]
name = "c"
`,
		},
		{
			name: "no storage at all is unchanged",
			in:   "[workspace]\nname = \"c\"\n",
			want: "[workspace]\nname = \"c\"\n",
		},
		{
			name: "CRLF",
			in:   "[workspace]\r\nname = \"c\"\r\n\r\n[storage.classes]\r\nwork = \"work\"\r\n",
			want: "[workspace]\r\nname = \"c\"\r\n\r\n",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := RemoveStorageFromCityTOML([]byte(tc.in))
			if err != nil {
				t.Fatal(err)
			}
			if string(got) != tc.want {
				t.Fatalf("got:\n%q\nwant:\n%q", got, tc.want)
			}
		})
	}
}

func TestRemoveStorageFromCityTOMLRefusesWhatItCannotRemoveInPlace(t *testing.T) {
	for name, in := range map[string]string{
		"inline table": "storage = { classes = { work = \"work\" } }\n[workspace]\nname = \"c\"\n",
		"dotted key":   "storage.classes.work = \"work\"\n[workspace]\nname = \"c\"\n",
		"a header-shaped line in a multi-line string": "[workspace]\nname = \"c\"\nnote = \"\"\"\n[storage.classes]\n\"\"\"\n[storage.classes]\nwork = \"work\"\n",
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := RemoveStorageFromCityTOML([]byte(in)); !errors.Is(err, ErrSurgicalStorageEditUnsupported) {
				t.Fatalf("err = %v, want ErrSurgicalStorageEditUnsupported", err)
			}
		})
	}
}

func TestSetSessionReconcilerInCityTOML(t *testing.T) {
	in := "[workspace]\nname = \"c\"\n\n[daemon]\n# pinned\nsession_reconciler = \"v2\" # keep\nother = 1\n\n[agents]\n"
	got, err := SetSessionReconcilerInCityTOML([]byte(in), SessionReconcilerLegacy)
	if err != nil {
		t.Fatal(err)
	}
	want := strings.Replace(in, `"v2" # keep`, `"legacy" # keep`, 1)
	if string(got) != want {
		t.Fatalf("got:\n%s\nwant:\n%s", got, want)
	}
	for name, bad := range map[string]string{
		"no key":                    "[daemon]\nother = 1\n",
		"key in a fragment's table": "[workspace]\nsession_reconciler = \"v2\"\n",
		"inline daemon":             "daemon = { session_reconciler = \"v2\" }\n",
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := SetSessionReconcilerInCityTOML([]byte(bad), SessionReconcilerLegacy); err == nil {
				t.Fatal("rewrote a city.toml whose [daemon] session_reconciler it cannot find in place")
			}
		})
	}
}

func TestSameTOMLDocument(t *testing.T) {
	if !SameTOMLDocument([]byte("a = 1\n# c\n"), []byte("# other\na = 1\n")) {
		t.Fatal("two files that differ only in comments read as different")
	}
	if SameTOMLDocument([]byte("a = 1\n"), []byte("a = 2\n")) {
		t.Fatal("two different documents read as the same")
	}
	declared, err := CityTOMLDeclaresStorage([]byte("[storage.classes]\nwork = \"work\"\n"))
	if err != nil || !declared {
		t.Fatalf("CityTOMLDeclaresStorage = %v, %v", declared, err)
	}
	if declared, _ := CityTOMLDeclaresStorage([]byte("[workspace]\n")); declared {
		t.Fatal("a file without [storage] declares it")
	}
}
