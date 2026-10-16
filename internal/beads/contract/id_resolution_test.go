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
