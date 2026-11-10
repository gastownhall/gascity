package opjournal

import "strconv"

// RuntimeDeliveryKeyPrefix scopes the delivery keys gc hands a runtime in
// the runtime's key space, apart from any other producer's (the nudge
// queue's nudge-batch-<hex>).
const RuntimeDeliveryKeyPrefix = "gc:"

// FormatDeliveryKey is the journal delivery key <head>:<n> that
// validateDeliveryKey accepts for a valid head op id and n >= 1.
func FormatDeliveryKey(head string, n uint64) string {
	return head + ":" + strconv.FormatUint(n, 10)
}

// RuntimeDeliveryKey is the key gc hands a runtime for the journal delivery
// key k: gc:<k>. The runtime derives its own idempotency ids from it; it is
// never delivered text.
func RuntimeDeliveryKey(k string) string {
	return RuntimeDeliveryKeyPrefix + k
}
