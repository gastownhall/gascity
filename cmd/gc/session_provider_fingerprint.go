package main

import (
	"strings"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/runtime"
	sessionpkg "github.com/gastownhall/gascity/internal/session"
)

func withSessionProviderFingerprint(cfg runtime.Config, bead beads.Bead, tp TemplateParams) runtime.Config {
	if strings.TrimSpace(bead.Metadata[sessionpkg.MetadataSessionProvider]) == "" &&
		!strings.HasPrefix(strings.TrimSpace(tp.SessionProvider), "exec:") {
		return cfg
	}
	extra := make(map[string]string, len(cfg.FingerprintExtra)+2)
	for k, v := range cfg.FingerprintExtra {
		extra[k] = v
	}
	extra[sessionpkg.MetadataSessionProvider] = tp.SessionProvider
	if tp.SessionProviderProfile != "" {
		extra[sessionpkg.MetadataSessionProviderProfile] = tp.SessionProviderProfile
	}
	cfg.FingerprintExtra = extra
	return cfg
}
