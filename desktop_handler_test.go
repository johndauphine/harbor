package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestDesktopTransport(t *testing.T) {
	app := NewApp()
	handler := desktopHandler(app)
	for _, test := range []struct {
		name, method, url, origin string
		status                    int
	}{
		{"profiles", "POST", "http://wails.localhost/api/profiles", "http://wails.localhost", 200},
		{"foreign origin", "POST", "http://wails.localhost/api/profiles", "https://example.com", 403},
		{"null origin", "POST", "http://wails.localhost/api/profiles", "null", 403},
		{"foreign host", "POST", "http://example.com/api/profiles", "", 403},
		{"wrong scheme", "POST", "https://wails.localhost/api/profiles", "", 403},
		{"get forbidden", "GET", "http://wails.localhost/api/profiles", "", 405},
		{"assets", "GET", "http://wails.localhost/", "", 200},
	} {
		t.Run(test.name, func(t *testing.T) {
			request := httptest.NewRequest(test.method, test.url, strings.NewReader("{}"))
			request.Header.Set("Origin", test.origin)
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			if response.Code != test.status {
				t.Fatalf("status = %d, want %d: %s", response.Code, test.status, response.Body.String())
			}
			if request.Header.Get("Authorization") != "" {
				t.Fatal("handler changed caller's headers")
			}
		})
	}
	app.beginShutdown()
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest("POST", "http://wails.localhost/api/profiles", strings.NewReader("{}")))
	if response.Code != http.StatusServiceUnavailable {
		t.Fatalf("shutdown must reject new work: %d", response.Code)
	}
}

func TestDesktopPickerTransport(t *testing.T) {
	app := NewApp()
	calls := 0
	handler := desktopHandler(app, func(folder bool) ([]desktopUploadFile, error) { calls++; return []desktopUploadFile{}, nil })
	for _, tc := range []struct {
		method, host, origin, body string
		want                       int
	}{
		{"POST", "wails.localhost", "http://wails.localhost", `{"Folder":true}`, 200},
		{"GET", "wails.localhost", "", "{}", 405},
		{"POST", "wails.localhost", "https://example.com", "{}", 403},
		{"POST", "example.com", "", "{}", 403},
		{"POST", "wails.localhost", "", "invalid", 400},
	} {
		response := httptest.NewRecorder()
		request := httptest.NewRequest(tc.method, "http://"+tc.host+"/api/desktop-upload-pick", strings.NewReader(tc.body))
		request.Header.Set("Origin", tc.origin)
		handler.ServeHTTP(response, request)
		if response.Code != tc.want {
			t.Fatalf("%+v: %d", tc, response.Code)
		}
	}
	if calls != 1 {
		t.Fatalf("picker invoked %d times", calls)
	}
	app.beginShutdown()
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest("POST", "http://wails.localhost/api/desktop-upload-pick", strings.NewReader("{}")))
	if response.Code != 503 || calls != 1 {
		t.Fatal("picker admitted after shutdown")
	}
}
