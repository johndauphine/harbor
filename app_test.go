package main

import (
	"bytes"
	"context"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
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
	a.secretDelete = func(k string) error { delete(vault, k); return nil }
	p, e := a.SaveProfile(Profile{Name: "Test", Region: "us-east-1"}, Secret{AccessKey: "fixture-access", SecretKey: "fixture-secret"})
	if e != nil {
		t.Fatal(e)
	}
	b, _ := os.ReadFile(a.configFile)
	if strings.Contains(string(b), "fixture-") {
		t.Fatal("secret persisted to metadata")
	}
	db, err := a.openStore()
	if err != nil {
		t.Fatal(err)
	}
	var raw string
	if err = db.QueryRow("SELECT metadata FROM profiles WHERE id=?", p.ID).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	db.Close()
	var saved Profile
	if json.Unmarshal([]byte(raw), &saved) != nil || saved.Name != "Test" {
		t.Fatal("profile did not round-trip through SQLite")
	}
	if !strings.Contains(vault[credentialID(p)], "fixture-secret") {
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

func TestDownloadCancelRetryAndUpload(t *testing.T) {
	var slow atomic.Bool
	slow.Store(true)
	var uploaded atomic.Bool
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "PUT" {
			data, _ := io.ReadAll(r.Body)
			if !bytes.Contains(data, []byte("fixture-payload")) {
				t.Error("upload payload missing")
			}
			uploaded.Store(true)
			return
		}
		if slow.Load() {
			w.Header().Set("Content-Length", "100000")
			w.WriteHeader(200)
			w.Write([]byte("partial"))
			w.(http.Flusher).Flush()
			<-r.Context().Done()
			return
		}
		w.Header().Set("Content-Length", "15")
		w.Write([]byte("fixture-payload"))
	}))
	defer server.Close()
	a := NewApp()
	a.ctx = context.Background()
	a.profiles = []Profile{{ID: "fixture", Name: "Fixture", Region: "us-east-1", Endpoint: server.URL, PathStyle: true}}
	raw, _ := json.Marshal(Secret{AccessKey: "fixture", SecretKey: "fixture"})
	a.secretGet = func(string) (string, error) { return string(raw), nil }
	ca := filepath.Join(t.TempDir(), "ca.pem")
	os.WriteFile(ca, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw}), 0600)
	a.profiles[0].CAFile = ca
	path := filepath.Join(t.TempDir(), "download")
	id := a.queue("download", "fixture", "bucket", "object", path)
	wait := func(status string) {
		t.Helper()
		deadline := time.Now().Add(5 * time.Second)
		for time.Now().Before(deadline) {
			a.mu.Lock()
			state := a.transfers[id].Status
			a.mu.Unlock()
			if state == status {
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
		t.Fatalf("did not reach %s", status)
	}
	wait("running")
	a.CancelTransfer(id)
	wait("cancelled")
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("partial download committed")
	}
	slow.Store(false)
	if err := a.RetryTransfer(id); err != nil {
		t.Fatal(err)
	}
	wait("completed")
	data, _ := os.ReadFile(path)
	if string(data) != "fixture-payload" {
		t.Fatalf("incorrect downloaded data %q", data)
	}

	destinationDir := t.TempDir()
	result, err := a.QueueDownloadTo("fixture", "bucket", "object", destinationDir, "chosen.txt", false)
	if err != nil {
		t.Fatal(err)
	}
	id = result.ID
	wait("completed")
	data, _ = os.ReadFile(result.Path)
	if string(data) != "fixture-payload" {
		t.Fatal("automatic save content incorrect")
	}
	a.mu.Lock()
	saved := *a.transfers[id]
	a.mu.Unlock()
	if saved.SavedPath != result.Path || saved.SavedAt.IsZero() {
		t.Fatal("automatic save confirmation missing")
	}
	count := len(a.Transfers())
	confirmation, err := a.QueueDownloadTo("fixture", "bucket", "object", destinationDir, "chosen.txt", false)
	if err != nil || !confirmation.NeedsOverwrite || len(a.Transfers()) != count {
		t.Fatal("overwrite queued without confirmation")
	}
	slow.Store(true)
	replacement, err := a.QueueDownloadTo("fixture", "bucket", "object", destinationDir, "chosen.txt", true)
	if err != nil {
		t.Fatal(err)
	}
	id = replacement.ID
	wait("running")
	a.CancelTransfer(id)
	wait("cancelled")
	data, _ = os.ReadFile(result.Path)
	if string(data) != "fixture-payload" {
		t.Fatal("cancelled replacement damaged existing file")
	}
	slow.Store(false)
	if err := a.RetryTransfer(id); err != nil {
		t.Fatal(err)
	}
	wait("completed")
	data, _ = os.ReadFile(result.Path)
	if string(data) != "fixture-payload" {
		t.Fatal("shutdown removed saved download")
	}
	upload := filepath.Join(t.TempDir(), "upload")
	os.WriteFile(upload, []byte("fixture-payload"), 0600)
	id, err = a.QueueLocalUpload(context.Background(), "fixture", "bucket", "object", upload)
	if err != nil {
		t.Fatal(err)
	}
	wait("completed")
	a.shutdown(context.Background())
	data, _ = os.ReadFile(result.Path)
	if string(data) != "fixture-payload" {
		t.Fatal("shutdown removed saved download")
	}
	data, _ = os.ReadFile(upload)
	if string(data) != "fixture-payload" {
		t.Fatal("upload removed original local file")
	}
	if !uploaded.Load() {
		t.Fatal("upload not received")
	}
}

