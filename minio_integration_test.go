package main

import (
	"bytes"
	"context"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Explicitly opt in: this test writes only to the local MinIO fixture.
func TestLocalMinIOIntegration(t *testing.T) {
	if os.Getenv("HARBOR_MINIO_TEST") != "1" {
		t.Skip("local MinIO integration is opt-in")
	}
	a := NewApp()
	a.startup(context.Background())
	defer a.shutdown(context.Background())
	var p Profile
	for _, v := range a.Profiles() {
		if v.Name == "MinIO Local" && v.Endpoint == "https://127.0.0.1:59000" {
			p = v
		}
	}
	if p.ID == "" {
		t.Fatal("local fixture profile not found")
	}
	c, err := a.client(context.Background(), p.ID)
	if err != nil {
		t.Fatal(err)
	}
	bucket := "harbor-test-" + strings.ToLower(id()[:12])
	_, err = c.CreateBucket(context.Background(), &s3.CreateBucketInput{Bucket: aws.String(bucket)})
	if err != nil {
		t.Fatal(err)
	}
	buckets, err := a.Buckets(p.ID)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, b := range buckets {
		if b == bucket {
			found = true
		}
	}
	if !found {
		t.Fatal("new bucket missing")
	}
	if _, err = a.CreateFolder(p.ID, bucket, "", "sample"); err != nil {
		t.Fatal(err)
	}
	if _, err = a.CreateFolder(p.ID, bucket, "", "sample"); err == nil {
		t.Fatal("existing folder marker unexpectedly overwritten")
	}
	payload := bytes.Repeat([]byte("Harbor MinIO multipart test\n"), 250000)
	source := filepath.Join(t.TempDir(), "fixture.txt")
	if err = os.WriteFile(source, payload, 0600); err != nil {
		t.Fatal(err)
	}
	wait := func(id string) {
		t.Helper()
		until := time.Now().Add(45 * time.Second)
		for time.Now().Before(until) {
			a.mu.Lock()
			state := *a.transfers[id]
			a.mu.Unlock()
			if state.Status == "completed" {
				return
			}
			if state.Status == "failed" {
				t.Fatal(state.Error)
			}
			time.Sleep(20 * time.Millisecond)
		}
		t.Fatal("transfer timed out")
	}
	upload := a.queue("upload", p.ID, bucket, "sample/fixture.txt", source)
	wait(upload)
	root, err := a.Browse(p.ID, bucket, "", "")
	if err != nil {
		t.Fatal(err)
	}
	if len(root.Objects) != 1 || !root.Objects[0].Folder {
		t.Fatalf("unexpected root listing: %+v", root)
	}
	page, err := a.Browse(p.ID, bucket, "sample/", "")
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Objects) != 1 || page.Objects[0].Size != int64(len(payload)) {
		t.Fatalf("unexpected folder listing: %+v", page)
	}
	download, err := a.QueueDownload(p.ID, bucket, "sample/fixture.txt")
	if err != nil {
		t.Fatal(err)
	}
	wait(download)
	a.mu.Lock()
	path := a.transfers[download].Path
	a.mu.Unlock()
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, payload) {
		t.Fatal("download bytes differ")
	}
	t.Log("Verified HTTPS/custom CA, bucket listing, folder marker creation, conditional write, multipart upload, folder browsing, and byte-identical download")
}
