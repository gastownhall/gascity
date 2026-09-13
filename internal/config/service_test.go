package config

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/fsys"
)

const builtinHealthzContract = "gc.healthz.v1"

func TestParseServiceConfig(t *testing.T) {
	cfg, err := Parse([]byte(`
[workspace]
name = "test-city"

[api]
bind = "127.0.0.1"
port = 9443

[[service]]
name = "review-intake"
publish_mode = "direct"

[service.workflow]
contract = "` + builtinHealthzContract + `"
`))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if len(cfg.Services) != 1 {
		t.Fatalf("len(Services) = %d, want 1", len(cfg.Services))
	}
	svc := cfg.Services[0]
	if svc.Name != "review-intake" {
		t.Fatalf("service name = %q, want review-intake", svc.Name)
	}
	if svc.Workflow.Contract != builtinHealthzContract {
		t.Errorf("workflow.contract = %q, want %q", svc.Workflow.Contract, builtinHealthzContract)
	}
	if svc.PublishModeOrDefault() != "direct" {
		t.Errorf("PublishModeOrDefault() = %q, want direct", svc.PublishModeOrDefault())
	}
	if svc.MountPathOrDefault() != "/svc/review-intake" {
		t.Errorf("MountPathOrDefault() = %q, want /svc/review-intake", svc.MountPathOrDefault())
	}
	if err := ValidateServices(cfg.Services); err != nil {
		t.Fatalf("ValidateServices: %v", err)
	}
}

func TestParseServicePublicationConfig(t *testing.T) {
	cfg, err := Parse([]byte(`
[workspace]
name = "test-city"

[[service]]
name = "review-intake"

[service.publication]
visibility = "public"
hostname = "review"
allow_websockets = true

[service.workflow]
contract = "` + builtinHealthzContract + `"
`))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if len(cfg.Services) != 1 {
		t.Fatalf("len(Services) = %d, want 1", len(cfg.Services))
	}
	svc := cfg.Services[0]
	if got := svc.PublicationVisibilityOrDefault(); got != "public" {
		t.Fatalf("PublicationVisibilityOrDefault() = %q, want public", got)
	}
	if got := svc.PublicationHostnameOrDefault(); got != "review" {
		t.Fatalf("PublicationHostnameOrDefault() = %q, want review", got)
	}
	if !svc.Publication.AllowWebSockets {
		t.Fatal("Publication.AllowWebSockets = false, want true")
	}
	if err := ValidateServices(cfg.Services); err != nil {
		t.Fatalf("ValidateServices: %v", err)
	}
}

func TestValidateServicesWorkflowRequiresContract(t *testing.T) {
	err := ValidateServices([]Service{{Name: "review-intake"}})
	if err == nil {
		t.Fatal("expected error for missing workflow.contract")
	}
	if !strings.Contains(err.Error(), "workflow.contract is required") {
		t.Fatalf("error = %v, want missing workflow.contract", err)
	}
}

func TestValidateServicesRejectsUnsupportedKind(t *testing.T) {
	err := ValidateServices([]Service{{
		Name: "review-intake",
		Kind: "mystery",
	}})
	if err == nil {
		t.Fatal("expected error for unsupported service kind")
	}
	if !strings.Contains(err.Error(), `kind must be "workflow" or "proxy_process"`) {
		t.Fatalf("error = %v, want unsupported workflow-only error", err)
	}
}

func TestValidateServicesProxyProcessRequiresCommand(t *testing.T) {
	err := ValidateServices([]Service{{
		Name: "bridge",
		Kind: "proxy_process",
	}})
	if err == nil {
		t.Fatal("expected error for missing process.command")
	}
	if !strings.Contains(err.Error(), "process.command is required") {
		t.Fatalf("error = %v, want missing process.command", err)
	}
}

func TestValidateServicesProxyProcessAcceptsCommand(t *testing.T) {
	err := ValidateServices([]Service{{
		Name: "bridge",
		Kind: "proxy_process",
		Process: ServiceProcessConfig{
			Command: []string{"./scripts/start-bridge.sh"},
		},
	}})
	if err != nil {
		t.Fatalf("ValidateServices: %v", err)
	}
}

func TestValidateServicesRejectsInvalidPublicationVisibility(t *testing.T) {
	err := ValidateServices([]Service{{
		Name: "review-intake",
		Publication: ServicePublicationConfig{
			Visibility: "internet",
		},
		Workflow: ServiceWorkflowConfig{Contract: builtinHealthzContract},
	}})
	if err == nil {
		t.Fatal("expected error for invalid publication.visibility")
	}
	if !strings.Contains(err.Error(), `publication.visibility must be "private", "public", or "tenant"`) {
		t.Fatalf("error = %v, want invalid publication visibility", err)
	}
}

func TestValidateServicesRejectsInvalidPublicationHostname(t *testing.T) {
	err := ValidateServices([]Service{{
		Name: "review-intake",
		Publication: ServicePublicationConfig{
			Visibility: "public",
			Hostname:   "bad.host",
		},
		Workflow: ServiceWorkflowConfig{Contract: builtinHealthzContract},
	}})
	if err == nil {
		t.Fatal("expected error for invalid publication.hostname")
	}
	if !strings.Contains(err.Error(), "publication.hostname must be a single DNS label") {
		t.Fatalf("error = %v, want invalid publication hostname", err)
	}
}

