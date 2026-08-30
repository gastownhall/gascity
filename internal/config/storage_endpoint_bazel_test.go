package config

import (
	"strings"
	"testing"
)

func TestValidateStorageEndpointURLBazel(t *testing.T) {
	tests := []struct {
		name, value string
		wantErr     bool
	}{
		{"https", "https://beads.example/workspaces", false},
		{"http", "http://localhost:8080", false},
		{"scheme", "ftp://beads.example", true},
		{"host", "https:///missing-host", true},
		{"userinfo", "https://user:secret@beads.example", true},
		{"query", "https://beads.example/?token=secret", true},
		{"fragment", "https://beads.example/#secret", true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateStorageEndpointURL(tc.value)
			if (err != nil) != tc.wantErr {
				t.Fatalf("ValidateStorageEndpointURL(%q) error = %v, wantErr %v", tc.value, err, tc.wantErr)
			}
			if err != nil && strings.Contains(err.Error(), "secret") {
				t.Fatalf("error leaked credential-shaped input: %v", err)
			}
		})
	}
}

func TestValidateStorageAuthReferenceBazel(t *testing.T) {
	long := strings.Repeat("x", storageAuthMaxLength+1)
	tests := []struct {
		name, value string
		wantErr     bool
	}{
		{"provider", StorageAuthCredentialProvider, false},
		{"env", "env:BEADS_TOKEN", false},
		{"env underscore", "env:_private2", false},
		{"env malformed", "env:1INVALID", true},
		{"length", long, true},
		{"whitespace", "env:BEADS TOKEN", true},
		{"url material", "https://user:secret@example", true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateStorageAuthReference(tc.value)
			if (err != nil) != tc.wantErr {
				t.Fatalf("ValidateStorageAuthReference(%q) error = %v, wantErr %v", tc.value, err, tc.wantErr)
			}
			if err != nil && strings.Contains(err.Error(), "secret") {
				t.Fatalf("error leaked credential-shaped input: %v", err)
			}
		})
	}
}
