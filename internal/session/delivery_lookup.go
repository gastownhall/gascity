package session

import (
	"context"
	"fmt"

	"github.com/gastownhall/gascity/internal/runtime"
)

// LookupDelivery asks session id's runtime whether it holds the delivery a
// send under delivery key key made (runtime.LookupDelivery), for the row's
// runtime name. It reads the row as the session's sends do, so a closed row
// is ErrSessionClosed, and it sends nothing and takes no lock. found, with
// the runtime's receipt, when the runtime holds it; not found where the
// runtime holds none or keeps no keyed deliveries; an error when it could
// not tell.
func (m *Manager) LookupDelivery(ctx context.Context, id, key string) (runtime.DeliveryReceipt, bool, error) {
	_, sessName, err := m.sessionBead(id)
	if err != nil {
		return runtime.DeliveryReceipt{}, false, err
	}
	r, found, err := runtime.LookupDelivery(ctx, m.sp, sessName, key)
	if err != nil {
		return runtime.DeliveryReceipt{}, false, fmt.Errorf("looking up a delivery to session %s: %w", id, err)
	}
	return r, found, nil
}