func TestParseProxyProcessServiceConfig(t *testing.T) {
	cfg, err := Parse([]byte(`
[workspace]
name = "test-city"

[[service]]
name = "bridge"
kind = "proxy_process"

[service.process]
command = ["./scripts/start-bridge.sh"]
health_path = "/healthz"
`))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if len(cfg.Services) != 1 {
		t.Fatalf("len(Services) = %d, want 1", len(cfg.Services))
	}
	svc := cfg.Services[0]
	if svc.KindOrDefault() != "proxy_process" {
		t.Fatalf("KindOrDefault = %q, want proxy_process", svc.KindOrDefault())
	}
	if len(svc.Process.Command) != 1 || svc.Process.Command[0] != "./scripts/start-bridge.sh" {
		t.Fatalf("process.command = %#v, want start-bridge.sh", svc.Process.Command)
	}
	if svc.Process.HealthPath != "/healthz" {
		t.Fatalf("process.health_path = %q, want /healthz", svc.Process.HealthPath)
	}
}

func TestExpandCityPacks_ServiceFromPack(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "packs/review/pack.toml", `
[pack]
name = "review"
schema = 1

[[service]]
name = "review-intake"
state_root = ".gc/services/github-intake"

[service.workflow]
contract = "`+builtinHealthzContract+`"
`)

	cfg := &City{
		Workspace: Workspace{Includes: []string{"packs/review"}},
	}
	if _, _, _, err := ExpandCityPacks(cfg, fsys.OSFS{}, dir); err != nil {
		t.Fatalf("ExpandCityPacks: %v", err)
	}
	if len(cfg.Services) != 1 {
		t.Fatalf("len(Services) = %d, want 1", len(cfg.Services))
	}
	if cfg.Services[0].SourceDir != filepath.Join(dir, "packs/review") {
		t.Errorf("service SourceDir = %q, want %q", cfg.Services[0].SourceDir, filepath.Join(dir, "packs/review"))
	}
	if cfg.Services[0].Workflow.Contract != builtinHealthzContract {
		t.Errorf("workflow.contract = %q, want %q", cfg.Services[0].Workflow.Contract, builtinHealthzContract)
	}
	if got := cfg.Services[0].StateRootOrDefault(); got != ".gc/services/github-intake" {
		t.Errorf("StateRootOrDefault() = %q, want %q", got, ".gc/services/github-intake")
	}
}

func TestExpandPacks_RejectsRigPackServices(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "packs/review/pack.toml", `
[pack]
name = "review"
schema = 1

[[service]]
name = "review-intake"

[service.workflow]
contract = "`+builtinHealthzContract+`"
`)

	cfg := &City{
		Rigs: []Rig{{
			Name:     "product",
			Path:     "/tmp/product",
			Includes: []string{"packs/review"},
		}},
	}
	err := ExpandPacks(cfg, fsys.OSFS{}, dir, nil)
	if err == nil {
		t.Fatal("expected rig pack service rejection")
	}
	if !strings.Contains(err.Error(), "[[service]] is only allowed in city-scoped packs") {
		t.Fatalf("error = %v, want rig pack service rejection", err)
	}
}

func TestValidateServicesAllowsSharedStateRootWithinSamePack(t *testing.T) {
	err := ValidateServices([]Service{
		{
			Name:      "github-webhook",
			Kind:      "proxy_process",
			StateRoot: ".gc/services/github-intake",
			SourceDir: "/packs/github-intake",
			Process: ServiceProcessConfig{
				Command: []string{"python3", "scripts/service.py"},
			},
		},
		{
			Name:      "github-admin",
			Kind:      "proxy_process",
			StateRoot: ".gc/services/github-intake",
			SourceDir: "/packs/github-intake",
			Process: ServiceProcessConfig{
				Command: []string{"python3", "scripts/service.py"},
			},
		},
	})
	if err != nil {
		t.Fatalf("ValidateServices: %v", err)
	}
}

func TestValidateServicesRejectsSharedStateRootAcrossSources(t *testing.T) {
	err := ValidateServices([]Service{
		{
			Name:      "github-webhook",
			Kind:      "proxy_process",
			StateRoot: ".gc/services/github-intake",
			SourceDir: "/packs/github-intake",
			Process: ServiceProcessConfig{
				Command: []string{"python3", "scripts/service.py"},
			},
		},
		{
			Name:      "review-intake",
			Kind:      "proxy_process",
			StateRoot: ".gc/services/github-intake",
			Process: ServiceProcessConfig{
				Command: []string{"./scripts/start-bridge.sh"},
			},
		},
	})
	if err == nil {
		t.Fatal("expected state_root collision rejection")
	}
	if !strings.Contains(err.Error(), "shared state_root is only allowed within the same pack source") {
		t.Fatalf("error = %v, want state_root collision rejection", err)
	}
}
