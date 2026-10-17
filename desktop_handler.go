package main

import "net/http"

// desktopHandler is only mounted in Wails' private asset server. No TCP
// listener or browser session token is exposed by the desktop application.
// Reuse the browser API's admission, cancellation, validation and size limits.
func desktopHandler(app *App) http.Handler {
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
		api.ServeHTTP(w, request)
	})
}
