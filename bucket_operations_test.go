package main

import (
	"context"
	"encoding/json"
	"encoding/pem"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestBucketNameValidation(t *testing.T) {
	for _, name := range []string{"abc", "my-bucket-123", "files.example.com", strings.Repeat("a", 63)} {
		if err := validateBucketName(name); err != nil {
			t.Errorf("valid %q: %v", name, err)
		}
	}
	for _, name := range []string{"", "ab", strings.Repeat("a", 64), "UPPER", "under_score", "-first", "last-", "has space", "bad..dots", "127.0.0.1", "xn--bucket", "sthree-bucket", "amzn-s3-demo-bucket", "bucket-s3alias", "bucket--ol-s3", "bucket.mrap", "bucket--x-s3", "bucket--table-s3", "bucket-an", "bucket/child", "bucket\n"} {
		if validateBucketName(name) == nil {
			t.Errorf("accepted invalid name %q", name)
		}
	}
}

func TestCreateBucketRegionAndExistingProtection(t *testing.T) {
	for _, region := range []string{"us-east-1", "eu-west-1"} {
		t.Run(region, func(t *testing.T) {
			exists := false
			puts := 0
			server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/new-test-bucket" {
					t.Errorf("unexpected path %q", r.URL.Path)
				}
				if r.Method == "HEAD" {
					if !exists {
						w.WriteHeader(404)
					}
					return
				}
				if r.Method != "PUT" {
					t.Errorf("unexpected method %s", r.Method)
					w.WriteHeader(400)
					return
				}
				puts++
				body, _ := io.ReadAll(r.Body)
				if region == "us-east-1" && len(body) != 0 {
					t.Errorf("us-east-1 must omit location constraint: %s", body)
				}
				if region != "us-east-1" && !strings.Contains(string(body), "<LocationConstraint>"+region+"</LocationConstraint>") {
					t.Errorf("missing region: %s", body)
				}
				if r.Header.Get("x-amz-acl") != "" {
					t.Error("unexpected ACL change")
				}
				exists = true
			}))
			defer server.Close()
			a := bucketTestApp(t, server, region)
			name, err := a.CreateBucket("fixture", "new-test-bucket")
			if err != nil || name != "new-test-bucket" {
				t.Fatalf("create: %q %v", name, err)
			}
			if _, err = a.CreateBucket("fixture", "new-test-bucket"); err == nil {
				t.Fatal("existing bucket accepted")
			}
			if puts != 1 {
				t.Fatalf("existing bucket received mutation: %d PUTs", puts)
			}
		})
	}
}

func TestCreateBucketRefusesUncertainAvailabilityAndDoesNotReplayMutation(t *testing.T) {
	for _, headStatus := range []int{403, 404} {
		t.Run(http.StatusText(headStatus), func(t *testing.T) {
			puts := 0
			server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == "HEAD" {
					w.WriteHeader(headStatus)
					return
				}
				puts++
				w.WriteHeader(500)
				io.WriteString(w, `<Error><Code>InternalError</Code></Error>`)
			}))
			defer server.Close()
			a := bucketTestApp(t, server, "us-east-1")
			if _, err := a.CreateBucket("fixture", "new-test-bucket"); err == nil {
				t.Fatal("expected failure")
			}
			expected := 0
			if headStatus == 404 {
				expected = 1
			}
			if puts != expected {
				t.Fatalf("got %d mutation requests, want %d", puts, expected)
			}
		})
	}
}

func bucketTestApp(t *testing.T, server *httptest.Server, region string) *App {
	t.Helper()
	a := NewApp()
	a.ctx = context.Background()
	a.profiles = []Profile{{ID: "fixture", Name: "Fixture", Region: region, Endpoint: server.URL, PathStyle: true}}
	raw, _ := json.Marshal(Secret{AccessKey: "fixture", SecretKey: "fixture"})
	a.secretGet = func(string) (string, error) { return string(raw), nil }
	ca := filepath.Join(t.TempDir(), "ca.pem")
	if err := os.WriteFile(ca, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw}), 0600); err != nil {
		t.Fatal(err)
	}
	a.profiles[0].CAFile = ca
	return a
}
