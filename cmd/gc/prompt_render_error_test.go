package main

import (
	"errors"
	"io"
	"io/fs"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/fsys"
)

// TestResolveTemplatePromptRenderFailure pins how resolveTemplate treats a
// declared prompt_template that cannot be used as written (ga-5wwp1u). The
// result becomes a spawned agent's live startup prompt, so a template that was
// read but failed to parse or execute must fail the agent build rather than
// ship its raw {{...}} text. A template that cannot be read keeps the
// beacon-only fallback pinned by TestResolveSessionTemplate_MissingDeclaredPromptIsNotSilent.
func TestResolveTemplatePromptRenderFailure(t *testing.T) {
	tests := []struct {
		name    string
		body    string // empty: the template file does not exist
		wantErr string // empty: resolveTemplate must succeed
	}{
		{name: "execute error: undefined fragment", body: `Hello {{ template "propulsion-dog" . }}`, wantErr: "not defined"},
		{name: "parse error: unclosed action", body: "Bad: {{ .Unclosed", wantErr: "prompt_template"},
		{name: "unreadable template keeps the fallback"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cityPath := t.TempDir()
			fake := fsys.NewFake()
			if tt.body != "" {
				fake.Files[cityPath+"/prompts/dog.template.md"] = []byte(tt.body)
			}
			params := &agentBuildParams{
				fs:         fake,
				cityName:   "bright-lights",
				cityPath:   cityPath,
				workspace:  &config.Workspace{Name: "bright-lights", Provider: "opencode"},
				providers:  config.BuiltinProviders(),
				lookPath:   func(string) (string, error) { return "/usr/bin/opencode", nil },
				beaconTime: testBeaconTime,
				beadNames:  make(map[string]string),
				stderr:     io.Discard,
			}
			agent := &config.Agent{Name: "dog", PromptTemplate: "prompts/dog.template.md", Provider: "opencode"}

			tp, err := resolveTemplate(params, agent, agent.QualifiedName(), nil)
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("resolveTemplate(unreadable template) error = %v, want nil (beacon-only fallback)", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("resolveTemplate(%s) = nil error, want one mentioning %q; prompt: %q", tt.name, tt.wantErr, tp.Prompt)
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("err = %v, want to mention %q", err, tt.wantErr)
			}
		})
	}
}

// TestRenderPromptWithMetaReadErrorIsTyped pins the typed signal resolveTemplate
// uses to tell "could not be read" from "failed to parse or execute": a read
// failure is a promptTemplateReadError that still unwraps to the cause, and a
// parse or execute failure is not.
func TestRenderPromptWithMetaReadErrorIsTyped(t *testing.T) {
	f := fsys.NewFake()
	f.Files["/city/prompts/bad.template.md"] = []byte("Bad: {{ .Unclosed")
	render := func(path string) error {
		return renderPromptWithMeta(f, "/city", "", path, PromptContext{}, "", io.Discard, nil, nil, nil).Err
	}

	var readErr *promptTemplateReadError
	missing := render("prompts/missing.template.md")
	if !errors.As(missing, &readErr) || !errors.Is(missing, fs.ErrNotExist) {
		t.Errorf("missing template Err = %v, want a *promptTemplateReadError wrapping fs.ErrNotExist", missing)
	}
	parse := render("prompts/bad.template.md")
	if parse == nil || errors.As(parse, &readErr) {
		t.Errorf("parse-error Err = %v, want non-nil and not a *promptTemplateReadError", parse)
	}
}
