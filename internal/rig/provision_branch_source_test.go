package rig

import "testing"

// The banner's default-branch suffix is the only place an operator learns
// where the rig's mainline came from. A rig registered against a feature
// branch is silently wrong (gas-4cu), so the "inferred" wording — and the
// remote name when a remote HEAD answered — is load-bearing output, not
// decoration. Pin the three arms.
func TestDefaultBranchSourceSuffix(t *testing.T) {
	tests := []struct {
		name     string
		override string
		remote   string
		want     string
	}{
		{
			name:     "explicit override needs no note",
			override: "release",
			remote:   "",
			want:     "",
		},
		{
			name:     "override wins even when a remote answered",
			override: "release",
			remote:   "ajb",
			want:     "",
		},
		{
			name:   "probed remote is named so a non-origin remote is visible",
			remote: "ajb",
			want:   " (from ajb/HEAD)",
		},
		{
			name:   "origin is named like any other remote",
			remote: "origin",
			want:   " (from origin/HEAD)",
		},
		{
			name: "checked-out-branch fallback says the branch was inferred",
			want: " (inferred from the checked-out branch; no remote HEAD is set — pass --default-branch if this is wrong)",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := defaultBranchSourceSuffix(tt.override, tt.remote); got != tt.want {
				t.Errorf("defaultBranchSourceSuffix(%q, %q) = %q, want %q", tt.override, tt.remote, got, tt.want)
			}
		})
	}
}
