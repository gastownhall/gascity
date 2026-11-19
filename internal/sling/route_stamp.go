package sling

import (
	"time"

	"github.com/gastownhall/gascity/internal/agentutil"
	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
)

// SetBuiltInRoute writes req's built-in route on store: gc.routed_to is
// req.Target normalized against cfg (verbatim when cfg is nil). When that
// route differs from req.PriorRoutedTo, normalized alike, the same write
// stamps gc.routed_at with now, so a route change and its time land together;
// re-routing a bead to the route it already has leaves its stamp alone.
func SetBuiltInRoute(store beads.Writer, cfg *config.City, req RouteRequest, now time.Time) error {
	routedTo := normalizedRoute(cfg, req.Target)
	if routedTo == normalizedRoute(cfg, req.PriorRoutedTo) {
		return store.SetMetadata(req.BeadID, beadmeta.RoutedToMetadataKey, routedTo)
	}
	return store.SetMetadataBatch(req.BeadID, map[string]string{
		beadmeta.RoutedToMetadataKey: routedTo,
		beadmeta.RoutedAtMetadataKey: beadmeta.FormatRoutedAt(now),
	})
}

// normalizedRoute is target as a built-in router records it in gc.routed_to.
func normalizedRoute(cfg *config.City, target string) string {
	if cfg == nil || target == "" {
		return target
	}
	return agentutil.NormalizePoolRouteTarget(cfg, target)
}
