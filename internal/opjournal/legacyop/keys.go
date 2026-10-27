package legacyop

import (
	"fmt"
	"strings"

	"github.com/gastownhall/gascity/internal/opjournal"
)

// Client-key namespaces (Request.KeyNamespace). gc runtime request-restart
// and a self gc handoff record restart{fresh} on their session, the request
// a session reset records (gc session reset, the API's reset), with the same
// digest: under one client key they would derive one op id, and a rerun of
// one would replay another's op. Each in-session restart request therefore
// keeps its clients' keys in its own namespace, and the namespaces are
// reserved: a restart request that keeps its key as given may not use one
// that starts with a namespace (ErrReservedKey), so it never names a
// namespaced op.
const (
	// KeyNamespaceRequestRestart keeps gc runtime request-restart's keys.
	KeyNamespaceRequestRestart = "request-restart:"
	// KeyNamespaceHandoff keeps a self gc handoff's keys.
	KeyNamespaceHandoff = "handoff:"
)

// restartKeyNamespaces are the restart requests' namespaces, each with the
// command whose keys it keeps.
var restartKeyNamespaces = []struct{ namespace, command string }{
	{KeyNamespaceRequestRestart, "gc runtime request-restart"},
	{KeyNamespaceHandoff, "gc handoff"},
}

// ErrReservedKey reports a client key that starts with a namespace another
// command keeps its keys in. It wraps opjournal.ErrInvalidRequest: Admit runs
// such a request unrecorded in auto and refuses it in require, as it does
// any invalid key.
var ErrReservedKey = fmt.Errorf("%w: client key in a reserved namespace", opjournal.ErrInvalidRequest)

// NamespacedKey is the client key key is stored under in namespace:
// namespace+key, or key alone when either is empty.
func NamespacedKey(namespace, key string) string {
	if namespace == "" || key == "" {
		return key
	}
	return namespace + key
}

// ValidateKey refuses a client key Admit refuses for a request of kind that
// keeps its keys in namespace ("" for none): one opjournal.ValidateClientKey
// refuses, one too long once in its namespace, or a restart request's key
// kept as given that starts with a restart request's namespace
// (ErrReservedKey). Its errors name key as the client gave it.
func ValidateKey(kind opjournal.Kind, namespace, key string) error {
	if err := opjournal.ValidateClientKey(key); err != nil {
		return err
	}
	if limit := opjournal.MaxClientKeyLen - len(namespace); len(key) > limit {
		return fmt.Errorf("%w: client key is %d bytes, max %d in the %q key namespace", opjournal.ErrInvalidRequest, len(key), limit, namespace)
	}
	if kind != opjournal.KindRestart || namespace != "" {
		return nil
	}
	for _, ns := range restartKeyNamespaces {
		if strings.HasPrefix(key, ns.namespace) {
			return fmt.Errorf("%w: %q starts with %q, which keeps %s's keys", ErrReservedKey, key, ns.namespace, ns.command)
		}
	}
	return nil
}

// storedKey is the client key req's op is stored under once key, the
// client's or one the recorder minted, is admitted: a client's key in req's
// namespace once ValidateKey accepts it, a minted one as minted.
func (req Request) storedKey(key string, minted bool) (string, error) {
	if minted {
		return key, nil
	}
	if err := ValidateKey(req.Kind, req.KeyNamespace, key); err != nil {
		return "", err
	}
	return NamespacedKey(req.KeyNamespace, key), nil
}
