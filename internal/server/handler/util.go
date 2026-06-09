package handler

import (
	"encoding/json"
	"net/http"
	"strings"
)

func jsonOK(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v) //nolint:errcheck
}

func jsonError(w http.ResponseWriter, msg string, code int) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(map[string]string{"error": msg}) //nolint:errcheck
}

// safeName converts a server_id to a safe PostgreSQL identifier segment.
func safeName(s string) string {
	return strings.NewReplacer("-", "_", ".", "_", " ", "_").Replace(strings.ToLower(s))
}

// nullJSON returns nil when the JSON is empty or "null".
func nullJSON(b json.RawMessage) any {
	if len(b) == 0 || string(b) == "null" {
		return nil
	}
	return b
}
