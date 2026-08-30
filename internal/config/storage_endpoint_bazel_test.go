package config

import (
	"strings"
	"testing"
)

func TestValidateStorageEndpointURLBazel(t *testing.T) {
	tests := []struct {
		name, value string
		wantErr     string
	}{
		{"https", "https://beads.example/workspaces", ""},
		{"http", "http://localhost:8080", ""},
		{"scheme", "ftp://beads.example", "url scheme must be http or https"},
		{"host", "https:///missing-host", "url has no host"},
		{"userinfo", "https://user:secret@beads.example", "url must not embed credentials"},
		{"query", "https://beads.example/?token=secret", "url must not carry a query"},
		{"fragment", "https://beads.example/#secret", "url must not carry a fragment"},
		{"control", "https://beads.example/\x01", "url is not a valid URL"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateStorageEndpointURL(tc.value)
			if (err != nil) != (tc.wantErr != "") {
				t.Fatalf("ValidateStorageEndpointURL(%q) error = %v, wantErr %q", tc.value, err, tc.wantErr)
			}
			if err != nil {
				if !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("ValidateStorageEndpointURL(%q) error = %v, want substring %q", tc.value, err, tc.wantErr)
				}
				if strings.Contains(err.Error(), tc.value) || strings.Contains(err.Error(), "secret") {
					t.Fatalf("error leaked credential-shaped input: %v", err)
				}
			}
		})
	}
}

func TestValidateStorageAuthReferenceBazel(t *testing.T) {
	long := strings.Repeat("x", storageAuthMaxLength+1)
	tests := []struct {
		name, value string
		wantErr     string
	}{
		{"provider", StorageAuthCredentialProvider, ""},
		{"env", "env:BEADS_TOKEN", ""},
		{"env underscore", "env:_private2", ""},
		{"env malformed", "env:1INVALID", "auth \"env:\" must be followed"},
		{"length", long, "auth is longer than 64 bytes"},
		{"whitespace", "env:BEADS TOKEN", "auth is a credential reference, not credential material"},
		{"url material", "https://user:secret@example", "auth is a credential reference, not credential material"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateStorageAuthReference(tc.value)
			if (err != nil) != (tc.wantErr != "") {
				t.Fatalf("ValidateStorageAuthReference(%q) error = %v, wantErr %q", tc.value, err, tc.wantErr)
			}
			if err != nil {
				if !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("ValidateStorageAuthReference(%q) error = %v, want substring %q", tc.value, err, tc.wantErr)
				}
				if strings.Contains(err.Error(), tc.value) || strings.Contains(err.Error(), "secret") {
					t.Fatalf("error leaked credential-shaped input: %v", err)
				}
			}
		})
	}
}
