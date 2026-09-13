package api

import (
	"encoding/json"
	"net/http"
)

type problemDetails struct {
	Title  string `json:"title"`
	Status int    `json:"status"`
	Detail string `json:"detail"`
}

func writeError(w http.ResponseWriter, status int, code, message string) {
	detail := message
	if code != "" {
		detail = code + ": " + message
	}
	title := http.StatusText(status)
	if title == "" {
		title = "Error"
	}
	writeJSONWithType(w, status, "application/problem+json", problemDetails{
		Title:  title,
		Status: status,
		Detail: detail,
	})
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	writeJSONWithType(w, status, "application/json", value)
}

func writeJSONWithType(w http.ResponseWriter, status int, contentType string, value any) {
	w.Header().Set("Content-Type", contentType)
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}
