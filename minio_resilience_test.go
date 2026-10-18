package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
)

// Opt-in real MinIO tests. The executable is supplied by the caller; every
// server, bucket, credential, database, and file belongs to this test run.
// Fault injection changes only the transport; MinIO implements all S3 behavior.
type minioResilience struct {
	t                     *testing.T
	binary, root, address string
	process               *exec.Cmd
	proxy                 *httptest.Server
	faultMu               sync.Mutex
	fault                 string
	release               chan struct{}
	reached               chan struct{}
	hits                  atomic.Int32
	a                     *App
	profile               Profile
	client                *s3.Client
	bucket, source, ca    string
	keys                  []string
	payload               []byte
}

func unusedMinIOAddress(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := l.Addr().String()
	l.Close()
	return address
}

func newMinIOResilience(t *testing.T) *minioResilience {
	t.Helper()
	binary := os.Getenv("HARBOR_MINIO_BINARY")
	if binary == "" {
		t.Skip("set HARBOR_MINIO_BINARY to a local MinIO executable")
	}
	t.Setenv("AWS_MAX_ATTEMPTS", "3")
	f := &minioResilience{t: t, binary: binary, root: t.TempDir(), address: unusedMinIOAddress(t)}
	t.Cleanup(func() {
		f.setFault("")
		f.stopMinIO()
		if f.proxy != nil {
			f.proxy.Close()
		}
	})
	f.startMinIO()
	target, _ := url.Parse("http://" + f.address)
	proxy := httputil.NewSingleHostReverseProxy(target)
	proxy.ErrorHandler = func(w http.ResponseWriter, r *http.Request, err error) {
		http.Error(w, "injected or upstream interruption", 502)
	}
	proxy.ModifyResponse = func(r *http.Response) error {
		fault, release, reached := f.getFault()
		if fault == "lose-completion" && r.Request.Method == "POST" && r.Request.URL.Query().Get("uploadId") != "" && r.StatusCode == 200 {
			return errors.New("injected lost completion response")
		}
		if fault == "hold-ranges" && r.Request.Method == "GET" && r.StatusCode == 206 && !strings.HasPrefix(r.Header.Get("Content-Range"), "bytes 0-") {
			r.Body = &minioFaultBody{ReadCloser: r.Body, remaining: 256 << 10, hold: true, release: release, reached: reached, ctx: r.Request.Context(), hits: &f.hits}
		}
		if r.Request.Method == "GET" && r.Request.URL.Path == "/"+f.bucket+"/download.bin" && r.StatusCode == 200 {
			if fault == "truncate-download" || fault == "hold-download" {
				r.Body = &minioFaultBody{ReadCloser: r.Body, remaining: 256 << 10, hold: fault == "hold-download", release: release, reached: reached, ctx: r.Request.Context(), hits: &f.hits}
			}
		}
		return nil
	}
	f.proxy = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fault, release, reached := f.getFault()
		if fault == "offline" {
			disconnectMinIO(w)
			return
		}
		part := r.Method == "PUT" && r.URL.Query().Get("partNumber") != ""
		if part && r.URL.Query().Get("partNumber") == "2" {
			if fault == "drop-part-always" || fault == "drop-part-once" && f.hits.CompareAndSwap(0, 1) {
				_, _ = io.CopyN(io.Discard, r.Body, 128<<10)
				disconnectMinIO(w)
				return
			}
			if fault == "hold-upload" {
				_, _ = io.CopyN(io.Discard, r.Body, 128<<10)
				select {
				case reached <- struct{}{}:
				default:
				}
				select {
				case <-release:
				case <-r.Context().Done():
				}
				disconnectMinIO(w)
				return
			}
		}
		proxy.ServeHTTP(w, r)
	}))
	f.ca = filepath.Join(f.root, "ca.pem")
	if err := os.WriteFile(f.ca, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: f.proxy.Certificate().Raw}), 0600); err != nil {
		t.Fatal(err)
	}
	vault := &journalVault{values: map[string]string{}}
	f.a = journalFixture(t, filepath.Join(f.root, "profiles.db"), vault)
	var err error
	f.profile, err = f.a.SaveProfile(Profile{Name: "Disposable MinIO resilience", Region: "us-east-1", Endpoint: f.proxy.URL, CAFile: f.ca, PathStyle: true}, Secret{AccessKey: "harbor-resilience", SecretKey: "synthetic-resilience-secret"})
	if err != nil {
		t.Fatal(err)
	}
	f.client, err = f.a.client(context.Background(), f.profile.ID)
	if err != nil {
		t.Fatal(err)
	}
	f.bucket = "harbor-resilience-" + id()[:12]
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if _, err = f.client.CreateBucket(ctx, &s3.CreateBucketInput{Bucket: aws.String(f.bucket)}); err != nil {
		t.Fatal(err)
	}
	// More than three 5 MiB parts, with distinct bytes throughout.
	f.payload = make([]byte, 17<<20)
	for i := range f.payload {
		f.payload[i] = byte((i*31 + i/7919) % 251)
	}
	f.source = filepath.Join(f.root, "source.bin")
	if err = os.WriteFile(f.source, f.payload, 0600); err != nil {
		t.Fatal(err)
	}
	return f
}

