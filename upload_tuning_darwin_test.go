package main

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/feature/s3/manager"
	"github.com/aws/aws-sdk-go-v2/service/s3"
)

// Controlled SDK experiments, not a change to Harbor's production transfer path.
// Compare buffering and multipart settings independently, rotating trial order.
func TestAWSUploadTuning(t *testing.T) {
	if os.Getenv("HARBOR_AWS_TUNING") != "1" {
		t.Skip("set HARBOR_AWS_TUNING=1 for real AWS upload experiments")
	}
	root := t.TempDir()
	a, p, c := perfAWSConnection(t, root)
	defer a.shutdown(context.Background())
	bucket := "harbor-tuning-" + id()
	_, err := a.CreateBucket(p.ID, bucket)
	mustPerf(t, err)
	variants := []struct {
		name        string
		seek        bool
		part        int64
		concurrency int
	}{{"buffered-default", false, 5 << 20, 5}, {"seekable-default", true, 5 << 20, 5}, {"seekable-32MiB-8", true, 32 << 20, 8}}
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		for _, v := range variants {
			if _, e := c.DeleteObject(ctx, &s3.DeleteObjectInput{Bucket: aws.String(bucket), Key: aws.String(v.name)}); e != nil {
				t.Error(e)
			}
		}
		if _, e := c.DeleteBucket(ctx, &s3.DeleteBucketInput{Bucket: aws.String(bucket)}); e != nil {
			t.Error(e)
		}
	}()
	source := filepath.Join(root, "source.bin")
	f, err := os.Create(source)
	mustPerf(t, err)
	_, err = io.CopyN(f, rand.Reader, 1<<30)
	mustPerf(t, err)
	mustPerf(t, f.Sync())
	f.Close()
	expected := perfHash(t, source, 1<<30)
	report := os.Getenv("HARBOR_PERFORMANCE_REPORT")
	if report == "" {
		report = filepath.Join("dist", "aws-performance")
	}
	mustPerf(t, os.MkdirAll(report, 0700))
	manifest, _ := json.MarshalIndent(map[string]string{"bucket": bucket, "region": p.Region, "connection": p.Name}, "", "  ")
	mustPerf(t, os.WriteFile(filepath.Join(report, "tuning-bucket-manifest.json"), manifest, 0600))
	out, err := os.Create(filepath.Join(report, "tuning.jsonl"))
	mustPerf(t, err)
	defer out.Close()
	enc := json.NewEncoder(out)
	if os.Getenv("HARBOR_TUNING_DOWNLOAD_ONLY") == "1" {
		file, e := os.Open(source)
		mustPerf(t, e)
		_, e = manager.NewUploader(c).Upload(context.Background(), &s3.PutObjectInput{Bucket: aws.String(bucket), Key: aws.String(variants[0].name), Body: file})
		file.Close()
		mustPerf(t, e)
		perfRangeExperiments(t, c, bucket, variants[0].name, root, expected, enc, out)
		t.Log("Controlled download experiments complete; temporary AWS bucket cleaned on exit")
		return
	}
	for trial := 0; trial < 3; trial++ {
		for index := 0; index < len(variants); index++ {
			v := variants[(index+trial)%len(variants)]
			file, e := os.Open(source)
			mustPerf(t, e)
			var body io.Reader = file
			if !v.seek {
				body = &progressReader{Reader: file, update: func(int64) {}}
			}
			runtime.GC()
			meter := startPerfMeter()
			started := time.Now()
			_, e = manager.NewUploader(c, func(u *manager.Uploader) { u.PartSize = v.part; u.Concurrency = v.concurrency }).Upload(context.Background(), &s3.PutObjectInput{Bucket: aws.String(bucket), Key: aws.String(v.name), Body: body})
			record := meter.stop(v.name, "1024MiB", trial+1, 1<<30, time.Since(started), 0)
			file.Close()
			mustPerf(t, e)
			mustPerf(t, enc.Encode(record))
			out.Sync()
			object, e := c.GetObject(context.Background(), &s3.GetObjectInput{Bucket: aws.String(bucket), Key: aws.String(v.name)})
			mustPerf(t, e)
			h := sha256.New()
			n, e := io.Copy(h, object.Body)
			object.Body.Close()
			mustPerf(t, e)
			if n != 1<<30 || hex.EncodeToString(h.Sum(nil)) != expected {
				t.Fatal("experiment checksum mismatch")
			}
			t.Logf("%s run %d: %.2fs, %.1f MiB/s, heap %.1f MiB, allocated %.1f MiB, CPU %.2fs; SHA-256 matched", v.name, trial+1, record.Seconds, record.MiBPerSecond, record.PeakHeapMiB, record.AllocatedMiB, record.CPUSeconds)
		}
	}
	perfRangeExperiments(t, c, bucket, variants[0].name, root, expected, enc, out)
	t.Log("Controlled upload and download experiments complete; temporary AWS bucket cleaned on exit")
}

