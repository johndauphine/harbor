package main

import (
	"encoding/json"
	"net/http"
)

// desktopHandler is only mounted in Wails' private asset server. No TCP
// listener or browser session token is exposed by the desktop application.
// Reuse the browser API's admission, cancellation, validation and size limits.
func desktopHandler(app *App, pickers ...func(bool) ([]desktopUploadFile, error)) http.Handler {
	const host = "wails.localhost"
	const token = "private-desktop-transport"
	api := webHandler(app, host, token)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Host != host || (r.URL.Scheme != "" && r.URL.Scheme != "http") {
			http.Error(w, "Invalid desktop host", http.StatusForbidden)
			return
		}
		if origin := r.Header.Get("Origin"); origin != "" && origin != "http://"+host {
			http.Error(w, "Invalid desktop origin", http.StatusForbidden)
			return
		}
		request := r.Clone(r.Context())
		request.Header.Set("Authorization", "Bearer "+token)
		if r.URL.Path == "/api/desktop-upload-pick" && len(pickers) > 0 {
			_, done, err := app.beginOperation(r.Context())
			if err != nil {
				http.Error(w, err.Error(), 503)
				return
			}
			defer done()
			if r.Method != http.MethodPost {
				http.Error(w, "POST required", 405)
				return
			}
			var in struct{ Folder bool }
			if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1024)).Decode(&in); err != nil {
				http.Error(w, "Invalid picker request", 400)
				return
			}
			files, err := pickers[0](in.Folder)
			w.Header().Set("Content-Type", "application/json")
			if err != nil {
				w.WriteHeader(400)
				json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
				return
			}
			json.NewEncoder(w).Encode(files)
			return
		}
		api.ServeHTTP(w, request)
	})
}
