package main

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"runtime/pprof"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	smithyhttp "github.com/aws/smithy-go/transport/http"
)

// Explicitly opt-in: uses the named saved AWS connection, creates only its own
// randomly named bucket, and removes only keys that this run created. No user
// profile, credential or transfer journal is modified. Run without -race for timing.
func TestAWSLargeFilePerformance(t *testing.T) {
	if os.Getenv("HARBOR_AWS_PERFORMANCE") != "1" {
		t.Skip("set HARBOR_AWS_PERFORMANCE=1 for real AWS transfer measurements")
	}
	reportDir := os.Getenv("HARBOR_PERFORMANCE_REPORT")
	if reportDir == "" {
		reportDir = filepath.Join("dist", "aws-performance")
	}
	mustPerf(t, os.MkdirAll(reportDir, 0700))
	root := t.TempDir()
	a, selected, client := perfAWSConnection(t, root)
	defer a.shutdown(context.Background())
	var err error
	bucket := "harbor-perf-" + id()
	_, err = a.CreateBucket(selected.ID, bucket)
	mustPerf(t, err)
	t.Log("Created disposable AWS benchmark bucket in", selected.Region)
	manifest, _ := json.MarshalIndent(map[string]string{"bucket": bucket, "region": selected.Region, "connection": selected.Name}, "", "  ")
	mustPerf(t, os.WriteFile(filepath.Join(reportDir, "bucket-manifest.json"), manifest, 0600))
	keys := map[string]bool{}
	defer func() {
		_ = a.shutdown(context.Background())
		cleanup, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		uploads, e := client.ListMultipartUploads(cleanup, &s3.ListMultipartUploadsInput{Bucket: aws.String(bucket)})
		if e == nil {
			for _, u := range uploads.Uploads {
				if keys[aws.ToString(u.Key)] {
					_, e = client.AbortMultipartUpload(cleanup, &s3.AbortMultipartUploadInput{Bucket: aws.String(bucket), Key: u.Key, UploadId: u.UploadId})
					if e != nil {
						t.Error(e)
					}
				}
			}
		}
		for key := range keys {
			if _, e = client.DeleteObject(cleanup, &s3.DeleteObjectInput{Bucket: aws.String(bucket), Key: aws.String(key)}); e != nil {
				t.Error(e)
			}
		}
		if _, e = client.DeleteBucket(cleanup, &s3.DeleteBucketInput{Bucket: aws.String(bucket)}); e != nil {
			t.Errorf("benchmark bucket cleanup incomplete: %v", e)
		} else {
			t.Log("Disposable AWS benchmark bucket cleaned up")
		}
	}()
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	mustPerf(t, err)
	token := id()
	server := httptest.NewUnstartedServer(webHandler(a, listener.Addr().String(), token))
	server.Listener.Close()
	server.Listener = listener
	server.Start()
	defer server.Close()
	source := filepath.Join(root, "random-source.bin")
	f, err := os.Create(source)
	mustPerf(t, err)
	t.Log("Generating 5 GiB of non-compressible random data")
	_, err = io.CopyN(f, rand.Reader, 5<<30)
	mustPerf(t, err)
	mustPerf(t, f.Sync())
	mustPerf(t, f.Close())
	sizes := []int64{100 << 20, 1 << 30, 5 << 30}
	digests := map[int64]string{}
	for _, size := range sizes {
		digests[size] = perfHash(t, source, size)
	}
	output, err := os.Create(filepath.Join(reportDir, "measurements.jsonl"))
	mustPerf(t, err)
	defer output.Close()
	encoder := json.NewEncoder(output)
	emit := func(record perfRecord) {
		mustPerf(t, encoder.Encode(record))
		mustPerf(t, output.Sync())
		t.Logf("%s %s run %d: %.2fs, %.2f MiB/s, staging %.2fs, peak Go heap %.1f MiB, allocated %.1f MiB, CPU %.2fs", record.Kind, record.SizeLabel, record.Run, record.Seconds, record.MiBPerSecond, record.StagingSeconds, record.PeakHeapMiB, record.AllocatedMiB, record.CPUSeconds)
	}
	for _, size := range sizes {
		label := fmt.Sprintf("%dMiB", size>>20)
		key := label + ".bin"
		keys[key] = true
		for trial := 1; trial <= 3; trial++ {
			runtime.GC()
			meter := startPerfMeter()
			started := time.Now()
			var cpu *os.File
			if size == 1<<30 && trial == 1 {
				cpu, err = os.Create(filepath.Join(reportDir, "upload-cpu.pprof"))
				mustPerf(t, err)
				mustPerf(t, pprof.StartCPUProfile(cpu))
			}
			id, stage := perfHTTPUpload(t, server.URL, token, selected.ID, connectionIdentity(selected), bucket, key, source, size)
			tr := perfWait(t, a, id, "completed")
			elapsed := time.Since(started)
			record := meter.stop("upload", label, trial, size, elapsed, stage)
			if cpu != nil {
				pprof.StopCPUProfile()
				cpu.Close()
			}
			if tr.Done != size || tr.Total != size {
				t.Fatal("upload length mismatch")
			}
			emit(record)
			// GET through the ordinary Harbor download path, including sync and publish.
			destination := fmt.Sprintf("download-%s-%d.bin", label, trial)
			runtime.GC()
			meter = startPerfMeter()
			started = time.Now()
			result, e := a.QueueDownloadTo(selected.ID, bucket, key, root, destination, false)
			mustPerf(t, e)
			tr = perfWait(t, a, result.ID, "completed")
			record = meter.stop("download", label, trial, size, time.Since(started), 0)
			emit(record)
			path := filepath.Join(root, destination)
			info, e := os.Stat(path)
			mustPerf(t, e)
			if info.Size() != size || tr.Done != size || tr.Total != size {
				t.Fatal("download length mismatch")
			}
			if perfHash(t, path, size) != digests[size] {
				t.Fatal("download SHA-256 differs")
			}
			mustPerf(t, os.Remove(path))
			t.Log("SHA-256 matched", label, "run", trial)
		}
	}
	// Interrupt a 1 GiB transfer at each milestone, then verify an explicit retry
	// restarts and completes. Polling granularity is recorded; no byte-offset resume
	// is claimed. Existing destination bytes must survive every interrupted download.
	if os.Getenv("HARBOR_PERFORMANCE_INTERRUPTS") != "0" {
		size := int64(1 << 30)
		key := "interrupt.bin"
		keys[key] = true
		for _, fraction := range []float64{.1, .5, .9} {
			transfer, stage := perfHTTPUpload(t, server.URL, token, selected.ID, connectionIdentity(selected), bucket, key, source, size)
			stopped := perfCancelAt(t, a, transfer, int64(float64(size)*fraction))
			started := time.Now()
			mustPerf(t, a.RetryTransfer(transfer))
			perfWait(t, a, transfer, "completed")
			mustPerf(t, encoder.Encode(map[string]any{"kind": "upload-interruption", "requested_fraction": fraction, "observed_bytes": stopped.Done, "size": size, "staging_seconds": stage.Seconds(), "retry_seconds": time.Since(started).Seconds()}))
			output.Sync()
			destination := filepath.Join(root, "interrupt-download.bin")
			mustPerf(t, os.WriteFile(destination, []byte("preserve original"), 0600))
			result, e := a.QueueDownloadTo(selected.ID, bucket, key, root, filepath.Base(destination), true)
			mustPerf(t, e)
			stopped = perfCancelAt(t, a, result.ID, int64(float64(size)*fraction))
			original, e := os.ReadFile(destination)
			mustPerf(t, e)
			if string(original) != "preserve original" {
				t.Fatal("interruption changed destination")
			}
			started = time.Now()
			mustPerf(t, a.RetryTransfer(result.ID))
			perfWait(t, a, result.ID, "completed")
			retrySeconds := time.Since(started).Seconds()
			info, e := os.Stat(destination)
			mustPerf(t, e)
			if info.Size() != size {
				t.Fatal("retry download length mismatch")
			}
			if perfHash(t, destination, size) != digests[size] {
				t.Fatal("retry checksum mismatch")
			}
			os.Remove(destination)
			mustPerf(t, encoder.Encode(map[string]any{"kind": "download-interruption", "requested_fraction": fraction, "observed_bytes": stopped.Done, "size": size, "retry_seconds": retrySeconds}))
			output.Sync()
			t.Logf("Interruption/retry passed near %.0f%% for upload and download", 100*fraction)
		}
	}
	heap, err := os.Create(filepath.Join(reportDir, "heap.pprof"))
	mustPerf(t, err)
	mustPerf(t, pprof.WriteHeapProfile(heap))
	heap.Close()
	t.Log("All checksums and interruption tests passed; measurements saved")
}