func TestCreateFolderMarker(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		if r.Method != "PUT" || r.URL.Path != "/bucket/parent/New folder/" {
			t.Errorf("unexpected folder request: %s %s", r.Method, r.URL.Path)
		}
		if r.Header.Get("If-None-Match") != "*" {
			t.Error("missing overwrite protection")
		}
		data, _ := io.ReadAll(r.Body)
		if len(data) != 0 {
			t.Error("folder marker is not empty")
		}
		if requests.Load() > 1 {
			w.WriteHeader(http.StatusPreconditionFailed)
			return
		}
	}))
	defer server.Close()
	a := NewApp()
	a.ctx = context.Background()
	a.profiles = []Profile{{ID: "fixture", Name: "Fixture", Region: "us-east-1", Endpoint: server.URL, PathStyle: true}}
	raw, _ := json.Marshal(Secret{AccessKey: "fixture", SecretKey: "fixture"})
	a.secretGet = func(string) (string, error) { return string(raw), nil }
	ca := filepath.Join(t.TempDir(), "ca.pem")
	os.WriteFile(ca, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw}), 0600)
	a.profiles[0].CAFile = ca
	key, err := a.CreateFolder("fixture", "bucket", "parent/", "New folder")
	if err != nil || key != "parent/New folder/" {
		t.Fatalf("folder creation: %q %v", key, err)
	}
	if _, err = a.CreateFolder("fixture", "bucket", "parent/", "New folder"); err == nil {
		t.Fatal("existing marker was overwritten")
	}
	for _, name := range []string{"", " ", "..", "nested/folder", "bad\\name", "bad\nname"} {
		if _, err = a.CreateFolder("fixture", "bucket", "parent/", name); err == nil {
			t.Errorf("accepted invalid name %q", name)
		}
	}
	if requests.Load() != 2 {
		t.Fatalf("invalid inputs caused requests: %d", requests.Load())
	}
}

func TestTransferListingKeepsCreationOrder(t *testing.T) {
	a := NewApp()
	a.ctx = context.Background()
	defer a.shutdown(context.Background())
	ids := []string{}
	for i := 0; i < 20; i++ {
		ids = append(ids, a.queue("download", "missing-profile", "fixture", "key", ""))
	}
	for n := 0; n < 30; n++ {
		list := a.Transfers()
		if len(list) != len(ids) {
			t.Fatalf("got %d transfers", len(list))
		}
		for i, v := range list {
			if v.ID != ids[i] {
				t.Fatalf("transfer order changed at position %d", i)
			}
		}
	}
}

func TestEditProfilePreservesAndReplacesCredentials(t *testing.T) {
	a := NewApp()
	a.configFile = filepath.Join(t.TempDir(), "profiles.db")
	vault := map[string]string{}
	a.secretSet = func(k, v string) error { vault[k] = v; return nil }
	a.secretGet = func(k string) (string, error) { return vault[k], nil }
	a.secretDelete = func(k string) error { delete(vault, k); return nil }
	p, err := a.SaveProfile(Profile{Name: "Original", Region: "us-east-1"}, Secret{AccessKey: "old-access", SecretKey: "old-secret", SessionToken: "old-token"})
	if err != nil {
		t.Fatal(err)
	}
	old := vault[credentialID(p)]
	p.Name = "Updated"
	if _, err = a.SaveProfile(p, Secret{}); err != nil {
		t.Fatal(err)
	}
	if vault[credentialID(p)] != old || len(a.Profiles()) != 1 {
		t.Fatal("metadata edit changed credentials or created duplicate")
	}
	if p, err = a.SaveProfile(p, Secret{AccessKey: "updated-access"}); err != nil {
		t.Fatal(err)
	}
	var partial Secret
	json.Unmarshal([]byte(vault[credentialID(p)]), &partial)
	if partial.AccessKey != "updated-access" || partial.SecretKey != "old-secret" {
		t.Fatal("single field edit did not preserve the other key")
	}
	if p, err = a.SaveProfile(p, Secret{AccessKey: "new-access", SecretKey: "new-secret"}); err != nil {
		t.Fatal(err)
	}
	var replacement Secret
	if err = json.Unmarshal([]byte(vault[credentialID(p)]), &replacement); err != nil {
		t.Fatal(err)
	}
	if replacement.AccessKey != "new-access" || replacement.SessionToken != "" {
		t.Fatal("replacement did not replace old credentials completely")
	}
}

