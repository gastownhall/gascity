package main

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/doctor"
	"github.com/gastownhall/gascity/internal/runtime"
)

// runtimeOwnerSource is the owner source behind gc doctor's orphan-sessions
// check: each call takes a fresh census of every store a session row of the
// city can live in (the stores the ownerless-runtimes check reads) and
// resolves runtimes against it. A provider or store that cannot be read fails
// the source, so no runtime is called an orphan on a read that never happened.
func (c *ownerlessRuntimesCheck) runtimeOwnerSource() doctor.RuntimeOwnerSource {
	return func() (doctor.RuntimeOwnerResolver, error) {
		if c.sp == nil {
			return nil, errors.New("no session provider to read runtime identity from")
		}
		city, err := c.newStore(c.cityPath)
		if err != nil {
			return nil, fmt.Errorf("opening bead store: %w", err)
		}
		rigs := map[string]beads.Store{}
		if c.cfg != nil {
			for _, rig := range c.cfg.Rigs {
				if strings.TrimSpace(rig.Path) == "" {
					continue
				}
				store, err := c.newStore(rig.Path)
				if err != nil {
					return nil, fmt.Errorf("opening rig %s's store: %w", rig.Name, err)
				}
				rigs[rig.Name] = store
			}
		}
		legs, err := sessionCensusStoreCandidates(c.cityPath, c.cfg, city, rigs, nil)
		if err != nil {
			return nil, err
		}
		census, err := readSessionCensus(time.Now(), legs)
		if err != nil {
			return nil, err
		}
		return runtimeOwnerResolver(census, c.sp), nil
	}
}

// runtimeOwnerResolver resolves a runtime by its own identity against the
// census's open rows, never by its name: it is owned when its GC_SESSION_ID
// names an open row, or, for an adopted runtime whose env id predates the row
// minted for it, when an open row records the runtime's session name with the
// runtime's own GC_INSTANCE_TOKEN (the adoption barrier's identity). It is
// orphaned only when both identity reads succeeded and a complete census holds
// no such row; an unreadable identity or an incomplete census leaves it
// unproven, which authorizes nothing.
func runtimeOwnerResolver(census *sessionCensus, sp runtime.Provider) doctor.RuntimeOwnerResolver {
	openIDs := map[string]bool{}
	tokensByName := map[string][]string{}
	for k, row := range census.Rows {
		if row.Info.Closed {
			continue
		}
		openIDs[k.ID] = true
		if name, token := strings.TrimSpace(row.Info.SessionNameMetadata), strings.TrimSpace(row.InstanceToken); name != "" && token != "" {
			tokensByName[name] = append(tokensByName[name], token)
		}
	}
	_, complete := census.complete()
	return func(name string) (doctor.RuntimeOwnership, string) {
		id, err := runtime.MetaValue(sp.GetMeta(name, "GC_SESSION_ID"))
		if err != nil {
			return doctor.RuntimeUnproven, fmt.Sprintf("GC_SESSION_ID unreadable: %v", err)
		}
		if id = strings.TrimSpace(id); id != "" && openIDs[id] {
			return doctor.RuntimeOwned, ""
		}
		token, err := runtime.MetaValue(sp.GetMeta(name, runtime.InstanceTokenEnv))
		if err != nil {
			return doctor.RuntimeUnproven, fmt.Sprintf("%s unreadable: %v", runtime.InstanceTokenEnv, err)
		}
		if token = strings.TrimSpace(token); token != "" {
			for _, rowToken := range tokensByName[name] {
				if rowToken == token {
					return doctor.RuntimeOwned, ""
				}
			}
		}
		if !complete {
			return doctor.RuntimeUnproven, "the session census is incomplete"
		}
		return doctor.RuntimeOrphaned, ""
	}
}
