package main

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/pem"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/feature/s3/manager"
	"github.com/aws/aws-sdk-go-v2/service/s3"
)

// This opt-in test uses real native time, real MinIO multipart/range requests,
// and the production SDK/scheduler. No controller/resource knobs are injected.
func TestMinIOAdaptiveMixedTransfers(t *testing.T) {
	if os.Getenv("HARBOR_ADAPTIVE_MIXED_MINIO") != "1" {
		t.Skip("set HARBOR_ADAPTIVE_MIXED_MINIO=1 and HARBOR_MINIO_BINARY for paced live mixed transfers")
	}
	f := newMinIOResilience(t)
	ctx, cancel := context.WithTimeout(context.Background(), 295*time.Second)
	defer cancel()
	shape := &mixedMinIOShape{rate: 4 << 20, activeKeys: map[string]int{}}
	target, _ := url.Parse("http://" + f.address)
	reverse := httputil.NewSingleHostReverseProxy(target)
	reverse.ErrorHandler = func(w http.ResponseWriter, r *http.Request, err error) {
		http.Error(w, "injected mixed-test transport interruption", 502)
	}
	reverse.ModifyResponse = func(r *http.Response) error {
		if r.Request.Method == "GET" && r.StatusCode == 206 {
			key := r.Request.URL.Path
			flow, _ := r.Request.Context().Value(mixedFlowKey{}).(*mixedDataFlow)
			r.Body = &mixedPacedBody{ReadCloser: r.Body, flow: flow, ctx: r.Request.Context(), rate: shape.rate, start: time.Now(), drop: func() bool {
				shape.mu.Lock()
				defer shape.mu.Unlock()
				if shape.downloadDropKey != "" && key == "/"+f.bucket+"/"+shape.downloadDropKey {
					if mixedOtherKeyLocked(shape, key) {
						shape.downloadFaultOverlap.Store(true)
					}
					shape.downloadDrops.Add(1)
					return true
				}
				return false
			}}
		}
		return nil
	}
	proxy := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upload := r.Method == "PUT" && r.URL.Query().Get("partNumber") != ""
		download := r.Method == "GET" && r.Header.Get("Range") != ""
		if upload && shape.slowDown.CompareAndSwap(0, 1) {
			// Drain the upload so a real HTTP 503 reaches the SDK, rather
			// than a reset caused by closing an unread large request body.
			if _, err := io.Copy(io.Discard, r.Body); err != nil {
				http.Error(w, "fault fixture could not read upload", http.StatusBadGateway)
				return
			}
			w.Header().Set("Content-Type", "application/xml")
			w.WriteHeader(503)
			_, _ = io.WriteString(w, "<Error><Code>SlowDown</Code><Message>Injected paced mixed test</Message></Error>")
			return
		}
		if upload || download {
			shape.mu.Lock()
			if upload && shape.uploadRetryKey == r.URL.Path && mixedOtherKeyLocked(shape, r.URL.Path) {
				shape.uploadRetryOverlap.Store(true)
			}
			if download && shape.downloadRetryKey == r.URL.Path && mixedOtherKeyLocked(shape, r.URL.Path) {
				shape.downloadRetryOverlap.Store(true)
			}
			shape.mu.Unlock()
		}
		var flow *mixedDataFlow
		if upload || download {
			flow = shape.startFlow(r.URL.Path, upload)
			defer flow.finish() // fallback for rejection/disconnect before a body wrapper
			r = r.WithContext(context.WithValue(r.Context(), mixedFlowKey{}, flow))
		}
		if upload {
			shape.mu.Lock()
			drop := shape.dropKey != "" && r.URL.Path == "/"+f.bucket+"/"+shape.dropKey
			if drop && mixedOtherKeyLocked(shape, r.URL.Path) {
				shape.uploadFaultOverlap.Store(true)
			}
			shape.mu.Unlock()
			if drop {
				shape.drops.Add(1)
				_, _ = io.CopyN(io.Discard, r.Body, 128<<10)
				flow.finish() // end the body before the client observes the disconnect
				disconnectMinIO(w)
				return
			}
			r.Body = &mixedPacedBody{ReadCloser: r.Body, flow: flow, ctx: r.Context(), rate: shape.rate, start: time.Now()}
		}
		reverse.ServeHTTP(w, r)
	}))
	t.Cleanup(func() { cancel(); _ = f.a.shutdown(context.Background()); proxy.Close() })
	ca := filepath.Join(f.root, "mixed-ca.pem")
	if err := os.WriteFile(ca, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: proxy.Certificate().Raw}), 0600); err != nil {
		t.Fatal(err)
	}
	profile, err := f.a.SaveProfile(Profile{Name: "Isolated paced mixed transfers", Region: "us-east-1", Endpoint: proxy.URL, CAFile: ca, PathStyle: true}, Secret{AccessKey: "harbor-resilience", SecretKey: "synthetic-resilience-secret"})
	if err != nil {
		t.Fatal(err)
	}
	if mode := f.a.TransferTuningStatus().Scheduler.Mode; mode != "auto" {
		t.Fatal("test requires production Automatic default", mode)
	}
	jobs := make([]mixedMinIOJob, 6)
	for i := range jobs {
		// Three eight-worker uploads can leave 24 pre-probe SDK operations
		// queued. After they drain, fresh warm-up plus a 15-completion probe
		// window still needs stable demand. 64 parts per upload preserves all
		// three transfer identities through that measurement and later faults.
		size := int64(1 << 30)
		if i >= 3 {
			size = 128 << 20
		}
		jobs[i] = mixedMakeFile(t, f.root, i, size)
		if i < 3 {
			jobs[i].direction = "upload"
			jobs[i].key = fmt.Sprintf("mixed-upload-%d.bin", i)
			f.keys = append(f.keys, jobs[i].key)
		} else {
			jobs[i].direction = "download"
			jobs[i].key = fmt.Sprintf("mixed-seed-%d.bin", i)
			file, err := os.Open(jobs[i].source)
			if err != nil {
				t.Fatal(err)
			}
			_, err = manager.NewUploader(f.client, func(u *manager.Uploader) { u.PartSize = 16 << 20; u.Concurrency = 1 }).Upload(ctx, &s3.PutObjectInput{Bucket: aws.String(f.bucket), Key: aws.String(jobs[i].key), Body: file})
			file.Close()
			if err != nil {
				t.Fatal(err)
			}
			jobs[i].destination = filepath.Join(f.root, fmt.Sprintf("mixed-saved-%d.bin", i))
			jobs[i].original = []byte("existing destination " + strconv.Itoa(i) + " must remain intact")
			if err = os.WriteFile(jobs[i].destination, jobs[i].original, 0600); err != nil {
				t.Fatal(err)
			}
		}
	}
	started := time.Now()
	for i := 0; i < 3; i++ {
		jobs[i].id, err = f.a.QueueLocalUpload(ctx, profile.ID, f.bucket, jobs[i].key, jobs[i].source)
		if err != nil {
			t.Fatal(err)
		}
		download := &jobs[i+3]
		result, e := f.a.QueueDownloadTo(profile.ID, f.bucket, download.key, f.root, filepath.Base(download.destination), true)
		if e != nil {
			t.Fatal(e)
		}
		download.id = result.ID
	}
	s := f.a.tuning().scheduler
	scope := tuningScope(profile)
	lowest := 8
	backoff, recovered := false, false
	lastTarget := -1
	lastProbe := -1
	var backoffAt, acceptedAt time.Time
	peakScheduler := 0
	interruptedDownload := -1
	for !recovered {
		select {
		case <-ctx.Done():
			t.Fatal("live adaptive recovery deadline", s.Snapshot())
		default:
		}
		// Interrupt one real ranged response after a durable first range, while
		// all six original transfers are still competing for network admission.
		if interruptedDownload < 0 {
			for i := 3; i < 6; i++ {
				tr := mixedFindTransfer(t, f.a, jobs[i].id)
				if tr.Status == "running" && tr.Resumable && tr.Done < tr.Total {
					interruptedDownload = i
					mixedAwaitOtherKey(t, ctx, shape, "/"+f.bucket+"/"+jobs[i].key)
					shape.mu.Lock()
					shape.downloadDropKey = jobs[i].key
					shape.mu.Unlock()
					failed := mixedWaitTransfer(t, ctx, f.a, jobs[i].id)
					if failed.Status != "failed" || !failed.Resumable || shape.downloadDrops.Load() == 0 {
						t.Fatalf("download interruption lost recovery: %+v", failed)
					}
					got, e := os.ReadFile(jobs[i].destination)
					if e != nil || string(got) != string(jobs[i].original) {
						t.Fatal("interrupted download changed existing destination", e)
					}
					shape.mu.Lock()
					shape.downloadDropKey = ""
					shape.downloadRetryKey = "/" + f.bucket + "/" + jobs[i].key
					shape.mu.Unlock()
					mixedAwaitOtherKey(t, ctx, shape, "/"+f.bucket+"/"+jobs[i].key)
					if e = f.a.RetryTransfer(jobs[i].id); e != nil {
						t.Fatal(e)
					}
					t.Logf("real elapsed=%s download interrupted after verified range; old destination intact; Retry queued", time.Since(started).Round(time.Millisecond))
					break
				}
			}
		}
		snapshot := s.Snapshot()
		peakScheduler = max(peakScheduler, snapshot.ActiveBulk)
		if snapshot.ActiveBulk > 8 || snapshot.ActiveControl > 2 {
			t.Fatal("scheduler admission cap exceeded", snapshot)
		}
		state := mixedReadScope(s, scope)
		if state.target != lastTarget || state.probe != lastProbe {
			t.Logf("real elapsed=%s upload target=%d download target=%d active=%d/%d proxy active=%d/%d throttles=%d completed checkpoints=%d", time.Since(started).Round(time.Millisecond), state.target, state.healthy, state.uploadActive, state.downloadActive, shape.uploadActive.Load(), shape.downloadActive.Load(), state.throttles, state.completed)
			t.Logf("policy reason=%q probeFrom=%d quorum=%d/%d windowBytes=%d baselineMiBps=%.3f previousMiBps=%.3f windowStart=%s eligibleAfter=%s cooldownUntil=%s files=%s", state.reason, state.probe, state.samples, 3*state.target, state.windowBytes, state.baseline/(1<<20), state.previous/(1<<20), mixedRelativeTime(started, state.windowStart), mixedRelativeTime(started, state.eligible), mixedRelativeTime(started, state.cooldown), mixedJobProgress(f.a, jobs))
			lastTarget = state.target
			lastProbe = state.probe
		}
		if state.target < lowest {
			lowest = state.target
		}
		if lowest < 8 {
			if !backoff {
				backoffAt = time.Now()
			}
			backoff = true
		}
		if backoff && state.healthy != 8 {
			t.Fatal("upload-only SlowDown reduced healthy download target", state)
		}
		recovered = backoff && state.target > lowest && state.probe == 0 && state.reason == "Higher concurrency improved speed" && state.baseline > 0
		if recovered {
			acceptedAt = time.Now()
			if acceptedAt.Sub(backoffAt) < 30*time.Second {
				t.Fatal("recovery bypassed real provider cooldown", acceptedAt.Sub(backoffAt))
			}
		}
		if !recovered {
			allDone := true
			for _, tr := range f.a.Transfers() {
				if tr.Status == "running" || tr.Status == "queued" {
					allDone = false
				}
			}
			if allDone {
				t.Fatalf("transfers ended before genuine provider recovery: low=%d state=%+v", lowest, state)
			}
			time.Sleep(50 * time.Millisecond)
		}
	}
	if interruptedDownload < 0 {
		t.Fatal("no live checkpointed download interruption occurred")
	}
	if !shape.overlap.Load() || shape.slowDown.Load() != 1 {
		t.Fatal("missing mixed streaming or real503 provenance")
	}
	t.Logf("accepted live upload recovery from%d to%d at%s, %s after observed backoff; measured gain confirmed; no fake clock or resource ceiling", lowest, mixedReadScope(s, scope).target, acceptedAt.Sub(started).Round(time.Millisecond), acceptedAt.Sub(backoffAt).Round(time.Millisecond))
	// Interrupt an upload only after accepted live adaptation, keeping the same
	// transfer population stable throughout the tuning comparison.
	interrupted := -1
	for i := 0; i < 3; i++ {
		tr := mixedFindTransfer(t, f.a, jobs[i].id)
		if tr.Status == "running" && tr.Resumable && tr.Done < tr.Total {
			interrupted = i
			break
		}
	}
	if interrupted < 0 {
		t.Fatal("no checkpointed upload remains for live interruption")
	}
	mixedAwaitOtherKey(t, ctx, shape, "/"+f.bucket+"/"+jobs[interrupted].key)
	shape.mu.Lock()
	shape.dropKey = jobs[interrupted].key
	shape.mu.Unlock()
	failed := mixedWaitTransfer(t, ctx, f.a, jobs[interrupted].id)
	if failed.Status != "failed" || !failed.Resumable || shape.drops.Load() == 0 {
		t.Fatalf("interruption did not preserve checkpoints: %+v drops=%d", failed, shape.drops.Load())
	}
	shape.mu.Lock()
	shape.dropKey = ""
	shape.uploadRetryKey = "/" + f.bucket + "/" + jobs[interrupted].key
	shape.mu.Unlock()
	mixedAwaitOtherKey(t, ctx, shape, "/"+f.bucket+"/"+jobs[interrupted].key)
	if err = f.a.RetryTransfer(jobs[interrupted].id); err != nil {
		t.Fatal(err)
	}
	for i := range jobs {
		tr := mixedWaitTransfer(t, ctx, f.a, jobs[i].id)
		if tr.Status != "completed" {
			t.Fatalf("mixed transfer %d did not complete: %+v", i, tr)
		}
		if (i == interrupted || i == interruptedDownload) && tr.ResumedBytes <= 0 {
			t.Fatal("interrupted mixed upload restarted instead of resuming")
		}
		var reader io.ReadCloser
		if jobs[i].direction == "upload" {
			out, e := f.client.GetObject(ctx, &s3.GetObjectInput{Bucket: aws.String(f.bucket), Key: aws.String(jobs[i].key)})
			if e != nil {
				t.Fatal(e)
			}
			reader = out.Body
		} else {
			reader, err = os.Open(jobs[i].destination)
			if err != nil {
				t.Fatal(err)
			}
		}
		h := sha256.New()
		n, e := io.Copy(h, reader)
		reader.Close()
		if e != nil || n != jobs[i].size || hex.EncodeToString(h.Sum(nil)) != jobs[i].digest {
			t.Fatalf("distinct mixed file%d SHA256 mismatch bytes=%d err=%v", i, n, e)
		}
		t.Logf("file%d %s distinct SHA256 verified; resumed=%d", i, jobs[i].direction, tr.ResumedBytes)
	}
	if !shape.uploadFaultOverlap.Load() || !shape.downloadFaultOverlap.Load() || !shape.uploadRetryOverlap.Load() || !shape.downloadRetryOverlap.Load() {
		t.Fatalf("missing actual different-object overlap: upload fault/retry=%t/%t download fault/retry=%t/%t", shape.uploadFaultOverlap.Load(), shape.uploadRetryOverlap.Load(), shape.downloadFaultOverlap.Load(), shape.downloadRetryOverlap.Load())
	}
	f.assertNoMultipart()
	assertNoMinIOPartials(t, f.root)
	deadline := time.Now().Add(5 * time.Second)
	for {
		snapshot := s.Snapshot()
		if snapshot.ActiveBulk == 0 && snapshot.ActiveControl == 0 && snapshot.WaitingBulk == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("mixed test leaked scheduler admission", snapshot)
		}
		time.Sleep(10 * time.Millisecond)
	}
	if peak := shape.combinedPeak.Load(); peak < 2 || peak > 8 || peakScheduler > 8 {
		t.Fatal("invalid actual combined stream concurrency", peak)
	}
	t.Logf("mixed real MinIO passed in%s: three1GiB uploads/three128MiB downloads, per-request cap4MiB/s, one503, %d persistent upload drops, proxy peaks upload/download=%d/%d; no multipart/partial/admission leaks", time.Since(started).Round(time.Millisecond), shape.drops.Load(), shape.uploadPeak.Load(), shape.downloadPeak.Load())
}

