package main

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	smithyhttp "github.com/aws/smithy-go/transport/http"
)

// Compare the actual journaled upload engine with rotated baseline/candidate
// runs, retaining durable staging, source hashing and multipart checkpoints.
func TestAWSUploadEngineTuning(t *testing.T) {
	if os.Getenv("HARBOR_AWS_UPLOAD_ENGINE") != "1" {
		t.Skip("set HARBOR_AWS_UPLOAD_ENGINE=1 for real AWS upload engine measurements")
	}
	candidatePart := int64(32 << 20)
	if value := os.Getenv("HARBOR_UPLOAD_PART_MIB"); value != "" {
		mib, err := strconv.ParseInt(value, 10, 64)
		mustPerf(t, err)
		candidatePart = mib << 20
	}
	if !validUploadPartSize(candidatePart) {
		t.Fatal("unsupported candidate part size")
	}
	root := t.TempDir()
	a, profile, c := perfAWSConnection(t, root)
	defer a.shutdown(context.Background())
	bucket := "harbor-tuning-" + id()
	_, err := a.CreateBucket(profile.ID, bucket)
	mustPerf(t, err)
	keys := map[string]bool{}
	defer func() {
		_ = a.shutdown(context.Background())
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		uploads, e := c.ListMultipartUploads(ctx, &s3.ListMultipartUploadsInput{Bucket: aws.String(bucket)})
		if e != nil {
			t.Error(e)
		} else {
			for _, u := range uploads.Uploads {
				if keys[aws.ToString(u.Key)] {
					_, e = c.AbortMultipartUpload(ctx, &s3.AbortMultipartUploadInput{Bucket: aws.String(bucket), Key: u.Key, UploadId: u.UploadId})
					if e != nil {
						t.Error(e)
					}
				}
			}
		}
		for key := range keys {
			_, e = c.DeleteObject(ctx, &s3.DeleteObjectInput{Bucket: aws.String(bucket), Key: aws.String(key)})
			if e != nil {
				t.Error(e)
			}
		}
		_, e = c.DeleteBucket(ctx, &s3.DeleteBucketInput{Bucket: aws.String(bucket)})
		if e != nil {
			t.Error(e)
		} else {
			t.Log("Removed disposable engine tuning bucket")
		}
	}()
	report := filepath.Join("dist", "aws-upload-engine")
	mustPerf(t, os.MkdirAll(report, 0700))
	manifest, _ := json.Marshal(map[string]string{"bucket": bucket, "region": profile.Region})
	mustPerf(t, os.WriteFile(filepath.Join(report, "bucket-manifest.json"), manifest, 0600))
	output, err := os.Create(filepath.Join(report, "measurements.jsonl"))
	mustPerf(t, err)
	defer output.Close()
	encoder := json.NewEncoder(output)
	source := filepath.Join(root, "source.bin")
	generated, err := os.Create(source)
	mustPerf(t, err)
	_, err = io.CopyN(generated, rand.Reader, 5<<30)
	mustPerf(t, err)
	mustPerf(t, generated.Sync())
	mustPerf(t, generated.Close())
	variants := []struct {
		name    string
		part    int64
		workers int
	}{{"baseline-5MiB-5", 5 << 20, 5}, {fmt.Sprintf("candidate-%dMiB-8", candidatePart>>20), candidatePart, 8}}
	for _, size := range []int64{1 << 30, 5 << 30} {
		trials := 3
		if size == 5<<30 {
			trials = 1
		}
		expected := perfHash(t, source, size)
		for trial := 0; trial < trials; trial++ {
			for j := range variants {
				v := variants[(j+trial)%len(variants)]
				if size == 5<<30 && j == 0 {
					continue
				}
				key := fmt.Sprintf("%s-%d.bin", v.name, size>>20)
				keys[key] = true
				// All worker activity has settled before adding the next direct benchmark
				// transfer. Actual queue-based downloads independently verify the result.
				a.workers.Wait()
				tr := journalEntry(t, a, profile, "upload", key)
				tr.Bucket = bucket
				tr.Total = size
				runtime.GC()
				meter := startPerfMeter()
				started := time.Now()
				sourceFile, e := os.Open(source)
				mustPerf(t, e)
				staged, e := os.OpenFile(tr.Path, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0600)
				mustPerf(t, e)
				_, e = io.CopyN(staged, sourceFile, size)
				sourceFile.Close()
				mustPerf(t, e)
				mustPerf(t, staged.Sync())
				checkpoint(t, a, tr)
				staging := time.Since(started)
				e = a.uploadMultipart(context.Background(), c, *tr, staged, size, v.part, v.workers)
				record := meter.stop(v.name, fmt.Sprintf("%dMiB", size>>20), trial+1, size, time.Since(started), staging)
				staged.Close()
				mustPerf(t, e)
				mustPerf(t, os.Remove(tr.Path))
				mustPerf(t, encoder.Encode(record))
				mustPerf(t, output.Sync())
				result, e := a.QueueDownloadTo(profile.ID, bucket, key, root, "verify.bin", true)
				mustPerf(t, e)
				perfWait(t, a, result.ID, "completed")
				if perfHash(t, result.Path, size) != expected {
					t.Fatal("production upload checksum mismatch")
				}
				mustPerf(t, os.Remove(result.Path))
				t.Logf("%s %dMiB run %d: %.1f MiB/s, %.2fs, staging %.2fs, heap %.1f MiB; SHA-256 matched", v.name, size>>20, trial+1, record.MiBPerSecond, record.Seconds, record.StagingSeconds, record.PeakHeapMiB)
			}
		}
	}
}

