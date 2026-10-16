package main

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	smithyhttp "github.com/aws/smithy-go/transport/http"
)

// Opt-in comparison through the real transfer queue and journal. Both modes
// retain identical part sizes, checksum verification and recovery semantics.
func TestAWSAdaptiveTuning(t *testing.T) {
	if os.Getenv("HARBOR_AWS_ADAPTIVE") != "1" {
		t.Skip("set HARBOR_AWS_ADAPTIVE=1 for real AWS adaptive verification")
	}
	root := t.TempDir()
	a, p, c := perfAWSConnection(t, root)
	bucket := "harbor-tuning-" + id()
	_, err := a.CreateBucket(p.ID, bucket)
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
				_, e = c.AbortMultipartUpload(ctx, &s3.AbortMultipartUploadInput{Bucket: aws.String(bucket), Key: u.Key, UploadId: u.UploadId})
				if e != nil {
					t.Error(e)
				}
			}
		}
		for _, key := range []string{"observe.bin", "auto.bin"} {
			_, e = c.DeleteObject(ctx, &s3.DeleteObjectInput{Bucket: aws.String(bucket), Key: aws.String(key)})
			if e != nil {
				t.Error(e)
			}
		}
		_, e = c.DeleteBucket(ctx, &s3.DeleteBucketInput{Bucket: aws.String(bucket)})
		if e != nil {
			t.Error(e)
		} else {
			_, e = c.HeadBucket(ctx, &s3.HeadBucketInput{Bucket: aws.String(bucket)})
			var response *smithyhttp.ResponseError
			if !errors.As(e, &response) || response.HTTPStatusCode() != 404 {
				t.Errorf("adaptive test bucket absence not established: %v", e)
			} else {
				t.Log("Verified disposable adaptive test bucket absent")
			}
		}
	}()
	report := os.Getenv("HARBOR_ADAPTIVE_REPORT")
	if report == "" {
		report = filepath.Join("dist", "aws-adaptive")
	}
	mustPerf(t, os.MkdirAll(report, 0700))
	manifest, _ := json.Marshal(map[string]string{"bucket": bucket, "region": p.Region})
	mustPerf(t, os.WriteFile(filepath.Join(report, "bucket-manifest.json"), manifest, 0600))
	source := filepath.Join(root, "source.bin")
	f, err := os.Create(source)
	mustPerf(t, err)
	_, err = io.CopyN(f, rand.Reader, 1<<30)
	mustPerf(t, err)
	mustPerf(t, f.Sync())
	mustPerf(t, f.Close())
	expected := perfHash(t, source, 1<<30)
	output, err := os.Create(filepath.Join(report, "measurements.jsonl"))
	mustPerf(t, err)
	defer output.Close()
	encoder := json.NewEncoder(output)
	modes := []string{"observe", "auto"}
	for trial := 0; trial < 3; trial++ {
		for j := range modes {
			mode := modes[(j+trial)%len(modes)]
			mustPerf(t, a.SetTransferTuningMode("fixed"))
			mustPerf(t, a.SetTransferTuningMode(mode))
			runtime.GC()
			meter := startPerfMeter()
			started := time.Now()
			tid, e := a.QueueLocalUpload(context.Background(), p.ID, bucket, mode+".bin", source)
			mustPerf(t, e)
			staging := time.Since(started)
			perfWait(t, a, tid, "completed")
			record := meter.stop(mode+"-upload", "1024MiB", trial+1, 1<<30, time.Since(started), staging)
			mustPerf(t, encoder.Encode(map[string]any{"measurement": record, "scheduler": a.TransferTuningStatus().Scheduler}))
			mustPerf(t, output.Sync())
			t.Logf("%s upload trial %d %.1f MiB/s; target %d", mode, trial+1, record.MiBPerSecond, a.TransferTuningStatus().Scheduler.BulkLimit)
			runtime.GC()
			meter = startPerfMeter()
			started = time.Now()
			result, e := a.QueueDownloadTo(p.ID, bucket, mode+".bin", root, "download.bin", true)
			mustPerf(t, e)
			perfWait(t, a, result.ID, "completed")
			record = meter.stop(mode+"-download", "1024MiB", trial+1, 1<<30, time.Since(started), 0)
			mustPerf(t, encoder.Encode(map[string]any{"measurement": record, "scheduler": a.TransferTuningStatus().Scheduler}))
			mustPerf(t, output.Sync())
			if perfHash(t, result.Path, 1<<30) != expected {
				t.Fatal("adaptive checksum mismatch")
			}
			mustPerf(t, os.Remove(result.Path))
			t.Logf("%s download trial %d %.1f MiB/s; SHA-256 matched; target %d", mode, trial+1, record.MiBPerSecond, a.TransferTuningStatus().Scheduler.BulkLimit)
		}
	}
}

func TestAWSAdaptiveBucketAbsent(t *testing.T) {
	if os.Getenv("HARBOR_AWS_ADAPTIVE_CLEANUP") != "1" {
		t.Skip("opt-in AWS test bucket absence check")
	}
	a, _, c := perfAWSConnection(t, t.TempDir())
	defer a.shutdown(context.Background())
	report := os.Getenv("HARBOR_ADAPTIVE_REPORT")
	if report == "" {
		report = filepath.Join("dist", "aws-adaptive")
	}
	raw, err := os.ReadFile(filepath.Join(report, "bucket-manifest.json"))
	mustPerf(t, err)
	var manifest map[string]string
	mustPerf(t, json.Unmarshal(raw, &manifest))
	bucket := manifest["bucket"]
	if !strings.HasPrefix(bucket, "harbor-tuning-") || validateBucketName(bucket) != nil {
		t.Fatal("not an adaptive test bucket")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	_, err = c.HeadBucket(ctx, &s3.HeadBucketInput{Bucket: aws.String(bucket)})
	var response *smithyhttp.ResponseError
	if !errors.As(err, &response) || response.HTTPStatusCode() != 404 {
		t.Fatalf("adaptive test bucket absence not established: %v", err)
	}
	t.Log("Verified adaptive test bucket absent")
}
