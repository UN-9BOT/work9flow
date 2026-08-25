package runtime

import "net/http"

// jsonNotFound returns a JSON 404 so all runtime responses share one
// content-type. Hooked into the mux via mux.Handle (no path).
func jsonNotFound(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusNotFound, map[string]string{"error": "not_found"})
}