func TestAWSTunedUploadRecovery(t *testing.T) {
	if os.Getenv("HARBOR_AWS_UPLOAD_RECOVERY") != "1" {
		t.Skip("set HARBOR_AWS_UPLOAD_RECOVERY=1 for real AWS tuned upload recovery")
	}
	root := t.TempDir()
	a, profile, c := perfAWSConnection(t, root)
	bucket := "harbor-tuning-" + id()
	_, err := a.CreateBucket(profile.ID, bucket)
	mustPerf(t, err)
	defer func() {
		_ = a.shutdown(context.Background())
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		uploads, e := c.ListMultipartUploads(ctx, &s3.ListMultipartUploadsInput{Bucket: aws.String(bucket)})
		if e != nil {
			t.Error(e)
		} else {
			for _, u := range uploads.Uploads {
				if aws.ToString(u.Key) == "recovery.bin" {
					_, e = c.AbortMultipartUpload(ctx, &s3.AbortMultipartUploadInput{Bucket: aws.String(bucket), Key: u.Key, UploadId: u.UploadId})
					if e != nil {
						t.Error(e)
					}
				}
			}
		}
		_, e = c.DeleteObject(ctx, &s3.DeleteObjectInput{Bucket: aws.String(bucket), Key: aws.String("recovery.bin")})
		if e != nil {
			t.Error(e)
		}
		_, e = c.DeleteBucket(ctx, &s3.DeleteBucketInput{Bucket: aws.String(bucket)})
		if e != nil {
			t.Error(e)
		} else {
			t.Log("Removed disposable tuned recovery bucket")
		}
	}()
	report := filepath.Join("dist", "aws-upload-engine")
	mustPerf(t, os.MkdirAll(report, 0700))
	manifest, _ := json.Marshal(map[string]string{"bucket": bucket, "region": profile.Region})
	mustPerf(t, os.WriteFile(filepath.Join(report, "recovery-bucket-manifest.json"), manifest, 0600))
	source := filepath.Join(root, "source.bin")
	local, e := os.Create(source)
	mustPerf(t, e)
	_, e = io.CopyN(local, rand.Reader, 1<<30)
	mustPerf(t, e)
	mustPerf(t, local.Sync())
	mustPerf(t, local.Close())

	tid, e := a.QueueLocalUpload(context.Background(), profile.ID, bucket, "recovery.bin", source)
	mustPerf(t, e)
	deadline := time.Now().Add(3 * time.Minute)
	for {
		a.mu.Lock()
		tr := *a.transfers[tid]
		a.mu.Unlock()
		if tr.Done >= 512<<20 && tr.Resumable {
			break
		}
		if tr.Status == "completed" || tr.Status == "failed" || time.Now().After(deadline) {
			t.Fatalf("missed recovery interruption: %+v", tr)
		}
		time.Sleep(5 * time.Millisecond)
	}
	mustPerf(t, a.shutdown(context.Background()))
	previous := a
	a = NewApp()
	a.configFile = previous.configFile
	a.secretGet = previous.secretGet
	a.secretSet = previous.secretSet
	a.secretDelete = previous.secretDelete
	mustPerf(t, a.startup(context.Background()))
	a.profiles = []Profile{profile}
	started := time.Now()
	mustPerf(t, a.RetryTransfer(tid))
	recovered := perfWait(t, a, tid, "completed")
	if recovered.ResumedBytes <= 0 {
		t.Fatal("tuned upload restart did not reuse acknowledged parts")
	}
	retryTime := time.Since(started).Seconds()
	result, e := a.QueueDownloadTo(profile.ID, bucket, "recovery.bin", root, "recovered.bin", true)
	mustPerf(t, e)
	perfWait(t, a, result.ID, "completed")
	if perfHash(t, result.Path, 1<<30) != perfHash(t, source, 1<<30) {
		t.Fatal("tuned restart checksum mismatch")
	}
	recovery, _ := json.Marshal(map[string]any{"resumed_bytes": recovered.ResumedBytes, "retry_seconds": retryTime, "sha256_verified": true})
	mustPerf(t, os.WriteFile(filepath.Join(report, "recovery.json"), recovery, 0600))
	t.Logf("Tuned AWS upload restart reused %d bytes; SHA-256 matched", recovered.ResumedBytes)

}

func TestAWSUploadTuningCleanup(t *testing.T) {
	if os.Getenv("HARBOR_AWS_UPLOAD_CLEANUP") != "1" {
		t.Skip("set HARBOR_AWS_UPLOAD_CLEANUP=1 to verify only recorded upload-test buckets are absent")
	}
	a, _, c := perfAWSConnection(t, t.TempDir())
	defer a.shutdown(context.Background())
	for _, path := range []string{"dist/aws-upload-tuning/tuning-bucket-manifest.json", "dist/aws-upload-engine/bucket-manifest.json", "dist/aws-upload-engine/recovery-bucket-manifest.json"} {
		raw, err := os.ReadFile(path)
		mustPerf(t, err)
		var manifest map[string]string
		mustPerf(t, json.Unmarshal(raw, &manifest))
		bucket := manifest["bucket"]
		if !strings.HasPrefix(bucket, "harbor-tuning-") || validateBucketName(bucket) != nil {
			t.Fatal("not an upload test bucket")
		}
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		_, err = c.HeadBucket(ctx, &s3.HeadBucketInput{Bucket: aws.String(bucket)})
		cancel()
		var response *smithyhttp.ResponseError
		if !errors.As(err, &response) || response.HTTPStatusCode() != 404 {
			t.Fatalf("upload-test bucket absence not established: %v", err)
		}
	}
	t.Log("Verified all three upload-test buckets absent")
}
