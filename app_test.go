package main

import (
	"context"
	"encoding/json"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestBrowserBoundary(t *testing.T) {
	a := NewApp()
	h := webHandler(a, "127.0.0.1:1234", "secret")
	for _, tc := range []struct {
		host, token, origin string
		status              int
	}{{"evil.example", "secret", "", 403}, {"127.0.0.1:1234", "", "", 401}, {"127.0.0.1:1234", "secret", "https://evil.example", 403}, {"127.0.0.1:1234", "secret", "http://127.0.0.1:1234", 200}} {
		r := httptest.NewRequest("POST", "http://"+tc.host+"/api/profiles", strings.NewReader("{}"))
		r.Header.Set("Authorization", "Bearer "+tc.token)
		r.Header.Set("Origin", tc.origin)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != tc.status {
			t.Fatalf("got %d want %d", w.Code, tc.status)
		}
	}
}
func TestSecretsStayOutOfProfiles(t *testing.T) {
	a := NewApp()
	a.configFile = filepath.Join(t.TempDir(), "profiles.json")
	vault := map[string]string{}
	a.secretSet = func(k, v string) error { vault[k] = v; return nil }
	a.secretGet = func(k string) (string, error) { return vault[k], nil }
	p, e := a.SaveProfile(Profile{Name: "Test", Region: "us-east-1"}, Secret{AccessKey: "fixture-access", SecretKey: "fixture-secret"})
	if e != nil {
		t.Fatal(e)
	}
	b, _ := os.ReadFile(a.configFile)
	if strings.Contains(string(b), "fixture-") {
		t.Fatal("secret persisted to metadata")
	}
	if !strings.Contains(vault[p.ID], "fixture-secret") {
		t.Fatal("vault missing secret")
	}
}
func TestHTTPSOnly(t *testing.T) {
	if validate(Profile{Name: "Test", Region: "us-east-1", Endpoint: "http://example.com"}) == nil {
		t.Fatal("plaintext endpoint accepted")
	}
}
func TestS3PrefixPagination(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("prefix") != "folder/" || r.URL.Query().Get("delimiter") != "/" || r.URL.Query().Get("continuation-token") != "page1" {
			t.Errorf("unexpected query %s", r.URL.RawQuery)
		}
		w.Header().Set("Content-Type", "application/xml")
		w.Write([]byte(`<ListBucketResult xmlns="http://s3.amazonaws.com/doc/2006-03-01/"><Name>bucket</Name><Prefix>folder/</Prefix><IsTruncated>true</IsTruncated><NextContinuationToken>page2</NextContinuationToken><CommonPrefixes><Prefix>folder/child/</Prefix></CommonPrefixes><Contents><Key>folder/file.txt</Key><Size>12</Size><LastModified>2026-01-01T00:00:00Z</LastModified><ETag>etag</ETag></Contents></ListBucketResult>`))
	}))
	defer server.Close()
	a := NewApp()
	a.ctx = context.Background()
	a.profiles = []Profile{{ID: "test", Name: "Test", Region: "us-east-1", Endpoint: server.URL, PathStyle: true}}
	raw, _ := json.Marshal(Secret{AccessKey: "fixture", SecretKey: "fixture"})
	a.secretGet = func(string) (string, error) { return string(raw), nil } // Trust only the fixture's certificate.
	cert := server.Certificate()
	ca := filepath.Join(t.TempDir(), "ca.pem")
	os.WriteFile(ca, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: cert.Raw}), 0600)
	a.profiles[0].CAFile = ca
	page, e := a.Browse("test", "bucket", "folder/", "page1")
	if e != nil {
		t.Fatal(e)
	}
	if page.Next != "page2" || len(page.Objects) != 2 || !page.Objects[0].Folder || page.Objects[1].Size != 12 {
		t.Fatalf("unexpected page %+v", page)
	}
}
