package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestCommittedCredentialCleanupWarningAPI(t *testing.T) {
	a, _ := credentialFixture(t)
	p := fixtureProfile(t, a)
	a.secretDelete = func(string) error { return errors.New("synthetic locked vault") }
	h := webHandler(a, "127.0.0.1:1234", "fixture-session")
	call := func(route string, payload any) *httptest.ResponseRecorder {
		t.Helper()
		b, err := json.Marshal(payload)
		if err != nil {
			t.Fatal(err)
		}
		r := httptest.NewRequest("POST", "http://127.0.0.1:1234/api/"+route, bytes.NewReader(b))
		r.Header.Set("Authorization", "Bearer fixture-session")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w
	}
	p.Name = "Updated fixture"
	w := call("save", map[string]any{"Profile": p, "Secret": Secret{AccessKey: "replacement-fixture"}})
	if w.Code != 200 {
		t.Fatalf("committed save reported failure: %d %s", w.Code, w.Body)
	}
	var saved struct {
		Value   Profile
		Warning string
	}
	if err := json.Unmarshal(w.Body.Bytes(), &saved); err != nil {
		t.Fatal(err)
	}
	if saved.Value.ID != p.ID || !strings.Contains(saved.Warning, "cleanup is pending") || a.Profiles()[0].Name != p.Name {
		t.Fatalf("committed save not accurately represented: %s", w.Body)
	}
	if strings.Contains(w.Body.String(), "replacement-fixture") || strings.Contains(w.Body.String(), "credentialID") {
		t.Fatal("credential details exposed")
	}
	w = call("delete-profile", map[string]any{"ProfileID": p.ID})
	if w.Code != 200 || !strings.Contains(w.Body.String(), "Connection deleted") || !strings.Contains(w.Body.String(), "cleanup is pending") || len(a.Profiles()) != 0 {
		t.Fatalf("committed deletion not accurately represented: %d %s", w.Code, w.Body)
	}
}
