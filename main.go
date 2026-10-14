package main

import (
	"context"
	"crypto/rand"
	"embed"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"time"
)

//go:embed frontend/*
var assets embed.FS

func main() {
	port := flag.Int("port", 0, "Local port (0 chooses an available port)")
	dbPath := flag.String("db", "", "SQLite path (default: ~/.harbor/profiles.db)")
	flag.Parse()
	listener, err := net.Listen("tcp4", fmt.Sprintf("127.0.0.1:%d", *port))
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	app := NewApp()
	app.configFile = *dbPath
	app.startup(context.Background())
	tokenBytes := make([]byte, 32)
	if _, err = rand.Read(tokenBytes); err != nil {
		panic(err)
	}
	token := hex.EncodeToString(tokenBytes)
	host := listener.Addr().String()
	server := &http.Server{Handler: webHandler(app, host, token), ReadHeaderTimeout: 5 * time.Second, IdleTimeout: 60 * time.Second, MaxHeaderBytes: 16384}
	fmt.Printf("Harbor S3 Browser\nOpen http://%s/#%s\nKeep this session URL private. Press Ctrl+C to stop.\n", host, token)
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt)
	go func() {
		<-stop
		app.shutdown(context.Background())
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = server.Shutdown(ctx)
	}()
	if err = server.Serve(listener); err != nil && err != http.ErrServerClosed {
		fmt.Fprintln(os.Stderr, err)
	}
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
			dir, err := os.UserCacheDir()
			if err != nil {
				http.Error(w, "Cache unavailable", 500)
				return
			}
			dir = dir + "/HarborS3"
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
			closeErr := f.Close()
			if err != nil || closeErr != nil {
				os.Remove(path)
				http.Error(w, "Upload staging failed", 400)
				return
			}
			id := a.queue("upload", profile, bucket, key, path)
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(id)
			return
		}

		r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
		var in struct {
			Profile   Profile
			Secret    Secret
			ProfileID string
			Bucket    string
			Prefix    string
			Token     string
			ID        string
			Key       string
			ETag      string
			Path      string
			Name      string
			Overwrite bool
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
		case "/api/test":
			value, err = a.TestConnection(in.ProfileID)
		case "/api/buckets":
			value, err = a.Buckets(in.ProfileID)
		case "/api/delete-file":
			value, err = a.DeleteFile(in.ProfileID, in.Bucket, in.Key, in.ETag)
		case "/api/create-folder":
			value, err = a.CreateFolder(in.ProfileID, in.Bucket, in.Prefix, in.Key)
		case "/api/browse":
			value, err = a.Browse(in.ProfileID, in.Bucket, in.Prefix, in.Token)
		case "/api/save-locations":
			value, err = saveLocations(in.Path)
		case "/api/save-download":
			value, err = a.SaveDownload(r.Context(), in.ID, in.Path, in.Name, in.Overwrite)
		case "/api/download":
			value, err = a.QueueDownload(in.ProfileID, in.Bucket, in.Key)
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
			w.WriteHeader(400)
			_ = json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
			return
		}
		_ = json.NewEncoder(w).Encode(value)
	})
}
