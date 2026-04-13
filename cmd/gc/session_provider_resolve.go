package main

import (
	"os"

	"github.com/gastownhall/gascity/internal/config"
	sessionprovider "github.com/gastownhall/gascity/internal/sessionprovider"
)

const remoteWorkerProfile = sessionprovider.RemoteWorkerProfile

func canonicalSessionProviderName(name string) string {
	return sessionprovider.CanonicalName(name)
}

func defaultSessionProviderForConfig(cfg *config.City) string {
	if provider := os.Getenv("GC_SESSION"); provider != "" {
		return canonicalSessionProviderName(provider)
	}
	if cfg == nil {
		return canonicalSessionProviderName("")
	}
	return canonicalSessionProviderName(cfg.Session.Provider)
}

func desiredSessionProviderForAgent(a *config.Agent, cityPath, defaultProvider string) (string, string, error) {
	return sessionprovider.DesiredForAgent(a, cityPath, defaultProvider)
}

func sessionProviderMetadataForAgent(a *config.Agent, cityPath, defaultProvider string) (map[string]string, error) {
	return sessionprovider.MetadataForAgent(a, cityPath, defaultProvider)
}

func sessionProviderMetadata(provider, profile string) map[string]string {
	return sessionprovider.Metadata(provider, profile)
}

func mergeExtraSessionMetadata(dst map[string]string, src map[string]string) map[string]string {
	return sessionprovider.MergeMetadata(dst, src)
}

func resolveAgentExecSessionScript(a config.Agent, cityPath string) (string, error) {
	return sessionprovider.ResolveAgentExecScript(a, cityPath)
}
