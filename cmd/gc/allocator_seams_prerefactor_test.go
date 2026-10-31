package main

// Frozen copy of the demand merge that P3-5a extracted into
// mergeCollectedDemand, taken from buildDesiredStateWithSessionBeadsAt on
// main at f31074c239 with its trace records and log lines removed. It is the
// "before" side of TestMergeCollectedDemandMatchesPreRefactor and goes away
// with the legacy reconciler: delete it, don't update it.

import (
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
)

func mergeDemandPreRefactor(
	cfg *config.City,
	evalCounts map[string]int,
	evalPartials map[string]bool,
	defaultProbed bool,
	defaultCounts map[string]int,
	defaultDemand map[string]scaleCheckDemand,
	defaultPartials map[string]bool,
	coldWakeTemplates, namedOnDemandTemplates map[string]bool,
	unassignedRoutedBeads []beads.Bead,
	unassignedRoutedStoreRefs []string,
	unassignedRoutedPartial bool,
	namedProbed bool,
	namedPartials map[string]bool,
) mergedDemand {
	var scaleCheckCounts map[string]int
	var scaleCheckDemandByTemplate map[string]scaleCheckDemand
	var poolScaleCheckPartialTemplates map[string]bool
	var poolPartialRetentionTemplates map[string]bool
	var namedScaleCheckPartialTemplates map[string]bool
	var scaleCheckPartialTemplates map[string]bool
	controlDispatcherOpenDemand := openControlDispatcherDemand(cfg, unassignedRoutedBeads)
	scaleCheckCounts, poolScaleCheckPartialTemplates = evalCounts, evalPartials
	if defaultProbed {
		poolScaleCheckPartialTemplates = mergeScaleCheckPartialTemplates(poolScaleCheckPartialTemplates, defaultPartials)
		if scaleCheckCounts == nil {
			scaleCheckCounts = make(map[string]int)
		}
		if scaleCheckDemandByTemplate == nil {
			scaleCheckDemandByTemplate = make(map[string]scaleCheckDemand)
		}
		for template, count := range defaultCounts {
			if coldWakeTemplates[template] && count > 1 {
				count = 1
			}
			if namedOnDemandTemplates[template] && count > 1 {
				count = 1
			}
			if count > scaleCheckCounts[template] {
				scaleCheckCounts[template] = count
			}
			scaleCheckDemandByTemplate[template] = mergeScaleCheckDemand(scaleCheckDemandByTemplate[template], defaultDemand[template], count)
		}
	}
	poolPartialRetentionTemplates = mergeScaleCheckPartialTemplates(poolPartialRetentionTemplates, poolScaleCheckPartialTemplates)
	if len(controlDispatcherOpenDemand) > 0 {
		if scaleCheckCounts == nil {
			scaleCheckCounts = make(map[string]int)
		}
		for template, hasDemand := range controlDispatcherOpenDemand {
			if hasDemand && scaleCheckCounts[template] < 1 {
				scaleCheckCounts[template] = 1
			}
		}
	}
	if unassignedRoutedPartial {
		poolPartialRetentionTemplates = markControlDispatcherTemplatesPartial(cfg, poolPartialRetentionTemplates)
	}
	readyUnassignedRoutedWorkBeads, readyUnassignedRoutedWorkStoreRefs := selectReadyUnassignedRoutedWork(
		unassignedRoutedBeads,
		unassignedRoutedStoreRefs,
		scaleCheckDemandByTemplate,
	)
	if namedProbed {
		namedScaleCheckPartialTemplates = mergeScaleCheckPartialTemplates(namedScaleCheckPartialTemplates, namedPartials)
	}
	scaleCheckPartialTemplates = mergeScaleCheckPartialTemplates(scaleCheckPartialTemplates, poolPartialRetentionTemplates)
	scaleCheckPartialTemplates = mergeScaleCheckPartialTemplates(scaleCheckPartialTemplates, namedScaleCheckPartialTemplates)
	return mergedDemand{
		ScaleCheckCounts:          scaleCheckCounts,
		ScaleCheckDemand:          scaleCheckDemandByTemplate,
		PoolScaleCheckPartial:     poolScaleCheckPartialTemplates,
		PoolPartialRetention:      poolPartialRetentionTemplates,
		NamedScaleCheckPartial:    namedScaleCheckPartialTemplates,
		ScaleCheckPartial:         scaleCheckPartialTemplates,
		ReadyUnassignedRouted:     readyUnassignedRoutedWorkBeads,
		ReadyUnassignedRoutedRefs: readyUnassignedRoutedWorkStoreRefs,
	}
}