func TestDeleteProfileRemovesOnlyLocalSettingsAndCredentials(t *testing.T) {
	a := NewApp()
	a.configFile = filepath.Join(t.TempDir(), "profiles.db")
	vault := map[string]string{}
	a.secretSet = func(k, v string) error { vault[k] = v; return nil }
	a.secretGet = func(k string) (string, error) { return vault[k], nil }
	a.secretDelete = func(k string) error { delete(vault, k); return nil }
	fail := true
	a.secretDelete = func(k string) error {
		if fail {
			return fmt.Errorf("vault locked")
		}
		delete(vault, k)
		return nil
	}
	p, err := a.SaveProfile(Profile{Name: "Disposable", Region: "us-east-1"}, Secret{AccessKey: "test", SecretKey: "test"})
	if err != nil {
		t.Fatal(err)
	}
	if err = a.DeleteProfile(p.ID); err == nil {
		t.Fatal("expected vault failure")
	}
	if len(a.Profiles()) != 0 || vault[credentialID(p)] == "" {
		t.Fatal("committed deletion should remove metadata and retain credentials for retry")
	}
	fail = false
	if err = a.RetryCredentialCleanup(); err != nil {
		t.Fatal(err)
	}
	if vault[credentialID(p)] != "" {
		t.Fatal("retry left deleted credentials")
	}
	p, err = a.SaveProfile(Profile{Name: "Disposable", Region: "us-east-1"}, Secret{AccessKey: "test", SecretKey: "test"})
	if err != nil {
		t.Fatal(err)
	}
	a.transfers["active"] = &Transfer{ProfileID: p.ID, Status: "running"}
	if err = a.DeleteProfile(p.ID); err == nil {
		t.Fatal("deleted active connection")
	}
	delete(a.transfers, "active")
	if err = a.DeleteProfile(p.ID); err != nil {
		t.Fatal(err)
	}
	if len(a.Profiles()) != 0 || vault[credentialID(p)] != "" {
		t.Fatal("profile or credentials remain")
	}
	db, err := a.openStore()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var count int
	if err = db.QueryRow("SELECT count(*) FROM profiles").Scan(&count); err != nil || count != 0 {
		t.Fatalf("metadata remains: %d %v", count, err)
	}
}

func TestDeleteBucketNeverDeletesContents(t *testing.T) {
	var calls int
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.Method != "DELETE" || (r.URL.Path != "/empty" && r.URL.Path != "/occupied") || r.URL.RawQuery != "" {
			t.Errorf("unexpected operation: %s %s", r.Method, r.URL)
			w.WriteHeader(400)
			return
		}
		if r.URL.Path == "/occupied" {
			w.WriteHeader(409)
			w.Write([]byte(`<Error><Code>BucketNotEmpty</Code><Message>Not empty</Message></Error>`))
			return
		}
		w.WriteHeader(204)
	}))
	defer server.Close()
	a := NewApp()
	a.ctx = context.Background()
	ca := filepath.Join(t.TempDir(), "ca.pem")
	os.WriteFile(ca, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw}), 0600)
	a.profiles = []Profile{{ID: "test", Name: "Test", Region: "us-east-1", Endpoint: server.URL, PathStyle: true, CAFile: ca}}
	a.secretGet = func(string) (string, error) { return `{"AccessKey":"fixture","SecretKey":"fixture"}`, nil }
	if err := a.DeleteBucket("test", "occupied"); err == nil {
		t.Fatal("nonempty bucket accepted")
	}
	if err := a.DeleteBucket("test", "empty"); err != nil {
		t.Fatal(err)
	}
	if calls != 2 {
		t.Fatalf("unexpected calls: %d", calls)
	}
}
