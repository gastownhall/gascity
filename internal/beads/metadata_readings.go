package beads

import "encoding/json"

// MetadataReadings returns the string-valued views gc's stores give the
// metadata object raw once bd has stored it: first the bd CLI's (the
// StringMap decode BdStore reads through, which the DoltLite read store
// matches), then the native Dolt store's (metadataMapFromNative). They agree
// on every JSON string value and can differ on any other: a JSON null reads as
// "" through the bd CLI and as the text "null" from the native store, and a
// number keeps its spelling in one and is re-encoded in the other. A city reads
// the same row through whichever store it opens, so a caller deciding
// something from a row bd is about to store, such as the row's storage class,
// decides it under every reading.
func MetadataReadings(raw []byte) ([]map[string]string, error) {
	var cli StringMap
	if err := json.Unmarshal(raw, &cli); err != nil {
		return nil, err
	}
	native, err := metadataMapFromNative(raw)
	if err != nil {
		return nil, err
	}
	return []map[string]string{cli, native}, nil
}
