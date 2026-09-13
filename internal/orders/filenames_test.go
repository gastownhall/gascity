package orders

import "testing"

func TestTrimFlatOrderFilenameStripsSupportedSuffixes(t *testing.T) {
	cases := []struct {
		name string
		file string
		want string
		ok   bool
	}{
		{name: "plain TOML", file: "health-check.toml", want: "health-check", ok: true},
		{name: "infixed TOML", file: "health-check.order.toml", want: "health-check", ok: true},
		{name: "not an order file", file: "README.md"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := TrimFlatOrderFilename(tc.file)
			if ok != tc.ok {
				t.Fatalf("ok = %v, want %v", ok, tc.ok)
			}
			if got != tc.want {
				t.Fatalf("name = %q, want %q", got, tc.want)
			}
		})
	}
}