type mixedMinIOShape struct {
	uploadRetryKey, downloadRetryKey                                                   string
	uploadFaultOverlap, downloadFaultOverlap, uploadRetryOverlap, downloadRetryOverlap atomic.Bool
	activeKeys                                                                         map[string]int
	combinedActive, combinedPeak                                                       atomic.Int32
	rate                                                                               int64
	slowDown, drops, uploadActive, downloadActive, uploadPeak, downloadPeak            atomic.Int32
	overlap                                                                            atomic.Bool
	mu                                                                                 sync.Mutex
	dropKey                                                                            string
	downloadDropKey                                                                    string
	downloadDrops                                                                      atomic.Int32
}
type mixedPacedBody struct {
	io.ReadCloser
	ctx   context.Context
	rate  int64
	start time.Time
	bytes int64
	drop  func() bool
	flow  *mixedDataFlow
}

func (b *mixedPacedBody) Read(p []byte) (int, error) {
	if b.drop != nil && b.drop() {
		b.flow.finish()
		return 0, io.ErrUnexpectedEOF
	}
	if len(p) > 64<<10 {
		p = p[:64<<10]
	}
	n, err := b.ReadCloser.Read(p)
	b.bytes += int64(n)
	wait := time.Duration(float64(b.bytes)/float64(b.rate)*float64(time.Second)) - time.Since(b.start)
	if wait > 0 {
		timer := time.NewTimer(wait)
		defer timer.Stop()
		select {
		case <-timer.C:
		case <-b.ctx.Done():
			b.flow.finish()
			return n, b.ctx.Err()
		}
	}
	if err != nil {
		b.flow.finish()
	}
	return n, err
}
func (b *mixedPacedBody) Close() error { err := b.ReadCloser.Close(); b.flow.finish(); return err }

