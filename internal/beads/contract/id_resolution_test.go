package contract

import "testing"

func TestBackendResolvesIDsExactly(t *testing.T) {
	cases := []struct {
		backend string
		want    bool
	}{
		{backend: "http", want: true},
		{backend: "\thttp\n", want: true},
		{backend: "HTTP", want: false},
		{backend: "dolt", want: false},
		{backend: "doltlite", want: false},
		{backend: "postgres", want: false},
		{backend: "sqlite", want: false},
		{backend: "", want: false},
	}
	for _, tc := range cases {
		if got := BackendResolvesIDsExactly(tc.backend); got != tc.want {
			t.Errorf("BackendResolvesIDsExactly(%q) = %v, want %v", tc.backend, got, tc.want)
		}
	}
}

func TestLooksLikePrefixedID(t *testing.T) {
	cases := []struct {
		id   string
		want bool
	}{
		{id: "demo-abc", want: true},
		{id: "demo-wisp-dv78", want: true},
		{id: "ga-abc.1", want: true},
		{id: "12345678-0bc", want: true},
		// Everything below fails bd's shape test, so its exact-only resolver
		// may retry the id as <issue_prefix>-<id>.
		{id: "abc", want: false},
		{id: "", want: false},
		{id: "-abc", want: false},
		{id: "demo-", want: false},
		{id: "123456789-abc", want: false},
		{id: "gcg--9223372036841971822", want: false},
		{id: "Demo-abc", want: false},
		{id: "demo-Abc", want: false},
		{id: "de_mo-abc", want: false},
		{id: "démo-abc", want: false},
	}
	for _, tc := range cases {
		if got := LooksLikePrefixedID(tc.id); got != tc.want {
			t.Errorf("LooksLikePrefixedID(%q) = %v, want %v", tc.id, got, tc.want)
		}
	}
}
