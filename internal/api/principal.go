package api

import "context"

// verifiedPrincipalKey carries the key id of the write grant the write-auth
// gate verified for this request.
type verifiedPrincipalKey struct{}

// principalSlotKey carries a mutable slot that withLogging installs so the
// outermost request log line can name the principal the inner write-auth gate
// verified. It mirrors the dataSourceKey slot.
type principalSlotKey struct{}

// VerifiedPrincipal returns the key id (kid) of the write grant verified for
// this request, if any. Only city-scoped mutations on a write-auth-hardened
// city carry one; reads, supervisor-scope routes, /svc and cities without a
// verifier yield ("", false). The kid is authenticated by the grant signature,
// so callers may record it as server fact.
func VerifiedPrincipal(ctx context.Context) (string, bool) {
	kid, ok := ctx.Value(verifiedPrincipalKey{}).(string)
	if !ok || kid == "" {
		return "", false
	}
	return kid, true
}

// withVerifiedPrincipal returns ctx carrying kid as the verified principal and
// fills the request-log principal slot when withLogging installed one. An
// empty kid leaves ctx unchanged.
func withVerifiedPrincipal(ctx context.Context, kid string) context.Context {
	if kid == "" {
		return ctx
	}
	if slot, ok := ctx.Value(principalSlotKey{}).(*string); ok {
		*slot = kid
	}
	return context.WithValue(ctx, verifiedPrincipalKey{}, kid)
}

// withPrincipalSlot returns ctx carrying an empty principal slot, and the slot,
// so an outer middleware can read the principal an inner one verified.
func withPrincipalSlot(ctx context.Context) (context.Context, *string) {
	var principal string
	return context.WithValue(ctx, principalSlotKey{}, &principal), &principal
}
