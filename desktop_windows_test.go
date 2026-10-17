//go:build desktop && windows

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"encoding/pem"
	"io"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"os"
	"path/filepath"
	"runtime"
	"strings"
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
	var uploaded atomic.Bool
	storage := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "PUT" {
			var reader io.Reader = r.Body
			if strings.Contains(r.Header.Get("Content-Encoding"), "aws-chunked") {
				reader = httputil.NewChunkedReader(reader)
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
	ca := filepath.Join(dir, "ca.pem")
	if err := os.WriteFile(ca, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: storage.Certificate().Raw}), 0600); err != nil {
		t.Fatal(err)
	}
	app := journalFixture(t, filepath.Join(dir, "profiles.db"), &journalVault{values: map[string]string{}})
	profile, err := app.SaveProfile(Profile{Name: "Desktop fixture", Region: "us-east-1", Endpoint: storage.URL, PathStyle: true, CAFile: ca}, Secret{AccessKey: "fixture", SecretKey: "fixture"})
	if err != nil {
		t.Fatal(err)
	}
	fixtureJSON, _ := json.Marshal(map[string]string{"ProfileID": profile.ID, "Path": dir})
	opts := desktopOptions(app)
	base := opts.AssetServer.Handler
	result := make(chan string, 1)
	ready := make(chan struct{}, 1)
	closed := make(chan struct{})
	opts.AssetServer.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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
  const response=await fetch('/api/upload?'+new URLSearchParams({profile:fixture.ProfileID,bucket:'bucket',key:'desktop.txt'}),{method:'POST',body:new Blob(['desktop-fixture-payload'])});
  if(!response.ok)throw Error('Upload staging failed');
  await wait(await response.json());
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
	data, err := os.ReadFile(filepath.Join(dir, "download.txt"))
	if err != nil || !bytes.Equal(data, payload) {
		t.Fatalf("download mismatch: %q, %v", data, err)
	}
}