type mixedMinIOJob struct {
	id, direction, key, source, destination, digest string
	size                                            int64
	original                                        []byte
}

func mixedMakeFile(t *testing.T, root string, index int, size int64) mixedMinIOJob {
	t.Helper()
	path := filepath.Join(root, fmt.Sprintf("mixed-source-%d.bin", index))
	file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		t.Fatal(err)
	}
	block := make([]byte, 1<<20)
	for i := range block {
		block[i] = byte((i*31 + i/7919 + index*43) % 251)
	}
	copy(block, []byte(fmt.Sprintf("Harbor distinct mixed file %d", index)))
	h := sha256.New()
	writer := io.MultiWriter(file, h)
	for left := size; left > 0; {
		n := min(left, int64(len(block)))
		binary.LittleEndian.PutUint64(block[len(block)-8:], uint64(size-left))
		if _, err = writer.Write(block[:n]); err != nil {
			file.Close()
			t.Fatal(err)
		}
		left -= n
	}
	if err = file.Close(); err != nil {
		t.Fatal(err)
	}
	return mixedMinIOJob{size: size, source: path, digest: hex.EncodeToString(h.Sum(nil))}
}

type mixedScopeState struct {
	target, healthy, uploadActive, downloadActive, probe int
	completed, throttles                                 uint64
	reason                                               string
	samples                                              int
	windowBytes                                          int64
	baseline, previous                                   float64
	windowStart, eligible, cooldown                      time.Time
}

