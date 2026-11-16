package execenv

import "testing"

// TestExecRequestKeyPlatformComparison pins that a GC_*_INFO request key is
// recognized the way the platform compares environment names: under any
// spelling on Windows, where gc_grant_info is the helper's $env:GC_GRANT_INFO,
// and only as written elsewhere, where the two are different variables.
func TestExecRequestKeyPlatformComparison(t *testing.T) {
	for _, tc := range []struct {
		key, goos string
		want      bool
	}{
		{"GC_EXEC_INFO", "linux", true},
		{"GC_GRANT_INFO", "windows", true},
		{"gc_grant_info", "windows", true},
		{"Gc_Exec_Info", "windows", true},
		{"gc_grant_info", "linux", false},
		{"GC_INFO_PATH", "windows", false},
		{"GC_TOKEN", "windows", false},
		{"XGC_EXEC_INFO", "windows", false},
	} {
		if got := isExecRequestKeyForGOOS(tc.key, tc.goos); got != tc.want {
			t.Errorf("isExecRequestKeyForGOOS(%q, %q) = %t, want %t", tc.key, tc.goos, got, tc.want)
		}
	}
}
