package main

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
)

// Opt-in follow-up: benchmarks the production checkpoint engines, then stops
// and reconstructs the app during real AWS uploads and downloads.
func TestAWSOptimizedTransfers(t *testing.T) {
	if os.Getenv("HARBOR_AWS_OPTIMIZED") != "1" {
		t.Skip("set HARBOR_AWS_OPTIMIZED=1 for real AWS verification")
	}
	root := t.TempDir()
	a, profile, client := perfAWSConnection(t, root)
	defer func() { _ = a.shutdown(context.Background()) }()
	bucket := "harbor-perf-" + id()
	_, err := a.CreateBucket(profile.ID, bucket)
	mustPerf(t, err)
	keys := map[string]bool{"1GiB.bin": true, "5GiB.bin": true, "recovery.bin": true}
	report := filepath.Join("dist", "aws-optimized")
	mustPerf(t, os.MkdirAll(report, 0700))
	manifest, _ := json.Marshal(map[string]string{"bucket": bucket, "region": profile.Region})
	mustPerf(t, os.WriteFile(filepath.Join(report, "bucket-manifest.json"), manifest, 0600))
	defer func() {
		_ = a.shutdown(context.Background())
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		parts, e := client.ListMultipartUploads(ctx, &s3.ListMultipartUploadsInput{Bucket: aws.String(bucket)})
		if e != nil {
			t.Error(e)
		} else {
			for _, u := range parts.Uploads {
				if keys[aws.ToString(u.Key)] {
					_, e = client.AbortMultipartUpload(ctx, &s3.AbortMultipartUploadInput{Bucket: aws.String(bucket), Key: u.Key, UploadId: u.UploadId})
					if e != nil {
						t.Error(e)
					}
				}
			}
		}
		for key := range keys {
			_, e = client.DeleteObject(ctx, &s3.DeleteObjectInput{Bucket: aws.String(bucket), Key: aws.String(key)})
			if e != nil {
				t.Error(e)
			}
		}
		_, e = client.DeleteBucket(ctx, &s3.DeleteBucketInput{Bucket: aws.String(bucket)})
		if e != nil {
			t.Error(e)
		} else {
			t.Log("Removed disposable AWS verification bucket")
		}
	}()
	source := filepath.Join(root, "source.bin")
	f, err := os.Create(source)
	mustPerf(t, err)
	_, err = io.CopyN(f, rand.Reader, 5<<30)
	mustPerf(t, err)
	mustPerf(t, f.Sync())
	mustPerf(t, f.Close())
	output, err := os.Create(filepath.Join(report, "measurements.jsonl"))
	mustPerf(t, err)
	defer output.Close()
	encoder := json.NewEncoder(output)
	digests := map[int64]string{}
	emit := func(r perfRecord) {
		mustPerf(t, encoder.Encode(r))
		mustPerf(t, output.Sync())
		t.Logf("%s %s trial %d: %.2f MiB/s, %.2fs, peak heap %.1f MiB", r.Kind, r.SizeLabel, r.Run, r.MiBPerSecond, r.Seconds, r.PeakHeapMiB)
	}
	for _, size := range []int64{1 << 30, 5 << 30} {
		label := "1GiB"
		trials := 3
		if size == 5<<30 {
			label = "5GiB"
			trials = 1
		}
		digests[size] = perfHash(t, source, size)
		// A size-limited source is staged through Harbor, just as its native picker.
		local := source
		if size == 1<<30 {
			local = filepath.Join(root, "one.bin")
			dest, e := os.Create(local)
			mustPerf(t, e)
			src, e := os.Open(source)
			mustPerf(t, e)
			_, e = io.CopyN(dest, src, size)
			src.Close()
			mustPerf(t, e)
			mustPerf(t, dest.Close())
		}
		for trial := 1; trial <= trials; trial++ {
			runtime.GC()
			meter := startPerfMeter()
			started := time.Now()
			tid, e := a.QueueLocalUpload(context.Background(), profile.ID, bucket, label+".bin", local)
			mustPerf(t, e)
			stage := time.Since(started)
			perfWait(t, a, tid, "completed")
			emit(meter.stop("upload", label, trial, size, time.Since(started), stage))
			runtime.GC()
			meter = startPerfMeter()
			started = time.Now()
			result, e := a.QueueDownloadTo(profile.ID, bucket, label+".bin", root, "download.bin", true)
			mustPerf(t, e)
			perfWait(t, a, result.ID, "completed")
			emit(meter.stop("download", label, trial, size, time.Since(started), 0))
			if perfHash(t, result.Path, size) != digests[size] {
				t.Fatal("optimized checksum mismatch")
			}
			mustPerf(t, os.Remove(result.Path))
		}
	}
	for _, direction := range []string{"upload", "download"} {
		var tid string
		target := filepath.Join(root, "recovered.bin")
		if direction == "upload" {
			tid, err = a.QueueLocalUpload(context.Background(), profile.ID, bucket, "recovery.bin", filepath.Join(root, "one.bin"))
		} else {
			mustPerf(t, os.WriteFile(target, []byte("original destination"), 0600))
			var result SaveResult
			result, err = a.QueueDownloadTo(profile.ID, bucket, "recovery.bin", root, filepath.Base(target), true)
			tid = result.ID
		}
		mustPerf(t, err)
		deadline := time.Now().Add(3 * time.Minute)
		for {
			a.mu.Lock()
			tr := *a.transfers[tid]
			a.mu.Unlock()
			if tr.Done >= 512<<20 && tr.Resumable {
				break
			}
			if tr.Status == "completed" || tr.Status == "failed" || time.Now().After(deadline) {
				t.Fatalf("missed interruption: %+v", tr)
			}
			time.Sleep(5 * time.Millisecond)
		}
		mustPerf(t, a.shutdown(context.Background()))
		if direction == "download" {
			got, e := os.ReadFile(target)
			mustPerf(t, e)
			if string(got) != "original destination" {
				t.Fatal("interruption changed destination")
			}
		}
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
		tr := perfWait(t, a, tid, "completed")
		if tr.ResumedBytes <= 0 {
			t.Fatal("AWS retry did not reuse saved progress")
		}
		mustPerf(t, encoder.Encode(map[string]any{"kind": direction + "-resume", "resumed_bytes": tr.ResumedBytes, "retry_seconds": time.Since(started).Seconds()}))
		mustPerf(t, output.Sync())
		t.Logf("AWS %s restart recovered %d bytes", direction, tr.ResumedBytes)
	}
	if perfHash(t, filepath.Join(root, "recovered.bin"), 1<<30) != digests[1<<30] {
		t.Fatal("AWS recovered data mismatch")
	}
}
