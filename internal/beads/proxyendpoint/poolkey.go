package proxyendpoint

import "fmt"

// PoolKey identifies one proxy generation serving one database. A connection
// pool is keyed by this and never by the port alone.
//
// The port alone is not an identity. bd allocates a fresh port on most respawns,
// but a sidecar that pins `--proxied-server-port` gets the same port back for a
// different process over a different Dolt child — and the reverse, a moved root
// whose replacement is recreated at the same path, yields the same root_id for a
// different database. {RootID, PID, Birth} together name the process generation,
// Port names where it listens, and Database names which of its databases this
// pool speaks to: a rig that shares its city's proxy root legitimately differs
// from the city in Database alone, and the key admits that.
type PoolKey struct {
	// RootID is the proxy root's path identity (Record.RootID).
	RootID string
	// PID and Birth are the process generation. Birth is what makes PID an
	// identity rather than a number.
	PID   int
	Birth string
	// Port is the proxy's loopback data port.
	Port int
	// Database is the Dolt database this pool selects.
	Database string
}

// NewPoolKey derives the key for a validated record and a database name.
func NewPoolKey(rec Record, database string) PoolKey {
	return PoolKey{RootID: rec.RootID, PID: rec.PID, Birth: rec.Birth, Port: rec.Port, Database: database}
}

// Generation renders the process generation alone: the pair that distinguishes
// one proxy from its own replacement on the same port.
//
// The birth token is fingerprinted rather than printed. It embeds the host's
// boot id, which has no place in a log line or a doctor payload, and a reader
// only ever asks whether two generations are the same one — which a digest
// answers exactly.
func (k PoolKey) Generation() string {
	return fmt.Sprintf("%d:%s", k.PID, ShortDigest(k.Birth))
}

// String renders the whole key for a log line or a diagnostic. It is not a
// parsing format; equality is done on the struct, which is comparable.
func (k PoolKey) String() string {
	return fmt.Sprintf("root=%s gen=%s port=%d db=%s", ShortID(k.RootID), k.Generation(), k.Port, k.Database)
}

// SameGeneration reports whether two keys name the same proxy process on the
// same port under the same root — the question the guard tick asks, where a
// different database on one proxy is not a different generation.
func (k PoolKey) SameGeneration(other PoolKey) bool {
	return k.RootID == other.RootID && k.PID == other.PID && k.Birth == other.Birth && k.Port == other.Port
}
