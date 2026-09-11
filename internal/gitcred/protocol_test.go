package gitcred

import (
	"strings"
	"testing"
)

func TestReadRequestBlankLineTerminates(t *testing.T) {
	in := "protocol=https\nhost=github.com\npath=gascity/repo.git\n\nignored=after\n"
	req, err := ReadRequest(strings.NewReader(in))
	if err != nil {
		t.Fatalf("ReadRequest: %v", err)
	}
	if req.Protocol != "https" || req.Host != "github.com" || req.Path != "gascity/repo.git" {
		t.Fatalf("unexpected request: %+v", req)
	}
}

func TestReadRequestEOFTerminates(t *testing.T) {
	req, err := ReadRequest(strings.NewReader("protocol=https\nhost=github.com"))
	if err != nil {
		t.Fatalf("ReadRequest: %v", err)
	}
	if req.Host != "github.com" {
		t.Fatalf("unexpected host %q", req.Host)
	}
}

func TestReadRequestUnknownKeysIgnored(t *testing.T) {
	req, err := ReadRequest(strings.NewReader("host=github.com\nwacky=1\ncapability[]=authtype\n"))
	if err != nil {
		t.Fatalf("ReadRequest: %v", err)
	}
	if req.Host != "github.com" {
		t.Fatalf("unexpected host %q", req.Host)
	}
}

func TestReadRequestMalformedLine(t *testing.T) {
	if _, err := ReadRequest(strings.NewReader("host=github.com\nno-equals-here\n")); err == nil {
		t.Fatalf("expected error for malformed line")
	}
}

func TestWriteCredentialExactBytes(t *testing.T) {
	var buf strings.Builder
	if err := WriteCredential(&buf, Credential{Username: "x-access-token", Password: "ghp_abc"}); err != nil {
		t.Fatalf("WriteCredential: %v", err)
	}
	want := "username=x-access-token\npassword=ghp_abc\n"
	if buf.String() != want {
		t.Fatalf("got %q want %q", buf.String(), want)
	}
}
