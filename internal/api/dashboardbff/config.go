package dashboardbff

import (
	"net/http"
	"strings"
)

// runtimeConfig is the wire shape the SPA fetches at boot via
// GET /api/city/{cityName}/config. It must match shared/src/snapshot/types.ts
// DashboardRuntimeConfig exactly (the frontend's decodeRuntimeConfig validates
// every field). enabledModules is always an explicit array (never null) and
// defaultView is null when unset. The optional maintainer field is omitted —
// the maintainer module was dropped from the SDK.
type runtimeConfig struct {
	CityName          string   `json:"cityName"`
	CityRoot          string   `json:"cityRoot"`
	UseFixtures       bool     `json:"useFixtures"`
	ReadOnly          bool     `json:"readOnly"`
	OperatorAlias     string   `json:"operatorAlias"`
	OperatorWireAlias string   `json:"operatorWireAlias"`
	DecisionLabel     string   `json:"decisionLabel"`
	EnabledModules    []string `json:"enabledModules"`
	DefaultView       *string  `json:"defaultView"`
}

func (p *Plane) registerConfig() {
	p.mux.HandleFunc("GET /api/city/{cityName}/config", func(w http.ResponseWriter, r *http.Request) {
		name := r.PathValue("cityName")
		root, ok := p.resolveCityPath(name)
		if !ok {
			writeError(w, http.StatusNotFound, "unknown city")
			return
		}
		writeJSON(w, http.StatusOK, p.runtimeConfigFor(name, root))
	})
}

// runtimeConfigFor projects the operator/runtime config for a city. Operator
// identity falls back to neutral, non-identifying defaults so an unconfigured
// install never bakes in a specific human (ZERO hardcoded roles); the real
// values come from gc config/env via Deps.
func (p *Plane) runtimeConfigFor(name, root string) runtimeConfig {
	alias := firstNonEmpty(p.deps.OperatorAlias, "operator")
	wire := firstNonEmpty(p.deps.OperatorWireAlias, "human")
	label := firstNonEmpty(p.deps.DecisionLabel, "needs/"+alias)

	mods := p.deps.EnabledModules
	if mods == nil {
		mods = []string{}
	}

	var defaultView *string
	if v := strings.TrimSpace(p.deps.DefaultView); v != "" {
		defaultView = &v
	}

	return runtimeConfig{
		CityName:          name,
		CityRoot:          root,
		UseFixtures:       false,
		ReadOnly:          p.deps.ReadOnly,
		OperatorAlias:     alias,
		OperatorWireAlias: wire,
		DecisionLabel:     label,
		EnabledModules:    mods,
		DefaultView:       defaultView,
	}
}