func mixedReadScope(s *transferScheduler, scope string) mixedScopeState {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := schedulerScope{scope, "upload"}
	state := mixedScopeState{target: s.scopeLimit(key), healthy: s.scopeLimit(schedulerScope{scope, "download"}), uploadActive: s.scopes[key], downloadActive: s.scopes[schedulerScope{scope, "download"}], completed: s.completed, throttles: s.throttled}
	if p := s.learning[key]; p != nil {
		state.probe = p.policy.probeFrom
		state.reason = p.policy.reason
		state.samples = p.policy.successes
		state.windowBytes = p.policy.bytes
		state.baseline = p.policy.baseline
		state.previous = p.policy.previous
		state.windowStart = p.policy.start
		state.eligible = p.policy.eligibleAfter
		state.cooldown = p.policy.cooldown
	}
	return state
}
func mixedFindTransfer(t *testing.T, a *App, id string) Transfer {
	t.Helper()
	for _, tr := range a.Transfers() {
		if tr.ID == id {
			return tr
		}
	}
	t.Fatal("mixed transfer disappeared")
	return Transfer{}
}
func mixedWaitTransfer(t *testing.T, ctx context.Context, a *App, id string) Transfer {
	t.Helper()
	for {
		a.mu.Lock()
		tr := *a.transfers[id]
		active := tr.cancel != nil
		a.mu.Unlock()
		if !active && (tr.Status == "completed" || tr.Status == "failed" || tr.Status == "cancelled") {
			return tr
		}
		select {
		case <-ctx.Done():
			t.Fatal("mixed transfer deadline", tr.Status, tr.Error)
		case <-time.After(20 * time.Millisecond):
		}
	}
}

