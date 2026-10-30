package formula

import "testing"

func TestTrimTOMLFilenameStripsSupportedSuffixes(t *testing.T) {
	cases := []struct {
		name string
		file string
		want string
		ok   bool
	}{
		{name: "plain TOML", file: "build-review.toml", want: "build-review", ok: true},
		{name: "infixed TOML", file: "build-review.formula.toml", want: "build-review", ok: true},
		{name: "JSON is not a TOML filename", file: "build-review.formula.json"},
		{name: "not a formula file", file: "README.md"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := TrimTOMLFilename(tc.file)
			if ok != tc.ok {
				t.Fatalf("ok = %v, want %v", ok, tc.ok)
			}
			if got != tc.want {
				t.Fatalf("name = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestTrimFormulaExtStripsEveryResolvableSpelling(t *testing.T) {
	for file, want := range map[string]string{
		"build-review":              "build-review",
		"build-review.toml":         "build-review",
		"build-review.formula.toml": "build-review",
		"build-review.formula.json": "build-review",
		"README.md":                 "README.md",
	} {
		if got := TrimFormulaExt(file); got != want {
			t.Errorf("TrimFormulaExt(%q) = %q, want %q", file, got, want)
		}
	}
}
