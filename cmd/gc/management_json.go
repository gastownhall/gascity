package main

import (
	"io"
	"path/filepath"
	"strings"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/fsys"
)

type managementActionResult struct {
	SchemaVersion string           `json:"schema_version"`
	OK            bool             `json:"ok"`
	Command       string           `json:"command"`
	Action        string           `json:"action"`
	Name          string           `json:"name,omitempty"`
	QualifiedName string           `json:"qualified_name,omitempty"`
	Rig           string           `json:"rig,omitempty"`
	Path          string           `json:"path,omitempty"`
	Prefix        string           `json:"prefix,omitempty"`
	DefaultBranch string           `json:"default_branch,omitempty"`
	Suspended     *bool            `json:"suspended,omitempty"`
	State         string           `json:"state,omitempty"`
	DryRun        bool             `json:"dry_run,omitempty"`
	Endpoint      *rigEndpointJSON `json:"endpoint,omitempty"`
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

func rigAddJSONSummary(cityPath, rigPath, nameOverride, prefixOverride string) managementActionResult {
	name := strings.TrimSpace(nameOverride)
	if name == "" {
		name = filepath.Base(rigPath)
	}
	result := managementActionResult{
		Command: commandName("rig", "add"),
		Action:  "add",
		Name:    name,
		Rig:     name,
		Path:    rigPath,
		Prefix:  strings.ToLower(strings.TrimSpace(prefixOverride)),
	}
	if cfg, err := loadCityConfigForEditFS(fsys.OSFS{}, filepath.Join(cityPath, "city.toml")); err == nil {
		for _, rig := range cfg.Rigs {
			if rig.Name != name {
				continue
			}
			result.Prefix = rig.EffectivePrefix()
			result.DefaultBranch = rig.EffectiveDefaultBranch()
			result.Suspended = managementBoolPtr(rig.Suspended)
			break
		}
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
