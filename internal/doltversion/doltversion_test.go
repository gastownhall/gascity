package doltversion

import (
	"errors"
	"testing"
)

func TestParse(t *testing.T) {
	tests := []struct {
		name       string
		input      string
		wantRaw    string
		wantMajor  int
		wantMinor  int
		wantPatch  int
		wantPreRel bool
		wantErr    bool
	}{
		{
			name:      "dolt version prefix",
			input:     "dolt version 1.86.2",
			wantRaw:   "1.86.2",
			wantMajor: 1,
			wantMinor: 86,
			wantPatch: 2,
		},
		{
			name:      "build metadata",
			input:     "dolt version 1.86.2+build-5",
			wantRaw:   "1.86.2",
			wantMajor: 1,
			wantMinor: 86,
			wantPatch: 2,
		},
		{
			name:       "pre-release",
			input:      "dolt version 1.99.0-rc1",
			wantRaw:    "1.99.0-rc1",
			wantMajor:  1,
			wantMinor:  99,
			wantPatch:  0,
			wantPreRel: true,
		},
		{
			name:       "pre-release with build metadata",
			input:      "dolt version 1.86.2-rc1+build.5",
			wantRaw:    "1.86.2-rc1+build.5",
			wantMajor:  1,
			wantMinor:  86,
			wantPatch:  2,
			wantPreRel: true,
		},
		{
			name:    "missing patch",
			input:   "dolt version 1.86",
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := Parse(tt.input)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("Parse(%q) error = nil, want error", tt.input)
				}
				return
			}
			if err != nil {
				t.Fatalf("Parse(%q) error = %v", tt.input, err)
			}
			if got.Raw != tt.wantRaw || got.Major != tt.wantMajor || got.Minor != tt.wantMinor ||
				got.Patch != tt.wantPatch || got.PreRelease != tt.wantPreRel {
				t.Fatalf("Parse(%q) = %+v, want raw=%q major=%d minor=%d patch=%d prerelease=%v",
					tt.input, got, tt.wantRaw, tt.wantMajor, tt.wantMinor, tt.wantPatch, tt.wantPreRel)
			}
		})
	}
}

func TestCheckFinalMinimum(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		wantErr error
	}{
		{name: "at floor", input: "dolt version 1.86.2"},
		{name: "above floor", input: "dolt version 1.86.10"},
		{name: "below floor", input: "dolt version 1.86.1", wantErr: ErrBelowMinimum},
		{name: "pre-release at floor", input: "dolt version 1.86.2-rc1", wantErr: ErrPreRelease},
		{name: "pre-release with build metadata at floor", input: "dolt version 1.86.2-rc1+build.5", wantErr: ErrPreRelease},
		{name: "pre-release above floor", input: "dolt version 2.0.0-rc1", wantErr: ErrPreRelease},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := CheckFinalMinimum(tt.input, ManagedMin)
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("CheckFinalMinimum(%q) error = %v, want %v", tt.input, err, tt.wantErr)
			}
		})
	}
}
