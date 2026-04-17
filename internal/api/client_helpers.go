package api

import "encoding/json"

// jsonUnmarshalTolerant decodes JSON into v, returning errors if any.
// Centralized so the few legitimate JSON-decode call sites in this
// package have one obvious helper. Used by the API client adapter to
// parse Problem Details bodies returned by the server when the
// generated client surfaces a non-2xx response.
func jsonUnmarshalTolerant(data []byte, v any) error {
	if len(data) == 0 {
		return nil
	}
	return json.Unmarshal(data, v)
}