func mixedAwaitOtherKey(t *testing.T, ctx context.Context, shape *mixedMinIOShape, selected string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		shape.mu.Lock()
		other := false
		for key, n := range shape.activeKeys {
			if key != selected && n > 0 {
				other = true
				break
			}
		}
		shape.mu.Unlock()
		if other {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("interruption or retry did not overlap a different object stream")
		}
		select {
		case <-ctx.Done():
			t.Fatal("deadline awaiting independent stream")
		case <-time.After(20 * time.Millisecond):
		}
	}
}

type mixedFlowKey struct{}
type mixedDataFlow struct {
	shape  *mixedMinIOShape
	key    string
	upload bool
	once   sync.Once
}

func (s *mixedMinIOShape) startFlow(key string, upload bool) *mixedDataFlow {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.activeKeys[key]++
	total := s.combinedActive.Add(1)
	for old := s.combinedPeak.Load(); total > old && !s.combinedPeak.CompareAndSwap(old, total); old = s.combinedPeak.Load() {
	}
	active, peak := &s.uploadActive, &s.uploadPeak
	if !upload {
		active, peak = &s.downloadActive, &s.downloadPeak
	}
	current := active.Add(1)
	for old := peak.Load(); current > old && !peak.CompareAndSwap(old, current); old = peak.Load() {
	}
	if upload && s.downloadActive.Load() > 0 || !upload && s.uploadActive.Load() > 0 {
		s.overlap.Store(true)
	}
	return &mixedDataFlow{shape: s, key: key, upload: upload}
}
func (f *mixedDataFlow) finish() {
	if f == nil {
		return
	}
	f.once.Do(func() {
		s := f.shape
		s.mu.Lock()
		defer s.mu.Unlock()
		if f.upload {
			s.uploadActive.Add(-1)
		} else {
			s.downloadActive.Add(-1)
		}
		s.combinedActive.Add(-1)
		s.activeKeys[f.key]--
		if s.activeKeys[f.key] == 0 {
			delete(s.activeKeys, f.key)
		}
	})
}