// Range reads are pinned to the same immutable ETag and checked for exact lengths.
// These are isolated experiments; the shipping download code is unchanged.
func perfRangeExperiments(t *testing.T, c *s3.Client, bucket, key, root, expected string, enc *json.Encoder, out *os.File) {
	t.Helper()
	head, err := c.HeadObject(context.Background(), &s3.HeadObjectInput{Bucket: aws.String(bucket), Key: aws.String(key)})
	mustPerf(t, err)
	size := aws.ToInt64(head.ContentLength)
	workers := []int{1, 4, 8}
	for trial := 0; trial < 3; trial++ {
		for index := 0; index < 3; index++ {
			count := workers[(index+trial)%3]
			path := filepath.Join(root, "range-download.bin")
			file, e := os.Create(path)
			mustPerf(t, e)
			mustPerf(t, file.Truncate(size))
			runtime.GC()
			meter := startPerfMeter()
			started := time.Now()
			var group sync.WaitGroup
			failures := make(chan error, count)
			partSize := int64(5 << 20)
			totalParts := (size + partSize - 1) / partSize
			stride := ((totalParts + int64(count) - 1) / int64(count)) * partSize
			for part := 0; part < count; part++ {
				start := stride * int64(part)
				end := min(start+stride, size) - 1
				group.Add(1)
				go func(start, end int64) {
					defer group.Done()
					input := &s3.GetObjectInput{Bucket: aws.String(bucket), Key: aws.String(key), IfMatch: head.ETag}
					if count > 1 {
						input.Range = aws.String(fmt.Sprintf("bytes=%d-%d", start, end))
					}
					object, e := c.GetObject(context.Background(), input)
					if e != nil {
						failures <- e
						return
					}
					defer object.Body.Close()
					if aws.ToString(object.ETag) != aws.ToString(head.ETag) {
						failures <- fmt.Errorf("object identity changed")
						return
					}
					if count > 1 && aws.ToString(object.ContentRange) != fmt.Sprintf("bytes %d-%d/%d", start, end, size) {
						failures <- fmt.Errorf("invalid content range")
						return
					}
					n, e := io.Copy(io.NewOffsetWriter(file, start), object.Body)
					if e != nil {
						failures <- e
						return
					}
					if n != end-start+1 {
						failures <- fmt.Errorf("range length mismatch")
					}
				}(start, end)
			}
			group.Wait()
			close(failures)
			for e := range failures {
				mustPerf(t, e)
			}
			mustPerf(t, file.Sync())
			mustPerf(t, file.Close())
			record := meter.stop(fmt.Sprintf("download-%d-streams", count), "1024MiB", trial+1, size, time.Since(started), 0)
			mustPerf(t, enc.Encode(record))
			out.Sync()
			if perfHash(t, path, size) != expected {
				t.Fatal("range download SHA-256 differs")
			}
			mustPerf(t, os.Remove(path))
			t.Logf("%s run %d: %.2fs, %.1f MiB/s; SHA-256 matched", record.Kind, trial+1, record.Seconds, record.MiBPerSecond)
		}
	}
}
