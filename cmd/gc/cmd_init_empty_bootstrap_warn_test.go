package main

import (
	"bytes"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/fsys"
)

// warnMarker is the distinctive phrase the empty-template bootstrap-profile
// warning must carry, kept in one place so the assertions below pin the actual
// operator-visible signal rather than an incidental substring.
const warnMarker = "not reachable outside this box"

// TestDoInitEmptyTemplateWithoutBootstrapProfileWarns pins the guard for the
// front-door entrypoint contract: the "empty" template ships NO [api] block by
// design and composes deterministic API config from --bootstrap-profile (e.g.
// k8s-cell: 0.0.0.0:9443, mutations allowed). Without a profile the API binds
// localhost — fine for a local controller, but a hosted controller entrypoint
// that forgets --bootstrap-profile leaves its front door reachable only inside
// the pod, and that regression should surface in logs rather than pass
// silently.
func TestDoInitEmptyTemplateWithoutBootstrapProfileWarns(t *testing.T) {
	f := fsys.NewFake()
	var stdout, stderr bytes.Buffer
	code := doInit(f, "/dark-front-door", wizardConfig{configName: "empty"}, "", &stdout, &stderr, false)
	if code != 0 {
		t.Fatalf("doInit = %d, want 0; stderr: %s", code, stderr.String())
	}
	if !strings.Contains(stderr.String(), warnMarker) {
		t.Fatalf("empty template without --bootstrap-profile must warn (%q); stderr: %q", warnMarker, stderr.String())
	}
	if !strings.Contains(stderr.String(), "--bootstrap-profile") {
		t.Fatalf("warning should name --bootstrap-profile as the fix; stderr: %q", stderr.String())
	}
}

// TestDoInitEmptyTemplateWithBootstrapProfileSuppressesWarn pins that supplying
// a bootstrap profile (the correct hosted invocation) silences the warning.
func TestDoInitEmptyTemplateWithBootstrapProfileSuppressesWarn(t *testing.T) {
	f := fsys.NewFake()
	var stdout, stderr bytes.Buffer
	code := doInit(f, "/lit-front-door", wizardConfig{configName: "empty", bootstrapProfile: bootstrapProfileK8sCell}, "", &stdout, &stderr, false)
	if code != 0 {
		t.Fatalf("doInit = %d, want 0; stderr: %s", code, stderr.String())
	}
	if strings.Contains(stderr.String(), warnMarker) {
		t.Fatalf("empty template WITH --bootstrap-profile must not warn; stderr: %q", stderr.String())
	}
}

// TestDoInitNonEmptyTemplateDoesNotWarnBootstrap pins that the warning is
// specific to the empty template: templates that ship their own [api] block are
// not affected by a missing bootstrap profile.
func TestDoInitNonEmptyTemplateDoesNotWarnBootstrap(t *testing.T) {
	f := fsys.NewFake()
	var stdout, stderr bytes.Buffer
	code := doInit(f, "/minimal-city", wizardConfig{configName: "minimal"}, "", &stdout, &stderr, false)
	if code != 0 {
		t.Fatalf("doInit = %d, want 0; stderr: %s", code, stderr.String())
	}
	if strings.Contains(stderr.String(), warnMarker) {
		t.Fatalf("non-empty template must not warn about --bootstrap-profile; stderr: %q", stderr.String())
	}
}
