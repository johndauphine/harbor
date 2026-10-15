package main

import (
	"context"
	"crypto/rand"
	"embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"
)

//go:embed frontend/*
var assets embed.FS

func main() {
	noBrowser := flag.Bool("no-browser", false, "Print the session URL without opening the default browser")
	showVersion := flag.Bool("version", false, "Print build information and exit")
	port := flag.Int("port", 0, "Local port (0 chooses an available port)")
	dbPath := flag.String("db", "", "SQLite path (default: ~/.harbor/profiles.db)")
	flag.Parse()
	if *showVersion {
		fmt.Println(versionText())
		return
	}
	store, err := acquireStoreLock(*dbPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	defer store.Close()
	listener, err := net.Listen("tcp4", fmt.Sprintf("127.0.0.1:%d", *port))
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	app := NewApp()
	app.configFile = store.path
	if err = app.startup(context.Background()); err != nil {
		listener.Close()
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	tokenBytes := make([]byte, 32)
	if _, err = rand.Read(tokenBytes); err != nil {
		panic(err)
	}
	token := hex.EncodeToString(tokenBytes)
	host := listener.Addr().String()
	server := &http.Server{Handler: webHandler(app, host, token), ReadHeaderTimeout: 5 * time.Second, IdleTimeout: 60 * time.Second, MaxHeaderBytes: 16384}
	fmt.Printf("Harbor S3 Browser\nOpen http://%s/#%s\nKeep this session URL private. Press Ctrl+C to stop.\n", host, token)
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(stop)
	if !*noBrowser {
		browserCtx, cancelBrowser := context.WithCancel(context.Background())
		defer cancelBrowser()
		go func() {
			if err := openBrowser(browserCtx, "http://"+host+"/#"+token); err != nil && browserCtx.Err() == nil {
				fmt.Fprintln(os.Stderr, "Could not open the default browser. Open the session URL printed above manually.")
			}
		}()
	}
	if err = serveUntilStopped(app, server, listener, stop); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}

}

// Keep shutdown on the main goroutine: Serve returning must not exit the
// process before transfers and active requests have finished unwinding.
func serveUntilStopped(app *App, server *http.Server, listener net.Listener, stop <-chan os.Signal) error {
	served := make(chan error, 1)
	go func() { served <- server.Serve(listener) }()
	var serveErr error
	select {
	case <-stop:
	case serveErr = <-served:
	}
	app.beginShutdown()
	requestCtx, cancelRequests := context.WithTimeout(context.Background(), 5*time.Second)
	if err := server.Shutdown(requestCtx); err != nil {
		_ = server.Close()
	}
	cancelRequests()
	transferCtx, cancelTransfers := context.WithTimeout(context.Background(), 25*time.Second)
	defer cancelTransfers()
	shutdownErr := app.shutdown(transferCtx)
	if serveErr == http.ErrServerClosed {
		serveErr = nil
	}
	return errors.Join(serveErr, shutdownErr)
}

func webHandler(a *App, host, token string) http.Handler {
	files, _ := fs.Sub(assets, "frontend")
	static := http.FileServer(http.FS(files))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Security-Policy", "default-src 'self'; script-src 'self'; style-src 'self'; connect-src 'self'; frame-ancestors 'none'; base-uri 'none'; form-action 'self'")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("Cache-Control", "no-store")
		if r.Host != host {
			http.Error(w, "Invalid host", 403)
			return
		}
		if !strings.HasPrefix(r.URL.Path, "/api/") {
			static.ServeHTTP(w, r)
			return
		}
		if r.Header.Get("Authorization") != "Bearer "+token {
			http.Error(w, "Session required", 401)
			return
		}
		if origin := r.Header.Get("Origin"); origin != "" && origin != "http://"+host {
			http.Error(w, "Invalid origin", 403)
			return
		}
		if r.Method != "POST" {
			http.Error(w, "POST required", 405)
			return
		}
		operationCtx, done, operationErr := a.beginOperation(r.Context())
		if operationErr != nil {
			http.Error(w, operationErr.Error(), http.StatusServiceUnavailable)
			return
		}
		defer done()
		r = r.WithContext(operationCtx)
		body := r.Body
		stopBodyClose := context.AfterFunc(operationCtx, func() { _ = body.Close() })
		defer stopBodyClose()
		if r.URL.Path == "/api/upload" {
			r.Body = http.MaxBytesReader(w, r.Body, 5<<30)
			profile, bucket, key := r.URL.Query().Get("profile"), r.URL.Query().Get("bucket"), r.URL.Query().Get("key")
			if bucket == "" || key == "" {
				http.Error(w, "Bucket and key required", 400)
				return
			}
			if _, err := a.client(r.Context(), profile); err != nil {
				http.Error(w, "Connection unavailable", 400)
				return
			}
			dir, err := a.transferCacheDir()
			if err != nil {
				http.Error(w, "Cache unavailable", 500)
				return
			}
			if err = os.MkdirAll(dir, 0700); err != nil {
				http.Error(w, "Cache unavailable", 500)
				return
			}
			f, err := os.CreateTemp(dir, "upload-*")
			if err != nil {
				http.Error(w, "Cache unavailable", 500)
				return
			}
			path := f.Name()
			_, err = io.Copy(f, r.Body)
			if err == nil {
				err = f.Sync()
			}
			closeErr := f.Close()
			if err != nil || closeErr != nil {
				os.Remove(path)
				http.Error(w, "Upload staging failed", 400)
				return
			}
			id, queueErr := a.queueTransfer("upload", profile, bucket, key, path)
			if queueErr != nil {
				os.Remove(path)
				http.Error(w, queueErr.Error(), http.StatusServiceUnavailable)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(id)
			return
		}

		r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
		var in struct {
			Profile      Profile
			Secret       Secret
			ProfileID    string
			Bucket       string
			Prefix       string
			Token        string
			ID           string
			Key          string
			ETag         string
			Path         string
			Name         string
			Confirmation string
			Kind         string
			PlanID       string
			Overwrite    bool
		}
		if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
			http.Error(w, "Invalid request", 400)
			return
		}
		var value interface{}
		var err error
		switch r.URL.Path {
		case "/api/profiles":
			value = a.Profiles()
		case "/api/save":
			value, err = a.SaveProfile(in.Profile, in.Secret)
		case "/api/preview-delete":
			value, err = a.PreviewDelete(r.Context(), in.ProfileID, in.Bucket, in.Prefix, in.Kind)
		case "/api/execute-delete":
			value, err = a.ExecuteDelete(r.Context(), in.PlanID, in.Confirmation)
		case "/api/delete-bucket":
			if in.Bucket == "" || in.Confirmation != in.Bucket {
				err = fmt.Errorf("Type the exact bucket name to confirm deletion")
			} else {
				err = a.DeleteBucket(in.ProfileID, in.Bucket)
			}
			value = "Bucket deleted"
		case "/api/delete-profile":
			err = a.DeleteProfile(in.ProfileID)
			value = "Connection deleted"
		case "/api/test":
			value, err = a.TestConnection(in.ProfileID)
		case "/api/buckets":
			value, err = a.Buckets(in.ProfileID)
		case "/api/delete-file":
			value, err = a.DeleteFile(in.ProfileID, in.Bucket, in.Key, in.ETag)
		case "/api/create-bucket":
			value, err = a.CreateBucket(in.ProfileID, in.Bucket)
		case "/api/create-folder":
			value, err = a.CreateFolder(in.ProfileID, in.Bucket, in.Prefix, in.Key)
		case "/api/browse":
			value, err = a.Browse(in.ProfileID, in.Bucket, in.Prefix, in.Token)
		case "/api/upload-locations":
			value, err = uploadLocations(in.Path)
		case "/api/upload-local":
			value, err = a.QueueLocalUpload(r.Context(), in.ProfileID, in.Bucket, in.Key, in.Path)
		case "/api/save-locations":
			value, err = saveLocations(in.Path)
		case "/api/save-download":
			value, err = a.SaveDownload(r.Context(), in.ID, in.Path, in.Name, in.Overwrite)
		case "/api/download":
			value, err = a.QueueDownloadTo(in.ProfileID, in.Bucket, in.Key, in.Path, in.Name, in.Overwrite)
		case "/api/transfers":
			value = a.Transfers()
		case "/api/cancel":
			a.CancelTransfer(in.ID)
		case "/api/retry":
			err = a.RetryTransfer(in.ID)
		default:
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		if err != nil {
			var cleanup *credentialCleanupError
			if errors.As(err, &cleanup) {
				// The settings change committed; report pending cleanup without
				// inviting the browser to repeat a successful mutation.
				_ = json.NewEncoder(w).Encode(map[string]interface{}{"Value": value, "Warning": cleanup.Error()})
				return
			}
			w.WriteHeader(400)
			_ = json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
			return
		}
		_ = json.NewEncoder(w).Encode(value)
	})
}
