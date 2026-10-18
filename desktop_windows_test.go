//go:build desktop && windows

package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-ole/go-ole"
	"github.com/wailsapp/wails/v2"
	wailsruntime "github.com/wailsapp/wails/v2/pkg/runtime"
)

// This opt-in test opens a real WebView2 window and exercises the embedded
// frontend and private API together, without accessing real profiles or S3.
func TestDesktopWindowSmoke(t *testing.T) {
	if os.Getenv("HARBOR_DESKTOP_TEST") != "1" {
		t.Skip("set HARBOR_DESKTOP_TEST=1 to open the Windows desktop test window")
	}
	t.Logf("Desktop test executable: %s/%s", runtime.GOOS, runtime.GOARCH)
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	// Go tests run outside the main thread where Wails initialises COM.
	if err := ole.CoInitializeEx(0, ole.COINIT_APARTMENTTHREADED); err != nil {
		t.Fatal(err)
	}
	defer ole.CoUninitialize()
	payload := []byte("desktop-fixture-payload")
	large := os.Getenv("HARBOR_DESKTOP_LARGE_TEST") == "1"
	var uploaded atomic.Bool
	var largeUploaded atomic.Bool
	var partsMu sync.Mutex
	type part struct {
		size int64
		hash []byte
	}
	parts := map[int]part{}
	var largeSource string
	storage := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		if r.Method == "POST" && q.Has("uploads") {
			fmt.Fprint(w, `<InitiateMultipartUploadResult><UploadId>fixture-upload</UploadId></InitiateMultipartUploadResult>`)
			return
		}
		if r.Method == "POST" && q.Get("uploadId") != "" {
			source, err := os.Open(largeSource)
			if err != nil {
				http.Error(w, err.Error(), 500)
				return
			}
			defer source.Close()
			partsMu.Lock()
			defer partsMu.Unlock()
			var offset int64
			for i := 1; i <= len(parts); i++ {
				p := parts[i]
				hash := sha256.New()
				_, err := io.Copy(hash, io.NewSectionReader(source, offset, p.size))
				if err != nil || !bytes.Equal(hash.Sum(nil), p.hash) {
					http.Error(w, "part bytes differ", 500)
					return
				}
				offset += p.size
			}
			largeUploaded.Store(offset == 731<<20)
			fmt.Fprint(w, `<CompleteMultipartUploadResult><ETag>fixture-etag</ETag></CompleteMultipartUploadResult>`)
			return
		}
		if r.Method == "PUT" {
			var reader io.Reader = r.Body
			if strings.Contains(r.Header.Get("Content-Encoding"), "aws-chunked") {
				reader = httputil.NewChunkedReader(reader)
			}
			if large && strings.HasSuffix(r.URL.Path, "/desktop-0.txt") {
				hash := sha256.New()
				n, err := io.Copy(hash, reader)
				if err != nil {
					http.Error(w, err.Error(), 500)
					return
				}
				number, _ := strconv.Atoi(q.Get("partNumber"))
				partsMu.Lock()
				parts[number] = part{n, hash.Sum(nil)}
				partsMu.Unlock()
				w.Header().Set("ETag", fmt.Sprintf(`"part-%d"`, number))
				return
			}
			body, err := io.ReadAll(reader)
			if err == nil && bytes.Equal(body, payload) {
				uploaded.Store(true)
			}
			return
		}
		http.ServeContent(w, r, "desktop.txt", time.Time{}, bytes.NewReader(payload))
	}))
	defer storage.Close()
	dir := t.TempDir()
	folder := filepath.Join(dir, "Downloads")
	if err := os.Mkdir(folder, 0700); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 11; i++ {
		path := filepath.Join(folder, fmt.Sprintf("desktop-%d.txt", i))
		if err := os.WriteFile(path, payload, 0600); err != nil {
			t.Fatal(err)
		}
		if large && i == 0 {
			largeSource = path
			f, err := os.OpenFile(path, os.O_WRONLY, 0600)
			if err != nil {
				t.Fatal(err)
			}
			err = f.Truncate(731 << 20)
			f.Close()
			if err != nil {
				t.Fatal(err)
			}
		}
	}
	ca := filepath.Join(dir, "ca.pem")
	if err := os.WriteFile(ca, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: storage.Certificate().Raw}), 0600); err != nil {
		t.Fatal(err)
	}
	app := journalFixture(t, filepath.Join(dir, "profiles.db"), &journalVault{values: map[string]string{}})
	profile, err := app.SaveProfile(Profile{Name: "Desktop fixture", Region: "us-east-1", Endpoint: storage.URL, PathStyle: true, CAFile: ca}, Secret{AccessKey: "fixture", SecretKey: "fixture"})
	if err != nil {
		t.Fatal(err)
	}
	fixtureJSON, _ := json.Marshal(map[string]any{"ProfileID": profile.ID, "ConnectionIdentity": profile.Identity, "Path": dir, "Large": large})
	opts := desktopOptions(app)
	base := opts.AssetServer.Handler
	native := desktopHandler(app, func(isFolder bool) ([]desktopUploadFile, error) {
		return collectDesktopUploads(context.Background(), []string{folder}, true)
	})
	result := make(chan string, 1)
	ready := make(chan struct{}, 1)
	closed := make(chan struct{})
	opts.AssetServer.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/desktop-upload-pick" {
			native.ServeHTTP(w, r)
			return
		}
		if r.URL.Path == "/desktop-test-result" {
			var value string
			if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8192)).Decode(&value); err != nil {
				value = err.Error()
			}
			select {
			case result <- value:
				ready <- struct{}{}
			default:
			}
			w.WriteHeader(http.StatusNoContent)
			return
		}
		base.ServeHTTP(w, r)
	})
	opts.OnStartup = func(ctx context.Context) {
		go func() {
			timer := time.NewTimer(30 * time.Second)
			defer timer.Stop()
			select {
			case <-ready:
			case <-timer.C:
				select {
				case result <- "timed out waiting for frontend":
				default:
				}
			case <-closed:
				return
			}
			wailsruntime.Quit(ctx)
		}()
	}
	opts.OnDomReady = func(ctx context.Context) {
		wailsruntime.WindowExecJS(ctx, `const fixture=`+string(fixtureJSON)+`; (async()=>{
 let result='ok';
 try {
  const profiles=await api('profiles');
  if(!Array.isArray(profiles)||profiles.length!==1)throw Error('Unexpected profiles');
  const transfers=await api('transfers');
  if(!Array.isArray(transfers)||transfers.length)throw Error('Unexpected transfers');
  if(!document.getElementById('add')||getComputedStyle(document.body).fontFamily==='Times New Roman')throw Error('Frontend assets missing');
  document.getElementById('add').click();
  if(!document.getElementById('connection').open)throw Error('Connection dialog did not open');
  document.getElementById('connection').close();
  const wait=async(id)=>{
   for(let i=0;i<200;i++){
    const transfer=(await api('transfers')).find(t=>t.ID===id);
    if(transfer?.Status==='completed')return;
    if(transfer?.Status==='failed')throw Error(transfer.Error);
    await new Promise(resolve=>setTimeout(resolve,50));
   }
   throw Error('Transfer timed out');
  };
  profile=fixture.ProfileID;bucket='bucket';prefix='';
  document.getElementById('upload').disabled=false;
  document.getElementById('upload').click();
  if(!nativeUploadPicker)throw Error('Desktop native upload picker was not detected');
  await document.getElementById('upload-add-folder').onclick();
  if(document.getElementById('upload-rows').children.length!==11)throw Error('Native picker did not populate files');
  if(!document.getElementById('upload-form').checkValidity())throw Error('Upload form is invalid after folder selection');
  const uploadButton=document.getElementById('upload-confirm');
  const bounds=uploadButton.getBoundingClientRect();
  if(document.elementFromPoint(bounds.x+bounds.width/2,bounds.y+bounds.height/2)!==uploadButton)throw Error('Upload button is covered by another element');
  uploadButton.click();
  let uploads;
  for(let i=0;i<100;i++){
   uploads=(await api('transfers')).filter(t=>t.Direction==='upload');
   if(uploads.length===11)break;
   await new Promise(resolve=>setTimeout(resolve,50));
  }
  if(uploads.length!==11)throw Error('Upload button did not queue every file: '+document.getElementById('upload-error').textContent);
  for(const upload of uploads)await wait(upload.ID);
  const download=await api('download',{...fixture,Bucket:'bucket',Key:'desktop.txt',Name:'download.txt'});
  await wait(download.ID);
 }catch(e){result=e.message}
 await fetch('/desktop-test-result',{method:'POST',headers:{'Content-Type':'application/json'},body:JSON.stringify(result)});
})()`)
	}
	err = wails.Run(opts)
	close(closed)
	if err != nil {
		t.Fatal(err)
	}
	select {
	case value := <-result:
		if value != "ok" {
			t.Fatal(value)
		}
	default:
		t.Fatal("window closed before the frontend completed")
	}
	if !uploaded.Load() {
		t.Fatal("upload payload did not reach storage")
	}
	if large && !largeUploaded.Load() {
		t.Fatal("large upload bytes did not reach storage")
	}
	data, err := os.ReadFile(filepath.Join(dir, "download.txt"))
	if err != nil || !bytes.Equal(data, payload) {
		t.Fatalf("download mismatch: %q, %v", data, err)
	}
}
