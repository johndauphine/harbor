package main

import (
	"context"
	"encoding/pem"
	"encoding/xml"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"
)

type deleteFixture struct {
	mu            sync.Mutex
	items         map[string]deleteItem
	uploads       map[string]deleteItem
	calls         []string
	denied        string
	deniedVersion string
	pageSize      int
	failPage      bool
	hideUploads   bool
	beforeDelete  func(*deleteFixture)
	deleting      chan struct{}
	release       chan struct{}
	bucketDeleted bool
}

func fixtureID(i deleteItem) string       { return i.Key + "\x00" + i.VersionID }
func (f *deleteFixture) put(i deleteItem) { f.items[fixtureID(i)] = i }
func (f *deleteFixture) countDeletes() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, c := range f.calls {
		if strings.HasPrefix(c, "DELETE ") {
			n++
		}
	}
	return n
}
func xmlText(s string) string {
	var b strings.Builder
	_ = xml.EscapeText(&b, []byte(s))
	return b.String()
}
func (f *deleteFixture) serve(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, r.Method+" "+r.URL.RequestURI())
	q := r.URL.Query()
	key := strings.TrimPrefix(r.URL.Path, "/bucket/")
	if r.Method == "GET" && q.Has("versions") {
		if f.failPage && q.Get("key-marker") != "" {
			w.WriteHeader(403)
			fmt.Fprint(w, `<Error><Code>AccessDenied</Code></Error>`)
			return
		}
		list := []deleteItem{}
		for _, i := range f.items {
			if strings.HasPrefix(i.Key, q.Get("prefix")) {
				list = append(list, i)
			}
		}
		sort.Slice(list, func(a, b int) bool { return fixtureID(list[a]) < fixtureID(list[b]) })
		start := 0
		if q.Get("key-marker") != "" {
			marker := q.Get("key-marker") + "\x00" + q.Get("version-id-marker")
			for start < len(list) && fixtureID(list[start]) <= marker {
				start++
			}
		}
		end := len(list)
		if f.pageSize > 0 && start+f.pageSize < end {
			end = start + f.pageSize
		}
		fmt.Fprint(w, `<ListVersionsResult xmlns="http://s3.amazonaws.com/doc/2006-03-01/">`)
		for _, i := range list[start:end] {
			tag := "Version"
			if i.Marker {
				tag = "DeleteMarker"
			}
			fmt.Fprintf(w, "<%s><Key>%s</Key><VersionId>%s</VersionId><ETag>%s</ETag><Size>%d</Size><IsLatest>%t</IsLatest></%s>", tag, xmlText(i.Key), xmlText(i.VersionID), xmlText(i.ETag), i.Size, i.IsLatest, tag)
		}
		if end < len(list) {
			last := list[end-1]
			fmt.Fprintf(w, "<IsTruncated>true</IsTruncated><NextKeyMarker>%s</NextKeyMarker><NextVersionIdMarker>%s</NextVersionIdMarker>", xmlText(last.Key), xmlText(last.VersionID))
		} else {
			fmt.Fprint(w, "<IsTruncated>false</IsTruncated>")
		}
		fmt.Fprint(w, "</ListVersionsResult>")
		return
	}
	if r.Method == "GET" && q.Has("uploads") {
		fmt.Fprint(w, `<ListMultipartUploadsResult xmlns="http://s3.amazonaws.com/doc/2006-03-01/"><IsTruncated>false</IsTruncated>`)
		for _, i := range f.uploads {
			if f.hideUploads {
				continue
			}
			if strings.HasPrefix(i.Key, q.Get("prefix")) {
				fmt.Fprintf(w, "<Upload><Key>%s</Key><UploadId>%s</UploadId></Upload>", xmlText(i.Key), xmlText(i.UploadID))
			}
		}
		fmt.Fprint(w, "</ListMultipartUploadsResult>")
		return
	}
	if r.Method == "GET" && q.Get("uploadId") != "" {
		if _, ok := f.uploads[q.Get("uploadId")]; !ok {
			w.WriteHeader(404)
			fmt.Fprint(w, `<Error><Code>NoSuchUpload</Code></Error>`)
			return
		}
		fmt.Fprint(w, `<ListPartsResult/>`)
		return
	}
	if r.Method == "HEAD" {
		i, ok := f.items[key+"\x00"+q.Get("versionId")]
		if !ok {
			w.WriteHeader(404)
			return
		}
		w.Header().Set("ETag", i.ETag)
		w.Header().Set("Content-Length", fmt.Sprint(i.Size))
		w.WriteHeader(200)
		return
	}
	if r.Method == "DELETE" {
		if f.deleting != nil {
			close(f.deleting)
			f.deleting = nil
			f.mu.Unlock()
			select {
			case <-f.release:
			case <-r.Context().Done():
			}
			f.mu.Lock()
		}
		if f.beforeDelete != nil {
			hook := f.beforeDelete
			f.beforeDelete = nil
			hook(f)
		}
		if r.URL.Path == "/bucket" {
			if len(f.items) > 0 || len(f.uploads) > 0 {
				w.WriteHeader(409)
				fmt.Fprint(w, `<Error><Code>BucketNotEmpty</Code></Error>`)
				return
			}
			f.bucketDeleted = true
			w.WriteHeader(204)
			return
		}
		if q.Get("uploadId") != "" {
			delete(f.uploads, q.Get("uploadId"))
			w.WriteHeader(204)
			return
		}
		if key == f.denied && (f.deniedVersion == "" || q.Get("versionId") == f.deniedVersion) {
			w.WriteHeader(403)
			fmt.Fprint(w, `<Error><Code>AccessDenied</Code></Error>`)
			return
		}
		i, ok := f.items[key+"\x00"+q.Get("versionId")]
		if !ok {
			w.WriteHeader(404)
			fmt.Fprint(w, `<Error><Code>NoSuchVersion</Code></Error>`)
			return
		}
		if !i.Marker && r.Header.Get("If-Match") != i.ETag {
			w.WriteHeader(412)
			fmt.Fprint(w, `<Error><Code>PreconditionFailed</Code></Error>`)
			return
		}
		if r.Header.Get("x-amz-bypass-governance-retention") != "" {
			panic("retention bypass forbidden")
		}
		delete(f.items, fixtureID(i))
		w.WriteHeader(204)
		return
	}
	w.WriteHeader(400)
	fmt.Fprint(w, `<Error><Code>UnexpectedOperation</Code></Error>`)
}
func setupDeleteFixture(t *testing.T) (*App, *deleteFixture) {
	t.Helper()
	f := &deleteFixture{items: map[string]deleteItem{}, uploads: map[string]deleteItem{}}
	server := httptest.NewTLSServer(http.HandlerFunc(f.serve))
	t.Cleanup(server.Close)
	ca := filepath.Join(t.TempDir(), "ca.pem")
	if err := os.WriteFile(ca, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw}), 0600); err != nil {
		t.Fatal(err)
	}
	a := NewApp()
	a.ctx = context.Background()
	a.profiles = []Profile{{ID: "test", Name: "Test", Endpoint: server.URL, Region: "us-east-1", PathStyle: true, CAFile: ca}}
	a.secretGet = func(string) (string, error) { return `{"AccessKey":"test","SecretKey":"test"}`, nil }
	return a, f
}
func content(key, version string) deleteItem {
	return deleteItem{Key: key, VersionID: version, ETag: `"etag"`, Size: 12}
}
func mustPreview(t *testing.T, a *App, kind, prefix string) DeletePreview {
	t.Helper()
	p, err := a.PreviewDelete(context.Background(), "test", "bucket", prefix, kind)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func TestDeletePlanFolderExactScopePaginationAndMarker(t *testing.T) {
	a, f := setupDeleteFixture(t)
	f.pageSize = 1
	for _, key := range []string{"folder/", "folder/nested/data", "folder/a%2Fb <x>.txt", "folder-other/keep"} {
		f.put(content(key, "null"))
	}
	p := mustPreview(t, a, "folder", "folder/")
	if p.Objects != 3 || p.Versions != 3 || p.TotalBytes != 36 || p.Confirmation != "folder/" {
		t.Fatalf("unexpected preview: %+v", p)
	}
	if len(a.activeDeletes) != 0 {
		t.Fatal("preview leaked scope reservation")
	}
	result, err := a.ExecuteDelete(context.Background(), p.PlanID, p.Confirmation)
	if err != nil || !result.Completed || result.Deleted != 3 {
		t.Fatalf("result %+v error %v", result, err)
	}
	if _, ok := f.items["folder-other/keep\x00null"]; !ok {
		t.Fatal("sibling was deleted")
	}
	if f.bucketDeleted {
		t.Fatal("folder deletion deleted bucket")
	}
	if _, err = a.ExecuteDelete(context.Background(), p.PlanID, p.Confirmation); err == nil {
		t.Fatal("replayed plan accepted")
	}
}
func TestDeletePlanBucketVersionsMarkersAndMultipart(t *testing.T) {
	a, f := setupDeleteFixture(t)
	f.put(content("report", "v1"))
	f.put(content("report", "v2"))
	f.put(deleteItem{Key: "report", VersionID: "marker-v3", Marker: true})
	f.put(content("plain", "null"))
	f.uploads["upload-id"] = deleteItem{Key: "unfinished", UploadID: "upload-id"}
	p := mustPreview(t, a, "bucket", "")
	if p.Objects != 2 || p.Versions != 3 || p.DeleteMarkers != 1 || p.MultipartUploads != 1 {
		t.Fatalf("unexpected counts %+v", p)
	}
	result, err := a.ExecuteDelete(context.Background(), p.PlanID, "bucket")
	if err != nil || !result.Completed || !result.BucketDeleted || result.Deleted != 4 || result.Aborted != 1 {
		t.Fatalf("%+v %v", result, err)
	}
	if !f.bucketDeleted {
		t.Fatal("bucket remains")
	}
}
func TestDeletePlanRejectsInvalidConfirmationExpiredChangedConnectionAndActiveTransfers(t *testing.T) {
	for _, invalid := range []struct{ kind, prefix string }{{"folder", ""}, {"folder", "/"}, {"folder", "no-trailing-slash"}, {"folder", "bad\x00/"}, {"bucket", "folder/"}, {"other", ""}} {
		a, f := setupDeleteFixture(t)
		if _, err := a.PreviewDelete(context.Background(), "test", "bucket", invalid.prefix, invalid.kind); err == nil {
			t.Fatalf("accepted %+v", invalid)
		}
		if len(f.calls) > 0 {
			t.Fatal("invalid scope reached storage")
		}
	}
	for _, mode := range []string{"wrong-confirmation", "expired", "config-changed", "active"} {
		t.Run(mode, func(t *testing.T) {
			a, f := setupDeleteFixture(t)
			f.put(content("folder/data", "null"))
			p := mustPreview(t, a, "folder", "folder/")
			confirmation := p.Confirmation
			switch mode {
			case "wrong-confirmation":
				confirmation = "bucket"
			case "expired":
				a.deletePlans[p.PlanID].ExpiresAt = time.Now().Add(-time.Second)
			case "config-changed":
				a.profiles[0].Region = "changed"
			case "active":
				a.transfers["active"] = &Transfer{ID: "active", ProfileID: "test", Bucket: "bucket", Key: "folder/new", Status: "queued"}
			}
			if _, err := a.ExecuteDelete(context.Background(), p.PlanID, confirmation); err == nil {
				t.Fatal("unsafe plan accepted")
			}
			if f.countDeletes() != 0 {
				t.Fatal("rejected plan deleted contents")
			}
		})
	}
	a, f := setupDeleteFixture(t)
	a.transfers["active"] = &Transfer{ID: "active", ProfileID: "test", Bucket: "bucket", Key: "data", Status: "running"}
	if _, err := a.PreviewDelete(context.Background(), "test", "bucket", "", "bucket"); err == nil {
		t.Fatal("preview accepted running transfer")
	}
	if f.countDeletes() != 0 {
		t.Fatal("preview mutated storage")
	}
}
func TestDeletePlanListingFailureAndChangedPreviewNeverMutate(t *testing.T) {
	a, f := setupDeleteFixture(t)
	f.pageSize = 1
	f.failPage = true
	f.put(content("folder/a", "null"))
	f.put(content("folder/b", "null"))
	if _, err := a.PreviewDelete(context.Background(), "test", "bucket", "folder/", "folder"); err == nil {
		t.Fatal("partial listing accepted")
	}
	if f.countDeletes() != 0 {
		t.Fatal("preview deleted objects")
	}
	f.failPage = false
	p := mustPreview(t, a, "folder", "folder/")
	f.put(content("folder/new-after-preview", "null"))
	result, err := a.ExecuteDelete(context.Background(), p.PlanID, p.Confirmation)
	if err != nil || result.Completed || result.Deleted != 0 || !strings.Contains(result.Remaining, "changed") {
		t.Fatalf("%+v %v", result, err)
	}
	if f.countDeletes() != 0 {
		t.Fatal("changed snapshot mutated storage")
	}
}
func TestDeletePlanPreservesRacedContentsAndReportsPartialFailures(t *testing.T) {
	for _, mode := range []string{"changed-etag", "denied", "new-after-check", "null-marker"} {
		t.Run(mode, func(t *testing.T) {
			a, f := setupDeleteFixture(t)
			f.put(content("a", "null"))
			f.put(content("b", "v2"))
			if mode == "null-marker" {
				f.put(deleteItem{Key: "marker", VersionID: "null", Marker: true})
			}
			p := mustPreview(t, a, "bucket", "")
			switch mode {
			case "changed-etag":
				f.beforeDelete = func(f *deleteFixture) { i := f.items["a\x00null"]; i.ETag = `"new"`; f.put(i) }
			case "denied":
				f.denied = "a"
			case "new-after-check":
				f.beforeDelete = func(f *deleteFixture) { f.put(content("new", "null")) }
			}
			result, err := a.ExecuteDelete(context.Background(), p.PlanID, p.Confirmation)
			if err != nil || result.Completed || result.BucketDeleted || result.Remaining == "" {
				t.Fatalf("%+v %v", result, err)
			}
			if result.Deleted == 0 {
				t.Fatal("committed successes were not reported")
			}
			if mode != "new-after-check" && len(result.Failures) == 0 {
				t.Fatal("failure missing")
			}
			if f.bucketDeleted {
				t.Fatal("partially deleted bucket was removed")
			}
			if mode == "changed-etag" && f.items["a\x00null"].ETag != `"new"` {
				t.Fatal("replacement was deleted")
			}
		})
	}
}
func TestDeleteReservationBlocksNewTransfersAndRetry(t *testing.T) {
	a, f := setupDeleteFixture(t)
	f.put(content("folder/a", "null"))
	p := mustPreview(t, a, "folder", "folder/")
	f.deleting = make(chan struct{})
	started := f.deleting
	f.release = make(chan struct{})
	finished := make(chan error, 1)
	go func() { _, err := a.ExecuteDelete(context.Background(), p.PlanID, p.Confirmation); finished <- err }()
	<-started
	if _, err := a.queueTransfer("upload", "test", "bucket", "folder/new", "/not-used"); err == nil {
		t.Fatal("new transfer slipped into deleting scope")
	}
	a.mu.Lock()
	a.transfers["retry"] = &Transfer{ID: "retry", ProfileID: "test", Bucket: "bucket", Key: "folder/retry", Status: "failed"}
	a.mu.Unlock()
	if err := a.RetryTransfer("retry"); err == nil {
		t.Fatal("retry slipped into deleting scope")
	}
	close(f.release)
	if err := <-finished; err != nil {
		t.Fatal(err)
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.deletionBlockedLocked("test", "bucket", "folder/new") {
		t.Fatal("finished deletion leaked scope reservation")
	}
}

func TestDeletePlanRetainsMarkerWhenProtectedContentCannotBeDeleted(t *testing.T) {
	a, f := setupDeleteFixture(t)
	f.put(content("hidden", "z-content"))
	f.put(deleteItem{Key: "hidden", VersionID: "a-marker", Marker: true})
	f.denied = "hidden"
	p := mustPreview(t, a, "bucket", "")
	result, err := a.ExecuteDelete(context.Background(), p.PlanID, p.Confirmation)
	if err != nil || result.Completed || len(result.Failures) < 2 {
		t.Fatalf("%+v %v", result, err)
	}
	if _, ok := f.items["hidden\x00a-marker"]; !ok {
		t.Fatal("marker removed, exposing protected content")
	}
	for _, call := range f.calls {
		if strings.HasPrefix(call, "DELETE ") && strings.Contains(call, "versionId=a-marker") {
			t.Fatal("attempted marker removal despite retained content")
		}
	}
}
func TestDeleteReservationProtectsConnectionAndLegacyStorageMutations(t *testing.T) {
	a, f := setupDeleteFixture(t)
	f.put(content("folder/a", "null"))
	p := mustPreview(t, a, "folder", "folder/")
	f.deleting = make(chan struct{})
	started := f.deleting
	f.release = make(chan struct{})
	finished := make(chan error, 1)
	go func() { _, err := a.ExecuteDelete(context.Background(), p.PlanID, p.Confirmation); finished <- err }()
	<-started
	if _, err := a.SaveProfile(Profile{ID: "test", Name: "Changed", Region: "other"}, Secret{}); err == nil || !strings.Contains(err.Error(), "Deletion") {
		t.Fatalf("profile edit admitted: %v", err)
	}
	if err := a.DeleteProfile("test"); err == nil || !strings.Contains(err.Error(), "Deletion") {
		t.Fatalf("profile removal admitted: %v", err)
	}
	if _, err := a.CreateFolder("test", "bucket", "folder/", "new"); err == nil || !strings.Contains(err.Error(), "Deletion") {
		t.Fatalf("folder creation admitted: %v", err)
	}
	if _, err := a.DeleteFile("test", "bucket", "folder/a", `"etag"`); err == nil || !strings.Contains(err.Error(), "Deletion") {
		t.Fatalf("legacy file mutation admitted: %v", err)
	}
	if err := a.DeleteBucket("test", "bucket"); err == nil || !strings.Contains(err.Error(), "Deletion") {
		t.Fatalf("legacy bucket deletion admitted: %v", err)
	}
	close(f.release)
	if err := <-finished; err != nil {
		t.Fatal(err)
	}
	done, err := a.beginStorageMutation("test", "bucket", "folder/in-flight")
	if err != nil {
		t.Fatal(err)
	}
	defer done()
	if _, err = a.PreviewDelete(context.Background(), "test", "bucket", "folder/", "folder"); err == nil {
		t.Fatal("preview raced an admitted mutation")
	}
	if _, err = a.PreviewDelete(context.Background(), "test", "bucket", "neighbor/", "folder"); err != nil {
		t.Fatalf("unrelated scope was blocked: %v", err)
	}
}
func TestDeletePlanBoundAndJournalOwnedMultipart(t *testing.T) {
	t.Run("bounded", func(t *testing.T) {
		a, f := setupDeleteFixture(t)
		f.pageSize = 500
		for i := 0; i <= deletePlanLimit; i++ {
			f.put(content(fmt.Sprintf("folder/%05d", i), "null"))
		}
		if _, err := a.PreviewDelete(context.Background(), "test", "bucket", "folder/", "folder"); err == nil || !strings.Contains(err.Error(), "safety limit") {
			t.Fatalf("oversized plan accepted: %v", err)
		}
		if f.countDeletes() != 0 {
			t.Fatal("oversized preview deleted contents")
		}
	})
	t.Run("journal-owned upload omitted by server listing", func(t *testing.T) {
		a, f := setupDeleteFixture(t)
		f.hideUploads = true
		f.uploads["owned-id"] = deleteItem{Key: "folder/incomplete", UploadID: "owned-id"}
		a.transfers["owned"] = &Transfer{ID: "owned", ProfileID: "test", Bucket: "bucket", Key: "folder/incomplete", Status: "failed", multipartID: "owned-id", multipartScope: a.multipartScopeFor("test")}
		p := mustPreview(t, a, "folder", "folder/")
		if p.MultipartUploads != 1 {
			t.Fatalf("journal multipart missed %+v", p)
		}
		result, err := a.ExecuteDelete(context.Background(), p.PlanID, p.Confirmation)
		if err != nil || !result.Completed || result.Aborted != 1 {
			t.Fatalf("%+v %v", result, err)
		}
	})
}
func TestDeletePlanCancellationReportsProgressAndReleasesReservation(t *testing.T) {
	a, f := setupDeleteFixture(t)
	f.put(content("a", "v1"))
	f.put(content("b", "v2"))
	p := mustPreview(t, a, "bucket", "")
	ctx, cancel := context.WithCancel(context.Background())
	f.beforeDelete = func(*deleteFixture) { cancel() }
	result, err := a.ExecuteDelete(ctx, p.PlanID, p.Confirmation)
	if err != nil {
		t.Fatal(err)
	}
	if result.Completed || result.Remaining == "" {
		t.Fatalf("cancellation falsely reported complete: %+v", result)
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if len(a.activeDeletes) != 0 || len(a.operations) != 0 {
		t.Fatal("cancelled deletion leaked admission or shutdown registration")
	}
}

func TestDeletePlanPreservesCurrentVersionWhenOlderVersionDeletionFails(t *testing.T) {
	a, f := setupDeleteFixture(t)
	latest := content("report", "a-current")
	latest.IsLatest = true
	f.put(latest)
	f.put(content("report", "z-protected-old"))
	f.denied = "report"
	f.deniedVersion = "z-protected-old"
	p := mustPreview(t, a, "bucket", "")
	result, err := a.ExecuteDelete(context.Background(), p.PlanID, p.Confirmation)
	if err != nil || result.Completed || len(result.Failures) != 2 || result.Deleted != 0 {
		t.Fatalf("%+v %v", result, err)
	}
	if _, ok := f.items["report\x00a-current"]; !ok {
		t.Fatal("current version removed, exposing retained older data")
	}
	for _, call := range f.calls {
		if strings.HasPrefix(call, "DELETE ") && strings.Contains(call, "versionId=a-current") {
			t.Fatal("current version must not be removed after older version deletion fails")
		}
	}
}
