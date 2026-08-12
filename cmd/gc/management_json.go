package main

import (
	"io"
	"path/filepath"
	"strings"

	"github.com/gastownhall/gascity/internal/config"
)

type managementActionResult struct {
	SchemaVersion string `json:"schema_version"`
	OK            bool   `json:"ok"`
	Command       string `json:"command"`
	Action        string `json:"action"`
	Name          string `json:"name,omitempty"`
	QualifiedName string `json:"qualified_name,omitempty"`
	Rig           string `json:"rig,omitempty"`
	Path          string `json:"path,omitempty"`
	Prefix        string `json:"prefix,omitempty"`
	DefaultBranch string `json:"default_branch,omitempty"`
	Suspended     *bool  `json:"suspended,omitempty"`
	State         string `json:"state,omitempty"`
	// Status and RequestID are additive fields the remote `rig add` path emits so
	// a script repointed at a remote city keeps the automation-critical keys plus
	// the async outcome (provisioned/exists) and its idempotency id. Local
	// management actions leave them empty (omitempty), preserving byte-identical
	// local JSON.
	Status      string           `json:"status,omitempty"`
	RequestID   string           `json:"request_id,omitempty"`
	Retried     *bool            `json:"retried,omitempty"`
	RetriedFrom string           `json:"retried_from_wait,omitempty"`
	ReadyWaitID string           `json:"ready_wait_id,omitempty"`
	DryRun      *bool            `json:"dry_run,omitempty"`
	Endpoint    *rigEndpointJSON `json:"endpoint,omitempty"`
}

type rigEndpointJSON struct {
	Mode            string `json:"mode"`
	Host            string `json:"host,omitempty"`
	Port            string `json:"port,omitempty"`
	User            string `json:"user,omitempty"`
	AdoptUnverified bool   `json:"adopt_unverified,omitempty"`
}

func writeManagementActionJSON(stdout io.Writer, result managementActionResult) error {
	result.SchemaVersion = "1"
	result.OK = true
	return writeCLIJSONLine(stdout, result)
}

func managementBoolPtr(v bool) *bool {
	return &v
}

func commandName(parts ...string) string {
	return strings.Join(parts, " ")
}

func agentJSONName(input, dir string) (name, qualified string) {
	inputDir, inputName := config.ParseQualifiedName(input)
	if inputDir != "" {
		dir = inputDir
		name = inputName
	} else {
		name = input
	}
	if strings.TrimSpace(dir) != "" {
		qualified = dir + "/" + name
	} else {
		qualified = name
	}
	return name, qualified
}

func agentJSONIdentity(cityPath, input string) (name, qualified string) {
	return agentJSONName(resolveAgentForAPI(cityPath, input), "")
}

func rigAddJSONSummary(rigPath string, rig config.Rig) managementActionResult {
	name := strings.TrimSpace(rig.Name)
	if name == "" {
		name = filepath.Base(rigPath)
	}
	result := managementActionResult{
		Command:       commandName("rig", "add"),
		Action:        "add",
		Name:          name,
		Rig:           name,
		Path:          rigPath,
		Prefix:        rig.EffectivePrefix(),
		DefaultBranch: rig.EffectiveDefaultBranch(),
		Suspended:     managementBoolPtr(rig.Suspended),
	}
	if result.Prefix == "" {
		result.Prefix = config.DeriveBeadsPrefix(name)
	}
	return result
}

func rigEndpointJSONFromOptions(opts rigEndpointOptions) *rigEndpointJSON {
	endpoint := &rigEndpointJSON{AdoptUnverified: opts.AdoptUnverified}
	switch {
	case opts.Inherit:
		endpoint.Mode = "inherit"
	case opts.Self:
		endpoint.Mode = "self"
		endpoint.Host = "127.0.0.1"
		endpoint.Port = strings.TrimSpace(opts.Port)
	case opts.External:
		endpoint.Mode = "external"
		endpoint.Host = strings.TrimSpace(opts.Host)
		endpoint.Port = strings.TrimSpace(opts.Port)
		endpoint.User = strings.TrimSpace(opts.User)
	}
	return endpoint
}