func (f *minioResilience) startMinIO() {
	f.t.Helper()
	log, err := os.OpenFile(filepath.Join(f.root, "minio.log"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		f.t.Fatal(err)
	}
	cmd := exec.Command(f.binary, "server", "--quiet", "--address", f.address, filepath.Join(f.root, "data"))
	cmd.Env = append(os.Environ(), "MINIO_ROOT_USER=harbor-resilience", "MINIO_ROOT_PASSWORD=synthetic-resilience-secret", "MINIO_BROWSER=off", "MINIO_UPDATE=off")
	cmd.Stdout, cmd.Stderr = log, log
	if err = cmd.Start(); err != nil {
		log.Close()
		f.t.Fatal(err)
	}
	log.Close()
	f.process = cmd
	client := &http.Client{Timeout: time.Second}
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		r, err := client.Get("http://" + f.address + "/minio/health/ready")
		if err == nil {
			r.Body.Close()
			if r.StatusCode == 200 {
				return
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	f.t.Fatal("isolated MinIO did not become ready")
}
func (f *minioResilience) stopMinIO() {
	if f.process != nil {
		_ = f.process.Process.Kill()
		_ = f.process.Wait()
		f.process = nil
	}
}
func (f *minioResilience) setFault(fault string) {
	f.faultMu.Lock()
	defer f.faultMu.Unlock()
	if f.release != nil {
		close(f.release)
	}
	f.fault = fault
	f.release, f.reached = make(chan struct{}), make(chan struct{}, 1)
	f.hits.Store(0)
}
func (f *minioResilience) getFault() (string, chan struct{}, chan struct{}) {
	f.faultMu.Lock()
	defer f.faultMu.Unlock()
	return f.fault, f.release, f.reached
}
func (f *minioResilience) awaitFault() {
	f.t.Helper()
	_, _, reached := f.getFault()
	select {
	case <-reached:
	case <-time.After(15 * time.Second):
		f.t.Fatal("transfer never reached injected interruption")
	}
}
func disconnectMinIO(w http.ResponseWriter) {
	c, _, err := w.(http.Hijacker).Hijack()
	if err == nil {
		c.Close()
	}
}

type minioFaultBody struct {
	io.ReadCloser
	remaining        int64
	hold             bool
	release, reached chan struct{}
	ctx              context.Context
	hits             *atomic.Int32
}

func (b *minioFaultBody) Read(p []byte) (int, error) {
	if b.remaining <= 0 {
		b.hits.Add(1)
		select {
		case b.reached <- struct{}{}:
		default:
		}
		if b.hold {
			select {
			case <-b.release:
			case <-b.ctx.Done():
			}
		}
		return 0, io.ErrUnexpectedEOF
	}
	if int64(len(p)) > b.remaining {
		p = p[:b.remaining]
	}
	n, err := b.ReadCloser.Read(p)
	b.remaining -= int64(n)
	return n, err
}
func minioWait(t *testing.T, a *App, transferID string) Transfer {
	t.Helper()
	deadline := time.Now().Add(45 * time.Second)
	for time.Now().Before(deadline) {
		a.mu.Lock()
		tr := *a.transfers[transferID]
		active := tr.cancel != nil
		a.mu.Unlock()
		if !active && (tr.Status == "completed" || tr.Status == "failed" || tr.Status == "cancelled") {
			return tr
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("transfer did not settle after interruption")
	return Transfer{}
}
func (f *minioResilience) upload(key string) string {
	f.t.Helper()
	f.keys = append(f.keys, key)
	tid, err := f.a.QueueLocalUpload(context.Background(), f.profile.ID, f.bucket, key, f.source)
	if err != nil {
		f.t.Fatal(err)
	}
	return tid
}
func (f *minioResilience) verifyObject(key string) {
	f.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	v, err := f.client.GetObject(ctx, &s3.GetObjectInput{Bucket: aws.String(f.bucket), Key: aws.String(key)})
	if err != nil {
		f.t.Fatal(err)
	}
	defer v.Body.Close()
	h := sha256.New()
	n, err := io.Copy(h, v.Body)
	want := sha256.Sum256(f.payload)
	if err != nil || n != int64(len(f.payload)) || !bytes.Equal(h.Sum(nil), want[:]) {
		f.t.Fatalf("MinIO object integrity mismatch: bytes=%d err=%v", n, err)
	}
}
func (f *minioResilience) multipartCount() int {
	f.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	// MinIO's unprefixed listing uses a volatile cache and can omit uploads
	// after a server restart. Exact object prefixes inspect persisted uploads.
	count := 0
	for _, key := range f.keys {
		out, err := f.client.ListMultipartUploads(ctx, &s3.ListMultipartUploadsInput{Bucket: aws.String(f.bucket), Prefix: aws.String(key)})
		if err != nil {
			f.t.Fatal(err)
		}
		count += len(out.Uploads)
	}
	return count
}
func (f *minioResilience) assertNoMultipart() {
	f.t.Helper()
	if n := f.multipartCount(); n != 0 {
		f.t.Fatalf("%d incomplete uploads remain in real MinIO", n)
	}
}

func (f *minioResilience) awaitStoredPart() {
	f.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	for ctx.Err() == nil {
		out, err := f.client.ListMultipartUploads(ctx, &s3.ListMultipartUploadsInput{Bucket: aws.String(f.bucket), Prefix: aws.String(f.keys[len(f.keys)-1])})
		if err != nil {
			f.t.Fatal(err)
		}
		if len(out.Uploads) == 1 {
			upload := out.Uploads[0]
			parts, err := f.client.ListParts(ctx, &s3.ListPartsInput{Bucket: aws.String(f.bucket), Key: upload.Key, UploadId: upload.UploadId})
			if err != nil {
				f.t.Fatal(err)
			}
			if len(parts.Parts) > 0 {
				return
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	f.t.Fatal("MinIO never stored a complete part before interruption")
}
func (f *minioResilience) seedDownload() {
	f.t.Helper()
	tid := f.upload("download.bin")
	if tr := minioWait(f.t, f.a, tid); tr.Status != "completed" {
		f.t.Fatal(tr.Error)
	}
}
func (f *minioResilience) download() (SaveResult, []byte) {
	f.t.Helper()
	original := []byte("existing destination must survive interruption")
	if err := os.WriteFile(filepath.Join(f.root, "saved.bin"), original, 0600); err != nil {
		f.t.Fatal(err)
	}
	result, err := f.a.QueueDownloadTo(f.profile.ID, f.bucket, "download.bin", f.root, "saved.bin", true)
	if err != nil {
		f.t.Fatal(err)
	}
	return result, original
}
func assertMinIOFile(t *testing.T, path string, want []byte) {
	t.Helper()
	got, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(got, want) {
		t.Fatalf("file content mismatch: %s (%v)", path, err)
	}
}
func assertNoMinIOPartials(t *testing.T, root string) {
	t.Helper()
	paths, err := filepath.Glob(filepath.Join(root, ".harbor-download-*"))
	if err != nil || len(paths) != 0 {
		t.Fatalf("partial downloads remain: %v (%v)", paths, err)
	}
}

func TestMinIOResilience(t *testing.T) {
	t.Run("transient_part_disconnect_retries_automatically", func(t *testing.T) {
		f := newMinIOResilience(t)
		f.setFault("drop-part-once")
		tid := f.upload("transient.bin")
		tr := minioWait(t, f.a, tid)
		if tr.Status != "completed" || f.hits.Load() != 1 {
			t.Fatalf("automatic retry failed: %+v; injected drops=%d", tr, f.hits.Load())
		}
		f.verifyObject("transient.bin")
		f.assertNoMultipart()
		assertMinIOFile(t, f.source, f.payload)
		if _, err := os.Stat(tr.Path); !os.IsNotExist(err) {
			t.Fatal("successful upload retained staging", err)
		}
		t.Log("one real multipart connection dropped; SDK retried automatically; 17 MiB SHA-256 matched; no incomplete uploads")
	})
	t.Run("cancel_upload_preserves_source_and_retries", func(t *testing.T) {
		f := newMinIOResilience(t)
		f.setFault("hold-upload")
		tid := f.upload("cancel.bin")
		f.awaitFault()
		if f.multipartCount() != 1 {
			t.Fatal("real multipart upload not established before cancellation")
		}
		f.a.CancelTransfer(tid)
		f.setFault("")
		tr := minioWait(t, f.a, tid)
		if tr.Status != "cancelled" {
			t.Fatalf("cancel failed: %+v", tr)
		}
		f.assertNoMultipart()
		assertMinIOFile(t, f.source, f.payload)
		assertMinIOFile(t, tr.Path, f.payload)
		if err := f.a.RetryTransfer(tid); err != nil {
			t.Fatal(err)
		}
		if tr = minioWait(t, f.a, tid); tr.Status != "completed" {
			t.Fatal(tr.Error)
		}
		f.verifyObject("cancel.bin")
		f.assertNoMultipart()
		t.Log("active upload cancelled; MinIO parts aborted; source and staging preserved; manual retry matched SHA-256")
	})
	t.Run("minio_process_outage_retains_cleanup_and_recovers", func(t *testing.T) {
		f := newMinIOResilience(t)
		f.setFault("hold-upload")
		tid := f.upload("outage.bin")
		f.awaitFault()
		if f.multipartCount() != 1 {
			t.Fatal("no real multipart upload before outage")
		}
		f.awaitStoredPart()
		f.stopMinIO()
		f.setFault("")
		tr := minioWait(t, f.a, tid)
		if tr.Status != "failed" || !strings.Contains(tr.Error, "cleanup failed") {
			t.Fatalf("outage did not report pending cleanup: %+v", tr)
		}
		assertMinIOFile(t, tr.Path, f.payload)
		f.startMinIO()
		if f.multipartCount() != 1 {
			t.Fatal("persisted multipart upload missing after MinIO restart")
		}
		if err := f.a.RetryTransfer(tid); err != nil {
			t.Fatal(err)
		}
		if tr = minioWait(t, f.a, tid); tr.Status != "completed" || tr.CleanupError != "" {
			t.Fatalf("outage retry failed: %+v", tr)
		}
		f.verifyObject("outage.bin")
		f.assertNoMultipart()
		t.Log("MinIO was killed mid-upload; Harbor failed with cleanup warning; restart/retry settled cleanup and uploaded exact content")
	})
	t.Run("network_outage_retains_remote_parts_until_retry", func(t *testing.T) {
		f := newMinIOResilience(t)
		f.setFault("hold-upload")
		tid := f.upload("network.bin")
		f.awaitFault()
		f.awaitStoredPart()
		f.setFault("offline")
		tr := minioWait(t, f.a, tid)
		if tr.Status != "failed" || !strings.Contains(tr.Error, "cleanup failed") {
			t.Fatalf("network outage not reported with cleanup warning: %+v", tr)
		}
		assertMinIOFile(t, tr.Path, f.payload)
		f.setFault("")
		if f.multipartCount() != 1 {
			t.Fatal("network outage did not leave the real multipart upload pending")
		}
		if err := f.a.RetryTransfer(tid); err != nil {
			t.Fatal(err)
		}
		if tr = minioWait(t, f.a, tid); tr.Status != "completed" || tr.CleanupError != "" {
			t.Fatalf("network recovery failed: %+v", tr)
		}
		f.verifyObject("network.bin")
		f.assertNoMultipart()
		t.Log("network unavailable during transfer and abort; old multipart remained in MinIO; retry cleaned it and uploaded exact bytes")
	})
	t.Run("truncated_download_preserves_existing_destination", func(t *testing.T) {
		f := newMinIOResilience(t)
		f.seedDownload()
		f.setFault("truncate-download")
		result, original := f.download()
		tr := minioWait(t, f.a, result.ID)
		if tr.Status != "failed" || tr.Error == "" || f.hits.Load() == 0 {
			t.Fatalf("truncated download not reported: %+v", tr)
		}
		assertMinIOFile(t, result.Path, original)
		assertNoMinIOPartials(t, f.root)
		f.setFault("")
		if err := f.a.RetryTransfer(result.ID); err != nil {
			t.Fatal(err)
		}
		if tr = minioWait(t, f.a, result.ID); tr.Status != "completed" {
			t.Fatal(tr.Error)
		}
		assertMinIOFile(t, result.Path, f.payload)
		assertNoMinIOPartials(t, f.root)
		t.Log("real MinIO response cut after 256 KiB; existing file untouched; partial removed; retry produced exact file")
	})
	t.Run("cancel_download_preserves_existing_destination", func(t *testing.T) {
		f := newMinIOResilience(t)
		f.seedDownload()
		f.setFault("hold-download")
		result, original := f.download()
		f.awaitFault()
		f.a.CancelTransfer(result.ID)
		f.setFault("")
		tr := minioWait(t, f.a, result.ID)
		if tr.Status != "cancelled" {
			t.Fatalf("download not cancelled: %+v", tr)
		}
		assertMinIOFile(t, result.Path, original)
		assertNoMinIOPartials(t, f.root)
		if err := f.a.RetryTransfer(result.ID); err != nil {
			t.Fatal(err)
		}
		if tr = minioWait(t, f.a, result.ID); tr.Status != "completed" {
			t.Fatal(tr.Error)
		}
		assertMinIOFile(t, result.Path, f.payload)
		t.Log("active download cancelled; old destination protected; retry completed with exact bytes")
	})
}

// A separate OS process uses Harbor's real server, locking, transfer engine,
// journal and shutdown path. Only credential access is replaced with synthetic
// fixture credentials, so abrupt-kill tests never access the user's vault.
func TestMinIOResilienceWorker(t *testing.T) {
	if os.Getenv("HARBOR_MINIO_WORKER") != "1" {
		t.Skip("internal isolated subprocess")
	}
	lock, err := acquireStoreLock(os.Getenv("HARBOR_MINIO_WORKER_DB"))
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	a := NewApp()
	a.configFile = lock.path
	a.secretGet = func(string) (string, error) {
		return `{"AccessKey":"harbor-resilience","SecretKey":"synthetic-resilience-secret"}`, nil
	}
	a.secretDelete = func(string) error { return nil }
	if err = a.startup(context.Background()); err != nil {
		t.Fatal(err)
	}
	l, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	server := &http.Server{Handler: webHandler(a, l.Addr().String(), "minio-resilience-fixture-token"), ReadHeaderTimeout: 5 * time.Second}
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(stop)
	fmt.Println("MINIO_WORKER_URL=http://" + l.Addr().String())
	if err = serveUntilStopped(a, server, l, stop); err != nil {
		t.Fatal(err)
	}
}

type minioWorker struct {
	cmd     *exec.Cmd
	url     string
	logPath string
}

func startMinIOWorker(t *testing.T, f *minioResilience) *minioWorker {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=^TestMinIOResilienceWorker$", "-test.v")
	cmd.Env = append(os.Environ(), "HARBOR_MINIO_WORKER=1", "HARBOR_MINIO_WORKER_DB="+f.a.configFile)
	output, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	logPath := filepath.Join(f.root, "worker-"+id()+".log")
	log, err := os.Create(logPath)
	if err != nil {
		t.Fatal(err)
	}
	cmd.Stderr = log
	if err = cmd.Start(); err != nil {
		log.Close()
		t.Fatal(err)
	}
	log.Close()
	worker := &minioWorker{cmd: cmd, logPath: logPath}
	t.Cleanup(func() {
		if cmd.ProcessState == nil {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
		}
	})
	ready := make(chan string, 1)
	go func() {
		scanner := bufio.NewScanner(output)
		for scanner.Scan() {
			if line := scanner.Text(); strings.HasPrefix(line, "MINIO_WORKER_URL=") {
				ready <- strings.TrimPrefix(line, "MINIO_WORKER_URL=")
			}
		}
	}()
	select {
	case worker.url = <-ready:
	case <-time.After(20 * time.Second):
		t.Fatal("Harbor worker startup timed out; see", logPath)
	}
	return worker
}
func (w *minioWorker) api(t *testing.T, route string, input, output any) {
	t.Helper()
	raw, err := json.Marshal(input)
	if err != nil {
		t.Fatal(err)
	}
	switch route {
	case "upload-local", "download", "delete-file":
		var fields map[string]json.RawMessage
		if err = json.Unmarshal(raw, &fields); err != nil {
			t.Fatal(err)
		}
		if len(fields["ConnectionIdentity"]) == 0 {
			var profileID string
			if err = json.Unmarshal(fields["ProfileID"], &profileID); err != nil {
				t.Fatal(err)
			}
			var profiles []Profile
			w.api(t, "profiles", map[string]string{}, &profiles)
			for _, p := range profiles {
				if p.ID == profileID {
					fields["ConnectionIdentity"], err = json.Marshal(p.Identity)
					break
				}
			}
			if err != nil || len(fields["ConnectionIdentity"]) == 0 {
				t.Fatal("reviewed connection identity unavailable", err)
			}
			raw, err = json.Marshal(fields)
			if err != nil {
				t.Fatal(err)
			}
		}
	}
	req, err := http.NewRequest("POST", w.url+"/api/"+route, bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer minio-resilience-fixture-token")
	client := &http.Client{Timeout: 15 * time.Second}
	res, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.StatusCode != 200 {
		body, _ := io.ReadAll(res.Body)
		t.Fatalf("%s: %s %s", route, res.Status, body)
	}
	if output != nil {
		if err = json.NewDecoder(res.Body).Decode(output); err != nil {
			t.Fatal(err)
		}
	}
}
func (w *minioWorker) wait(t *testing.T, tid string) Transfer {
	t.Helper()
	deadline := time.Now().Add(45 * time.Second)
	for time.Now().Before(deadline) {
		var transfers []Transfer
		w.api(t, "transfers", map[string]string{}, &transfers)
		for _, tr := range transfers {
			if tr.ID == tid && (tr.Status == "completed" || tr.Status == "failed" || tr.Status == "cancelled") {
				return tr
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("worker transfer timed out")
	return Transfer{}
}
func TestMinIOResilienceProcessRecovery(t *testing.T) {
	for _, direction := range []string{"upload", "download"} {
		stopKinds := []string{"kill", "graceful"}
		if direction == "upload" {
			stopKinds = append(stopKinds, "kill_offline_reopen")
		}
		for _, stopKind := range stopKinds {
			t.Run(direction+"_"+stopKind, func(t *testing.T) {
				f := newMinIOResilience(t)
				if direction == "download" {
					f.seedDownload()
				}
				if err := f.a.shutdown(context.Background()); err != nil {
					t.Fatal(err)
				}
				worker := startMinIOWorker(t, f)
				f.setFault("hold-" + direction)
				var tid string
				destination := filepath.Join(f.root, "saved.bin")
				original := []byte("keep original after process exit")
				if direction == "upload" {
					f.keys = append(f.keys, "crash.bin")
					worker.api(t, "upload-local", map[string]string{"ProfileID": f.profile.ID, "Bucket": f.bucket, "Key": "crash.bin", "Path": f.source}, &tid)
				} else {
					if err := os.WriteFile(destination, original, 0600); err != nil {
						t.Fatal(err)
					}
					var result SaveResult
					worker.api(t, "download", map[string]any{"ProfileID": f.profile.ID, "Bucket": f.bucket, "Key": "download.bin", "Path": f.root, "Name": "saved.bin", "Overwrite": true}, &result)
					tid = result.ID
				}
				f.awaitFault()
				if direction == "upload" && f.multipartCount() != 1 {
					t.Fatal("no remote multipart before Harbor exit")
				}
				if direction == "upload" {
					f.awaitStoredPart()
				}
				if direction == "download" {
					// Ensure some bytes actually reached disk before stopping the process.
					deadline := time.Now().Add(5 * time.Second)
					for {
						paths, _ := filepath.Glob(filepath.Join(f.root, ".harbor-download-*"))
						if len(paths) > 0 {
							st, err := os.Stat(paths[0])
							if err == nil && st.Size() > 0 {
								break
							}
						}
						if time.Now().After(deadline) {
							t.Fatal("download never wrote partial data")
						}
						time.Sleep(10 * time.Millisecond)
					}
				}
				if stopKind != "graceful" {
					if err := worker.cmd.Process.Kill(); err != nil {
						t.Fatal(err)
					}
				} else {
					if err := worker.cmd.Process.Signal(os.Interrupt); err != nil {
						t.Fatal(err)
					}
				}
				done := make(chan error, 1)
				go func() { done <- worker.cmd.Wait() }()
				select {
				case err := <-done:
					if stopKind == "graceful" && err != nil {
						t.Fatal("graceful exit failed:", err)
					}
				case <-time.After(35 * time.Second):
					t.Fatal("Harbor did not exit within shutdown budget")
				}
				f.setFault("")
				if direction == "download" {
					assertMinIOFile(t, destination, original)
				}
				if stopKind == "kill_offline_reopen" {
					f.setFault("offline")
				}
				recovered := startMinIOWorker(t, f)
				tr := recovered.wait(t, tid)
				if stopKind != "graceful" && (tr.Status != "failed" || !strings.Contains(tr.Error, "Interrupted")) {
					t.Fatalf("crash not exposed as retryable interruption: %+v", tr)
				}
				if stopKind == "graceful" && tr.Status != "cancelled" {
					t.Fatalf("shutdown not retained as cancelled: %+v", tr)
				}
				if direction == "upload" {
					if stopKind == "kill_offline_reopen" {
						if tr.CleanupError == "" {
							t.Fatal("offline recovery hid pending multipart cleanup")
						}
						assertMinIOFile(t, tr.Path, f.payload)
						f.setFault("")
						if f.multipartCount() != 1 {
							t.Fatal("offline startup discarded pending real multipart upload")
						}
					} else {
						f.assertNoMultipart()
					}
					assertMinIOFile(t, tr.Path, f.payload)
				} else {
					assertNoMinIOPartials(t, f.root)
					assertMinIOFile(t, destination, original)
				}
				recovered.api(t, "retry", map[string]string{"ID": tid}, nil)
				if tr = recovered.wait(t, tid); tr.Status != "completed" {
					t.Fatalf("restart/retry failed: %+v", tr)
				}
				if direction == "upload" {
					f.verifyObject("crash.bin")
					f.assertNoMultipart()
				} else {
					assertMinIOFile(t, destination, f.payload)
					assertNoMinIOPartials(t, f.root)
				}
				assertMinIOFile(t, f.source, f.payload)
				_ = recovered.cmd.Process.Signal(os.Interrupt)
				if err := recovered.cmd.Wait(); err != nil {
					t.Fatal(err)
				}
				t.Logf("%s during real MinIO %s: recovered journal, cleaned owned resources, retried successfully with exact bytes", stopKind, direction)
			})
		}
	}
}
