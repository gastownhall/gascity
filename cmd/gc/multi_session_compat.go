package main

import (
	"strings"

	"github.com/gastownhall/gascity/internal/config"
)

func isMultiSessionCfgAgent(a *config.Agent) bool {
	if a == nil {
		return false
	}
	if strings.TrimSpace(a.Namepool) != "" || len(a.NamepoolNames) > 0 {
		return true
	}
	maxSess := a.EffectiveMaxActiveSessions()
	return maxSess == nil || *maxSess != 1
}