type perfRecord struct {
	Kind           string  `json:"kind"`
	SizeLabel      string  `json:"size_label"`
	Run            int     `json:"run"`
	Bytes          int64   `json:"bytes"`
	Seconds        float64 `json:"seconds"`
	MiBPerSecond   float64 `json:"mib_per_second"`
	StagingSeconds float64 `json:"staging_seconds"`
	PeakHeapMiB    float64 `json:"peak_heap_mib"`
	AllocatedMiB   float64 `json:"allocated_mib"`
	CPUSeconds     float64 `json:"cpu_seconds"`
}
type perfMeter struct {
	start  runtime.MemStats
	usage  syscall.Rusage
	peak   uint64
	stopCh chan struct{}
	done   chan struct{}
}

func startPerfMeter() *perfMeter {
	m := &perfMeter{stopCh: make(chan struct{}), done: make(chan struct{})}
	runtime.ReadMemStats(&m.start)
	syscall.Getrusage(syscall.RUSAGE_SELF, &m.usage)
	m.peak = m.start.HeapAlloc
	go func() {
		defer close(m.done)
		ticker := time.NewTicker(100 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-m.stopCh:
				return
			case <-ticker.C:
				var s runtime.MemStats
				runtime.ReadMemStats(&s)
				if s.HeapAlloc > m.peak {
					m.peak = s.HeapAlloc
				}
			}
		}
	}()
	return m
}
func (m *perfMeter) stop(kind, label string, run int, size int64, elapsed, stage time.Duration) perfRecord {
	close(m.stopCh)
	<-m.done
	var end runtime.MemStats
	runtime.ReadMemStats(&end)
	if end.HeapAlloc > m.peak {
		m.peak = end.HeapAlloc
	}
	var u syscall.Rusage
	syscall.Getrusage(syscall.RUSAGE_SELF, &u)
	seconds := func(u syscall.Rusage) float64 {
		return float64(u.Utime.Sec+u.Stime.Sec) + float64(u.Utime.Usec+u.Stime.Usec)/1e6
	}
	return perfRecord{kind, label, run, size, elapsed.Seconds(), float64(size) / (1 << 20) / elapsed.Seconds(), stage.Seconds(), float64(m.peak) / (1 << 20), float64(end.TotalAlloc-m.start.TotalAlloc) / (1 << 20), seconds(u) - seconds(m.usage)}
}
func perfHTTPUpload(t *testing.T, base, token, profile, identity, bucket, key, path string, size int64) (string, time.Duration) {
	t.Helper()
	f, e := os.Open(path)
	mustPerf(t, e)
	defer f.Close()
	query := url.Values{"profile": {profile}, "identity": {identity}, "bucket": {bucket}, "key": {key}}
	req, e := http.NewRequest("POST", base+"/api/upload?"+query.Encode(), io.LimitReader(f, size))
	mustPerf(t, e)
	req.ContentLength = size
	req.Header.Set("Authorization", "Bearer "+token)
	started := time.Now()
	response, e := http.DefaultClient.Do(req)
	mustPerf(t, e)
	defer response.Body.Close()
	if response.StatusCode != 200 {
		body, _ := io.ReadAll(io.LimitReader(response.Body, 4096))
		t.Fatalf("staging: %s %s", response.Status, body)
	}
	var transfer string
	mustPerf(t, json.NewDecoder(response.Body).Decode(&transfer))
	return transfer, time.Since(started)
}
func perfWait(t *testing.T, a *App, id, status string) Transfer {
	t.Helper()
	deadline := time.Now().Add(30 * time.Minute)
	for time.Now().Before(deadline) {
		a.mu.Lock()
		tr := *a.transfers[id]
		a.mu.Unlock()
		if tr.Status == status {
			return tr
		}
		if tr.Status == "failed" || tr.Status == "cancelled" || tr.Status == "completed" {
			t.Fatalf("unexpected transfer status %s: %s", tr.Status, tr.Error)
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatal("transfer timeout")
	return Transfer{}
}
func perfCancelAt(t *testing.T, a *App, id string, threshold int64) Transfer {
	t.Helper()
	deadline := time.Now().Add(30 * time.Minute)
	for time.Now().Before(deadline) {
		a.mu.Lock()
		tr := *a.transfers[id]
		a.mu.Unlock()
		if tr.Status == "completed" || tr.Status == "failed" {
			t.Fatalf("transfer ended before interruption at %d: %s", threshold, tr.Status)
		}
		if tr.Done >= threshold {
			a.CancelTransfer(id)
			return perfWait(t, a, id, "cancelled")
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("interruption timeout")
	return Transfer{}
}
func perfHash(t *testing.T, path string, size int64) string {
	t.Helper()
	f, e := os.Open(path)
	mustPerf(t, e)
	defer f.Close()
	h := sha256.New()
	n, e := io.CopyN(h, f, size)
	mustPerf(t, e)
	if n != size {
		t.Fatal("hash length mismatch")
	}
	return hex.EncodeToString(h.Sum(nil))
}
func mustPerf(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func perfAWSConnection(t *testing.T, root string) (*App, Profile, *s3.Client) {
	t.Helper()
	a := NewApp()
	a.secretSet = func(string, string) error { return errors.New("performance diagnostics cannot modify credentials") }
	a.secretDelete = func(string) error { return errors.New("performance diagnostics cannot delete credentials") }
	a.configFile = filepath.Join(root, "profiles.db")
	mustPerf(t, a.startup(context.Background()))
	home, err := os.UserHomeDir()
	mustPerf(t, err)
	storeURL := url.URL{Scheme: "file", Path: filepath.Join(home, ".harbor", "profiles.db"), RawQuery: "mode=ro"}
	db, err := sql.Open("sqlite", storeURL.String())
	mustPerf(t, err)
	name := os.Getenv("HARBOR_PERFORMANCE_CONNECTION")
	if name == "" {
		name = "aws-s3"
	}
	rows, err := db.Query("SELECT p.metadata, COALESCE(c.credential_id,p.id) FROM profiles p LEFT JOIN profile_credentials c ON c.profile_id=p.id")
	mustPerf(t, err)
	var selected Profile
	for rows.Next() {
		var raw, ref string
		mustPerf(t, rows.Scan(&raw, &ref))
		var p Profile
		mustPerf(t, json.Unmarshal([]byte(raw), &p))
		if p.Name == name {
			p.credentialID = ref
			selected = p
		}
	}
	mustPerf(t, rows.Err())
	rows.Close()
	db.Close()
	if selected.ID == "" || selected.Endpoint != "" {
		t.Fatal("select a saved native AWS S3 connection")
	}
	a.profiles = []Profile{selected}
	client, err := a.client(context.Background(), selected.ID)
	mustPerf(t, err)

	return a, selected, client
}

// Independently check successful cleanup using only the recorded test bucket names.
func TestAWSPerformanceCleanup(t *testing.T) {
	if os.Getenv("HARBOR_AWS_CLEANUP_CHECK") != "1" {
		t.Skip("set HARBOR_AWS_CLEANUP_CHECK=1 to verify recorded benchmark buckets are absent")
	}
	root := t.TempDir()
	a, _, c := perfAWSConnection(t, root)
	defer a.shutdown(context.Background())
	report := os.Getenv("HARBOR_PERFORMANCE_REPORT")
	if report == "" {
		report = filepath.Join("dist", "aws-performance")
	}
	for _, name := range []string{"bucket-manifest.json", "tuning-bucket-manifest.json"} {
		data, e := os.ReadFile(filepath.Join(report, name))
		mustPerf(t, e)
		var manifest map[string]string
		mustPerf(t, json.Unmarshal(data, &manifest))
		bucket := manifest["bucket"]
		if validateBucketName(bucket) != nil || (!strings.HasPrefix(bucket, "harbor-perf-") && !strings.HasPrefix(bucket, "harbor-tuning-")) {
			t.Fatal("manifest is not a benchmark bucket")
		}
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		_, e = c.HeadBucket(ctx, &s3.HeadBucketInput{Bucket: aws.String(bucket)})
		cancel()
		var response *smithyhttp.ResponseError
		if !errors.As(e, &response) || response.HTTPStatusCode() != 404 {
			t.Fatalf("benchmark bucket absence not verified: %v", e)
		}
		t.Log("Verified temporary bucket absent for", name)
	}
}
