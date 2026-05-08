package worker

import "github.com/gastownhall/gascity/internal/runtime"

func profileFamily(profile Profile) string {
	switch profile {
	case ProfileCodexTmuxCLI:
		return "codex"
	case ProfileGeminiTmuxCLI:
		return "gemini"
	case ProfileOpenCodeTmuxCLI:
		return "opencode"
	case ProfileClaudeTmuxCLI:
		return "claude"
	default:
		return ""
	}
}

func cloneStringMap(in map[string]string) map[string]string {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]string, len(in))
	for key, value := range in {
		out[key] = value
	}
	return out
}

func mergeStringMaps(base, extra map[string]string) map[string]string {
	switch {
	case len(base) == 0 && len(extra) == 0:
		return nil
	case len(base) == 0:
		return cloneStringMap(extra)
	case len(extra) == 0:
		return cloneStringMap(base)
	}
	out := cloneStringMap(base)
	for key, value := range extra {
		out[key] = value
	}
	return out
}

func cloneRuntimeConfig(cfg runtime.Config) runtime.Config {
	cfg.Env = cloneStringMap(cfg.Env)
	cfg.MCPServers = runtime.NormalizeMCPServerConfigs(cfg.MCPServers)
	cfg.ProcessNames = append([]string(nil), cfg.ProcessNames...)
	cfg.PreStart = append([]string(nil), cfg.PreStart...)
	cfg.SessionSetup = append([]string(nil), cfg.SessionSetup...)
	cfg.SessionLive = append([]string(nil), cfg.SessionLive...)
	cfg.PackOverlayDirs = append([]string(nil), cfg.PackOverlayDirs...)
	cfg.CopyFiles = append([]runtime.CopyEntry(nil), cfg.CopyFiles...)
	cfg.FingerprintExtra = cloneStringMap(cfg.FingerprintExtra)
	return cfg
}

func cloneSessionSpec(spec SessionSpec) SessionSpec {
	spec.Env = cloneStringMap(spec.Env)
	spec.Metadata = cloneStringMap(spec.Metadata)
	spec.Hints = cloneRuntimeConfig(spec.Hints)
	return spec
}
