package main

import (
	"strings"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/orders"
)

const coreJSONLExportOrder = "jsonl-export"

func filterOrdersForBackend(cityPath string, cfg *config.City, aa []orders.Order) []orders.Order {
	if len(aa) == 0 || backendSupportsCoreJSONLExport(cityPath, cfg) {
		return aa
	}
	filtered := make([]orders.Order, 0, len(aa))
	for _, a := range aa {
		if a.Name == coreJSONLExportOrder {
			continue
		}
		filtered = append(filtered, a)
	}
	return filtered
}

func backendSupportsCoreJSONLExport(cityPath string, cfg *config.City) bool {
	provider := orderBackendProvider(cityPath, cfg)
	backend := orderBackendName(cityPath, cfg)
	if provider == "bd" && backend == "dolt" {
		return true
	}
	if provider != "plugin" {
		return false
	}
	caps, ok := beadsBackendPluginCapabilitiesForCity(cityPath)
	return ok && caps.JSONLExport
}

func orderBackendProvider(cityPath string, cfg *config.City) string {
	if cfg != nil && strings.TrimSpace(cfg.Beads.Provider) != "" {
		return normalizeRawBeadsProvider(cityPath, cfg.Beads.Provider)
	}
	return rawBeadsProvider(cityPath)
}

func orderBackendName(cityPath string, cfg *config.City) string {
	if cfg != nil && strings.TrimSpace(cfg.Beads.Backend) != "" {
		return strings.ToLower(strings.TrimSpace(cfg.Beads.Backend))
	}
	return beadsBackend(cityPath)
}
