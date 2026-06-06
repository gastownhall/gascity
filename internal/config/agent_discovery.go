package config

import (
	"fmt"
	"path/filepath"
	"strings"

	"github.com/BurntSushi/toml"
	"github.com/gastownhall/gascity/internal/fsys"
)

var agentPromptConventionFilenames = []string{
	"prompt.template.md",
	"prompt.md.tmpl",
	"prompt.md",
}

// DiscoverPackAgents scans a pack's agents/ tree and returns
// convention-discovered agents. Each immediate subdirectory is an agent.
// agent.toml provides optional per-agent config, prompt.template.md is
// canonical, prompt.md.tmpl remains temporarily supported, and prompt.md is
// the plain-markdown fallback.
func DiscoverPackAgents(fs fsys.FS, packDir, _ string, skipNames map[string]bool) ([]Agent, error) {
	agentsDir := filepath.Join(packDir, "agents")
	entries, err := fs.ReadDir(agentsDir)
	if err != nil {
		return nil, nil
	}

	var discovered []Agent
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		agentName := entry.Name()
		if strings.HasPrefix(agentName, ".") || strings.HasPrefix(agentName, "_") {
			continue
		}
		if skipNames != nil && skipNames[agentName] {
			continue
		}

		agentDir := filepath.Join(agentsDir, agentName)
		agent := Agent{Name: agentName}

		agentTomlPath := filepath.Join(agentDir, "agent.toml")
		if atData, atErr := fs.ReadFile(agentTomlPath); atErr == nil {
			if _, decErr := toml.Decode(string(atData), &agent); decErr != nil {
				return nil, fmt.Errorf("agents/%s/agent.toml: %w", agentName, decErr)
			}
			agent.Name = agentName
		}
		applyAgentConventionDefaults(fs, packDir, &agent)
		agent.source = sourcePack
		agent.layout = layoutV2Convention

		discovered = append(discovered, agent)
	}

	return discovered, nil
}

// DiscoverPackAttachmentRoots reports the shared attachment catalog roots
// for the current city pack if they exist.
func DiscoverPackAttachmentRoots(fs fsys.FS, packDir string) (skillsDir, mcpDir string) {
	skillsPath := filepath.Join(packDir, "skills")
	if info, err := fs.Stat(skillsPath); err == nil && info.IsDir() {
		skillsDir = skillsPath
	}

	mcpPath := filepath.Join(packDir, "mcp")
	if info, err := fs.Stat(mcpPath); err == nil && info.IsDir() {
		mcpDir = mcpPath
	}

	return skillsDir, mcpDir
}

func applyAgentConventionDefaults(fs fsys.FS, packDir string, agent *Agent) {
	if agent == nil || strings.TrimSpace(agent.Name) == "" {
		return
	}

	agentDir := filepath.Join(packDir, "agents", agent.Name)
	info, err := fs.Stat(agentDir)
	if err != nil || !info.IsDir() {
		return
	}

	if agent.PromptTemplate == "" {
		for _, promptName := range agentPromptConventionFilenames {
			promptPath := filepath.Join(agentDir, promptName)
			if _, pErr := fs.Stat(promptPath); pErr == nil {
				agent.PromptTemplate = promptPath
				break
			}
		}
	}

	if agent.OverlayDir == "" {
		overlayPath := filepath.Join(agentDir, "overlay")
		if info, oErr := fs.Stat(overlayPath); oErr == nil && info.IsDir() {
			agent.OverlayDir = overlayPath
		}
	}

	if agent.Namepool == "" {
		namepoolPath := filepath.Join(agentDir, "namepool.txt")
		if _, npErr := fs.Stat(namepoolPath); npErr == nil {
			agent.Namepool = namepoolPath
		}
	}

	if agent.SkillsDir == "" {
		skillsDir := filepath.Join(agentDir, "skills")
		if info, sErr := fs.Stat(skillsDir); sErr == nil && info.IsDir() {
			agent.SkillsDir = skillsDir
		}
	}

	if agent.MCPDir == "" {
		mcpDir := filepath.Join(agentDir, "mcp")
		if info, mErr := fs.Stat(mcpDir); mErr == nil && info.IsDir() {
			agent.MCPDir = mcpDir
		}
	}
}
