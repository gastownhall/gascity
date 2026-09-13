package api

import (
	"encoding/json"
	"net/http"
)

func decodeBody(r *http.Request, v any) error {
	r.Body = http.MaxBytesReader(nil, r.Body, 1<<20) // 1 MiB
	return json.NewDecoder(r.Body).Decode(v)
}