func mixedOtherKeyLocked(shape *mixedMinIOShape, selected string) bool {
	for key, n := range shape.activeKeys {
		if key != selected && n > 0 {
			return true
		}
	}
	return false
}

func TestMixedDataFlowCountsBodyCompletion(t *testing.T) {
	shape := &mixedMinIOShape{activeKeys: map[string]int{}}
	upload := shape.startFlow("upload", true)
	download := shape.startFlow("download", false)
	body := &mixedPacedBody{ReadCloser: io.NopCloser(strings.NewReader("x")), ctx: context.Background(), rate: 1 << 20, start: time.Now(), flow: upload}
	if _, err := io.Copy(io.Discard, body); err != nil {
		t.Fatal(err)
	}
	if shape.uploadActive.Load() != 0 || shape.combinedActive.Load() != 1 {
		t.Fatal("bodyEOF retained completed upload stream")
	}
	// The proxy handler can still exist after EOF; its deferred finish and Close
	// must not double-decrement the actual body-stream count.
	upload.finish()
	_ = body.Close()
	if shape.combinedActive.Load() != 1 {
		t.Fatal("body completion was counted twice")
	}
	download.finish()
	if shape.combinedActive.Load() != 0 {
		t.Fatal("stream count leaked")
	}
}

func mixedRelativeTime(started, at time.Time) string {
	if at.IsZero() {
		return "none"
	}
	return at.Sub(started).Round(time.Millisecond).String()
}
func mixedJobProgress(a *App, jobs []mixedMinIOJob) string {
	byID := map[string]Transfer{}
	for _, tr := range a.Transfers() {
		byID[tr.ID] = tr
	}
	values := make([]string, 0, len(jobs))
	for i, job := range jobs {
		tr := byID[job.id]
		values = append(values, fmt.Sprintf("%s%d:%s:%d/%dMiB", job.direction, i, tr.Status, tr.Done>>20, tr.Total>>20))
	}
	return strings.Join(values, ",")
}
