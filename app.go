package main

import (
	"context"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/feature/s3/manager"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/zalando/go-keyring"
	"io"
	"log"
	_ "modernc.org/sqlite"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

type Profile struct {
	credentialID string
	ID           string
	Name         string
	Endpoint     string
	Region       string
	PathStyle    bool
	CAFile       string
	AccessKey    string `json:"-"`
	SecretKey    string `json:"-"`
	SessionToken string `json:"-"`
}
type Secret struct {
	AccessKey    string
	SecretKey    string
	SessionToken string
}
type Object struct {
	Key          string
	Size         int64
	Modified     string
	Folder       bool
	ETag         string
	StorageClass string
}
type Page struct {
	Objects []Object
	Next    string
}
type downloadTarget struct {
	Path      string
	Overwrite bool
}

type Transfer struct {
	attempt     uint64
	destination *downloadTarget
	ID          string
	Direction   string
	Key         string
	Path        string
	ProfileID   string
	Bucket      string
	Done        int64
	Total       int64
	Status      string
	Error       string
	CreatedAt   time.Time
	SavedAt     time.Time
	SavedPath   string
	SaveError   string
	cancel      context.CancelFunc
}
type App struct {
	ctx           context.Context
	appCancel     context.CancelFunc
	mu            sync.Mutex
	profileMu     sync.Mutex
	closing       bool
	workers       sync.WaitGroup
	operations    map[string]context.CancelFunc
	shutdownDone  chan struct{}
	profiles      []Profile
	transfers     map[string]*Transfer
	transferOrder []string
	saveMu        sync.Mutex
	configFile    string
	secretGet     func(string) (string, error)
	secretSet     func(string, string) error
	secretDelete  func(string) error
	profileCommit func(*sql.Tx) error
}

func NewApp() *App {
	return &App{secretDelete: func(id string) error { return keyring.Delete("HarborS3", id) }, transfers: map[string]*Transfer{}, secretGet: func(id string) (string, error) { return keyring.Get("HarborS3", id) }, secretSet: func(id, v string) error { return keyring.Set("HarborS3", id, v) }}
}
func (a *App) openStore() (*sql.DB, error) {
	if a.configFile == "" {
		return nil, errors.New("Configuration store unavailable")
	}
	if err := os.MkdirAll(filepath.Dir(a.configFile), 0700); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(a.configFile, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	f.Close()
	db, err := sql.Open("sqlite", a.configFile)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	if _, err = db.Exec("PRAGMA busy_timeout=5000; CREATE TABLE IF NOT EXISTS profiles(id TEXT PRIMARY KEY,metadata TEXT NOT NULL); CREATE TABLE IF NOT EXISTS profile_credentials(profile_id TEXT PRIMARY KEY,credential_id TEXT NOT NULL); CREATE TABLE IF NOT EXISTS credential_cleanup(id TEXT PRIMARY KEY)"); err != nil {
		db.Close()
		return nil, err
	}
	return db, nil
}
func (a *App) startup(ctx context.Context) (startupErr error) {
	a.ctx, a.appCancel = context.WithCancel(ctx)
	defer func() {
		if startupErr != nil {
			a.appCancel()
		}
	}()
	if a.configFile == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return fmt.Errorf("Find configuration directory: %w", err)
		}
		a.configFile = filepath.Join(home, ".harbor", "profiles.db")
	}
	db, err := a.openStore()
	if err != nil {
		return fmt.Errorf("Open configuration database: %w", err)
	}
	defer db.Close()
	if err = a.bindStoreLocation(db); err != nil {
		return err
	}
	rows, err := db.Query("SELECT p.id, p.metadata, COALESCE(c.credential_id,p.id) FROM profiles p LEFT JOIN profile_credentials c ON c.profile_id=p.id ORDER BY p.id")
	if err != nil {
		return fmt.Errorf("Read saved connections: %w", err)
	}
	defer rows.Close()
	var loaded []Profile
	for rows.Next() {
		var storedID, raw, ref string
		if err = rows.Scan(&storedID, &raw, &ref); err != nil {
			return fmt.Errorf("Read saved connection: %w", err)
		}
		var p Profile
		if err = json.Unmarshal([]byte(raw), &p); err != nil {
			return errors.New("Configuration contains invalid connection metadata; restore a valid configuration backup or use a separate database")
		}
		if p.ID == "" || p.ID != storedID || ref == "" || validate(p) != nil {
			return errors.New("Configuration contains an invalid saved connection; restore a valid configuration backup or use a separate database")
		}
		p.credentialID = ref
		loaded = append(loaded, p)
	}
	if err = rows.Err(); err != nil {
		return fmt.Errorf("Read saved connections: %w", err)
	}
	if err = rows.Close(); err != nil {
		return fmt.Errorf("Close saved connection query: %w", err)
	}
	a.profileMu.Lock()
	defer a.profileMu.Unlock()
	a.profiles = loaded
	if err = a.cleanupCredentials(db); err != nil {
		log.Print("Unused credential cleanup remains pending; unlock the credential vault and restart Harbor to retry")
	}
	return nil
}

var errShuttingDown = errors.New("Application is shutting down")

// beginOperation registers synchronous disk/API work before shutdown can wait.
func (a *App) beginOperation(ctx context.Context) (context.Context, func(), error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.closing {
		return nil, nil, errShuttingDown
	}
	ctx, cancel := context.WithCancel(ctx)
	if a.operations == nil {
		a.operations = make(map[string]context.CancelFunc)
	}
	operationID := id()
	a.operations[operationID] = cancel
	a.workers.Add(1)
	return ctx, func() {
		cancel()
		a.mu.Lock()
		delete(a.operations, operationID)
		a.mu.Unlock()
		a.workers.Done()
	}, nil
}

func (a *App) beginShutdown() <-chan struct{} {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.shutdownDone != nil {
		return a.shutdownDone
	}
	a.closing = true
	if a.appCancel != nil {
		a.appCancel()
	}
	a.shutdownDone = make(chan struct{})
	for _, cancel := range a.operations {
		cancel()
	}
	for _, t := range a.transfers {
		if t.cancel != nil {
			t.cancel()
		} else if t.Status == "queued" {
			t.Status = "cancelled"
		}
	}
	// No further Add is possible: admission and closing share a.mu.
	go func() { a.workers.Wait(); close(a.shutdownDone) }()
	return a.shutdownDone
}

func (a *App) shutdown(ctx context.Context) error {
	done := a.beginShutdown()
	select {
	case <-done:
	case <-ctx.Done():
		// Never remove files while an operation might still be using them.
		return ctx.Err()
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	var cleanupErr error
	for _, t := range a.transfers {
		if t.Path != "" {
			if err := os.Remove(t.Path); err != nil && !os.IsNotExist(err) {
				cleanupErr = errors.Join(cleanupErr, err)
			}
		}
	}
	return cleanupErr
}
func id() string { b := make([]byte, 16); _, _ = rand.Read(b); return hex.EncodeToString(b) }
func (a *App) Profiles() []Profile {
	a.profileMu.Lock()
	defer a.profileMu.Unlock()
	return append([]Profile{}, a.profiles...)
}
func validate(p Profile) error {
	if strings.TrimSpace(p.Name) == "" || strings.TrimSpace(p.Region) == "" {
		return errors.New("Name and region are required")
	}
	if p.Endpoint != "" {
		u, e := url.Parse(p.Endpoint)
		if e != nil || u.Host == "" || u.Scheme != "https" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
			return errors.New("Endpoint must be an HTTPS URL without credentials, query or fragment")
		}
	}
	return nil
}

// credentialCleanupError means the metadata operation committed successfully,
// but unused vault entries remain queued for cleanup. No active keys are removed.
type credentialCleanupError struct{ Operation string }

func (e *credentialCleanupError) Error() string {
	return "Connection " + e.Operation + "; unused credential cleanup is pending. Unlock the credential vault and restart Harbor to retry cleanup."
}

func credentialID(p Profile) string {
	if p.credentialID != "" {
		return p.credentialID
	}
	return p.ID // Legacy profiles used the profile ID as the vault key.
}

// cleanupCredentials never removes a reference still used by committed metadata.
// The durable queue is written before staging credentials, so interrupted saves
// and failed vault deletions can be recovered without storing any secrets in SQLite.
func (a *App) cleanupCredentials(db *sql.DB) error {
	rows, err := db.Query(`SELECT q.id FROM credential_cleanup q
		WHERE NOT EXISTS (SELECT 1 FROM profile_credentials c WHERE c.credential_id=q.id)
		AND NOT EXISTS (SELECT 1 FROM profiles p WHERE p.id=q.id
		 AND NOT EXISTS (SELECT 1 FROM profile_credentials c WHERE c.profile_id=p.id))`)
	if err != nil {
		return err
	}
	var ids []string
	for rows.Next() {
		var id string
		if err = rows.Scan(&id); err != nil {
			rows.Close()
			return err
		}
		ids = append(ids, id)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	var failures []error
	for _, id := range ids {
		if err = a.secretDelete(id); err != nil && !errors.Is(err, keyring.ErrNotFound) {
			failures = append(failures, errors.New("Credential vault cleanup unavailable"))
			continue
		}
		if _, err = db.Exec("DELETE FROM credential_cleanup WHERE id=?", id); err != nil {
			failures = append(failures, err)
		}
	}
	return errors.Join(failures...)
}

// RetryCredentialCleanup retries removal of unreferenced credentials only.
func (a *App) RetryCredentialCleanup() error {
	a.profileMu.Lock()
	defer a.profileMu.Unlock()
	db, err := a.openStore()
	if err != nil {
		return err
	}
	defer db.Close()
	if err = a.cleanupCredentials(db); err != nil {
		return &credentialCleanupError{Operation: "updated"}
	}
	return nil
}

func (a *App) commitProfile(tx *sql.Tx) error {
	if a.profileCommit != nil {
		return a.profileCommit(tx)
	}
	return tx.Commit()
}

func (a *App) SaveProfile(p Profile, secret Secret) (Profile, error) {
	if err := validate(p); err != nil {
		return Profile{}, err
	}
	a.profileMu.Lock()
	defer a.profileMu.Unlock()
	var previous Profile
	for _, saved := range a.profiles {
		if saved.ID == p.ID {
			previous = saved
			break
		}
	}
	if p.ID != "" && previous.ID == "" {
		return Profile{}, errors.New("Connection not found")
	}
	if p.ID == "" {
		p.ID = id()
	}
	p.credentialID = credentialID(previous) // Never trust a caller's credential reference.
	replace := secret.AccessKey != "" || secret.SecretKey != "" || secret.SessionToken != ""
	var saved Secret
	if previous.ID != "" && (!replace || secret.AccessKey == "" || secret.SecretKey == "") {
		raw, err := a.secretGet(credentialID(previous))
		if err != nil {
			return Profile{}, fmt.Errorf("Credential vault: %w", err)
		}
		if err = json.Unmarshal([]byte(raw), &saved); err != nil {
			return Profile{}, errors.New("Unable to read saved credentials")
		}
	}
	if replace {
		if secret.AccessKey == "" {
			secret.AccessKey = saved.AccessKey
		}
		if secret.SecretKey == "" {
			secret.SecretKey = saved.SecretKey
		}
		if secret.AccessKey == "" || secret.SecretKey == "" {
			return Profile{}, errors.New("Both access key and secret key are required for a new connection")
		}
		p.credentialID = id()
	} else if previous.ID == "" || saved.AccessKey == "" || saved.SecretKey == "" {
		return Profile{}, errors.New("Enter credentials for this connection")
	}
	db, err := a.openStore()
	if err != nil {
		return Profile{}, err
	}
	defer db.Close()
	// Write-ahead cleanup intent survives a crash before or during vault staging.
	if replace {
		if _, err = db.Exec("INSERT INTO credential_cleanup(id) VALUES(?)", p.credentialID); err != nil {
			return Profile{}, err
		}
	}
	tx, err := db.Begin()
	if err != nil {
		return Profile{}, err
	}
	defer tx.Rollback()
	b, _ := json.Marshal(p)
	if _, err = tx.Exec("INSERT INTO profiles(id,metadata) VALUES(?,?) ON CONFLICT(id) DO UPDATE SET metadata=excluded.metadata", p.ID, string(b)); err == nil {
		_, err = tx.Exec("INSERT INTO profile_credentials(profile_id,credential_id) VALUES(?,?) ON CONFLICT(profile_id) DO UPDATE SET credential_id=excluded.credential_id", p.ID, p.credentialID)
	}
	if err == nil && replace && previous.ID != "" {
		_, err = tx.Exec("INSERT OR IGNORE INTO credential_cleanup(id) VALUES(?)", credentialID(previous))
	}
	if err == nil && replace {
		b, _ = json.Marshal(secret)
		err = a.secretSet(p.credentialID, string(b))
		if err != nil {
			err = fmt.Errorf("Credential vault: %w", err)
		}
	}
	if err == nil {
		err = a.commitProfile(tx)
	}
	if err != nil {
		tx.Rollback()
		if cleanupErr := a.cleanupCredentials(db); cleanupErr != nil {
			return Profile{}, fmt.Errorf("Connection was not saved; existing credentials are unchanged. Unused credential cleanup is pending: %w", err)
		}
		return Profile{}, err
	}
	found := false
	for i := range a.profiles {
		if a.profiles[i].ID == p.ID {
			a.profiles[i] = p
			found = true
			break
		}
	}
	if !found {
		a.profiles = append(a.profiles, p)
	}
	if err = a.cleanupCredentials(db); err != nil {
		return p, &credentialCleanupError{Operation: "saved"}
	}
	return p, nil
}

var (
	errS3HTTPSRequired = errors.New("S3 requests require HTTPS")
	errS3Redirect      = errors.New("S3 endpoint redirects are not allowed; configure the final HTTPS endpoint")
)

type httpsOnlyTransport struct {
	*http.Transport
}

func (t httpsOnlyTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	// Check the resolved URL too: AWS environment/config can override the endpoint.
	if r.URL == nil || r.URL.Scheme != "https" {
		return nil, errS3HTTPSRequired
	}
	return t.Transport.RoundTrip(r)
}

func (a *App) client(ctx context.Context, profileID string) (*s3.Client, error) {
	a.profileMu.Lock()
	var p Profile
	for _, v := range a.profiles {
		if v.ID == profileID {
			p = v
		}
	}
	if p.ID == "" {
		a.profileMu.Unlock()
		return nil, errors.New("Connection not found")
	}
	raw, err := a.secretGet(credentialID(p))
	a.profileMu.Unlock()
	if err != nil {
		return nil, fmt.Errorf("Credential vault: %w", err)
	}
	var s Secret
	if err = json.Unmarshal([]byte(raw), &s); err != nil {
		return nil, err
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	if p.CAFile != "" {
		pem, e := os.ReadFile(p.CAFile)
		if e != nil {
			return nil, e
		}
		pool, e := x509.SystemCertPool()
		if e != nil {
			pool = x509.NewCertPool()
		}
		if !pool.AppendCertsFromPEM(pem) {
			return nil, errors.New("Custom CA file contains no certificates")
		}
		transport.TLSClientConfig = &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}
	}
	httpClient := &http.Client{
		Transport: httpsOnlyTransport{Transport: transport},
		// Redirects can forward signed headers and session tokens to another origin.
		CheckRedirect: func(*http.Request, []*http.Request) error { return errS3Redirect },
	}
	cfg, e := config.LoadDefaultConfig(ctx, config.WithRegion(p.Region), config.WithCredentialsProvider(credentials.NewStaticCredentialsProvider(s.AccessKey, s.SecretKey, s.SessionToken)), config.WithHTTPClient(httpClient))
	if e != nil {
		return nil, e
	}
	return s3.NewFromConfig(cfg, func(o *s3.Options) {
		// Custom endpoints come only from the saved Harbor connection, never ambient AWS configuration.
		o.BaseEndpoint = nil
		o.UsePathStyle = p.PathStyle
		if p.Endpoint != "" {
			o.BaseEndpoint = aws.String(p.Endpoint)
		}
	}), nil
}
func (a *App) Buckets(profileID string) ([]string, error) {
	ctx, cancel := context.WithTimeout(a.ctx, 30*time.Second)
	defer cancel()
	c, e := a.client(ctx, profileID)
	if e != nil {
		return nil, e
	}
	out := []string{}
	var token *string
	for {
		v, e := c.ListBuckets(ctx, &s3.ListBucketsInput{ContinuationToken: token})
		if e != nil {
			return nil, e
		}
		for _, b := range v.Buckets {
			out = append(out, aws.ToString(b.Name))
		}
		token = v.ContinuationToken
		if token == nil || *token == "" {
			break
		}
	}
	return out, nil
}
func (a *App) TestConnection(profileID string) (string, error) {
	b, e := a.Buckets(profileID)
	if e != nil {
		return "", e
	}
	return fmt.Sprintf("Connected. %d buckets available.", len(b)), nil
}
func (a *App) Browse(profileID, bucket, prefix, token string) (Page, error) {
	ctx, cancel := context.WithTimeout(a.ctx, 30*time.Second)
	defer cancel()
	c, e := a.client(ctx, profileID)
	if e != nil {
		return Page{}, e
	}
	in := &s3.ListObjectsV2Input{Bucket: aws.String(bucket), Prefix: aws.String(prefix), Delimiter: aws.String("/"), MaxKeys: aws.Int32(500)}
	if token != "" {
		in.ContinuationToken = aws.String(token)
	}
	v, e := c.ListObjectsV2(ctx, in)
	if e != nil {
		return Page{}, e
	}
	page := Page{Objects: []Object{}, Next: aws.ToString(v.NextContinuationToken)}
	for _, f := range v.CommonPrefixes {
		page.Objects = append(page.Objects, Object{Key: aws.ToString(f.Prefix), Folder: true})
	}
	for _, f := range v.Contents {
		key := aws.ToString(f.Key)
		if key == prefix && strings.HasSuffix(key, "/") {
			continue
		}
		modified := ""
		if f.LastModified != nil {
			modified = f.LastModified.Format(time.RFC3339)
		}
		page.Objects = append(page.Objects, Object{Key: key, Size: aws.ToInt64(f.Size), Modified: modified, ETag: aws.ToString(f.ETag), StorageClass: string(f.StorageClass)})
	}
	return page, nil
}
func (a *App) QueueDownload(profile, bucket, key string, target ...downloadTarget) (string, error) {
	if bucket == "" || key == "" {
		return "", errors.New("Bucket and key required")
	}
	if _, e := a.client(a.ctx, profile); e != nil {
		return "", e
	}
	dir, e := os.UserCacheDir()
	if e != nil {
		return "", e
	}
	dir = filepath.Join(dir, "HarborS3")
	if e = os.MkdirAll(dir, 0700); e != nil {
		return "", e
	}
	transferID := a.queue("download", profile, bucket, key, filepath.Join(dir, "download-"+id()), target...)
	if transferID == "" {
		return "", errShuttingDown
	}
	return transferID, nil
}
func (a *App) queue(direction, profile, bucket, key, path string, target ...downloadTarget) string {
	t := &Transfer{attempt: 1, ID: id(), Direction: direction, ProfileID: profile, Bucket: bucket, Key: key, Path: path, Status: "queued", CreatedAt: time.Now()}
	if len(target) > 0 {
		t.destination = &target[0]
	}
	a.mu.Lock()
	if a.closing {
		a.mu.Unlock()
		return ""
	}
	a.transfers[t.ID] = t
	a.transferOrder = append(a.transferOrder, t.ID)
	a.workers.Add(1)
	a.mu.Unlock()
	go func() {
		defer a.workers.Done()
		a.run(t.ID, 1)
	}()
	return t.ID
}
func (a *App) Transfers() []Transfer {
	a.mu.Lock()
	defer a.mu.Unlock()
	out := make([]Transfer, 0, len(a.transferOrder))
	for _, id := range a.transferOrder {
		if t := a.transfers[id]; t != nil {
			out = append(out, *t)
		}
	}
	return out
}
func (a *App) CancelTransfer(id string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if t := a.transfers[id]; t != nil {
		if t.cancel != nil {
			t.cancel()
		} else if t.Status == "queued" {
			t.Status = "cancelled"
		}
	}
}
func (a *App) RetryTransfer(id string) error {
	a.mu.Lock()
	if a.closing {
		a.mu.Unlock()
		return errors.New("Application is shutting down")
	}
	t := a.transfers[id]
	if t == nil || (t.Status != "failed" && t.Status != "cancelled") {
		a.mu.Unlock()
		return errors.New("Only failed or cancelled transfers can be retried")
	}
	t.attempt++
	attempt := t.attempt
	t.Status = "queued"
	t.Done = 0
	t.Error = ""
	a.workers.Add(1)
	a.mu.Unlock()
	go func() {
		defer a.workers.Done()
		a.run(id, attempt)
	}()
	return nil
}

type progressReader struct {
	io.Reader
	update func(int64)
}

func (p *progressReader) Read(b []byte) (int, error) {
	n, e := p.Reader.Read(b)
	p.update(int64(n))
	return n, e
}
func (a *App) run(id string, attempt uint64) {
	ctx, cancel := context.WithCancel(a.ctx)
	defer cancel()
	a.mu.Lock()
	t := a.transfers[id]
	// A delayed worker from a cancelled queue entry must not claim its retry.
	if t == nil || t.attempt != attempt || t.Status != "queued" {
		a.mu.Unlock()
		return
	}
	t.cancel = cancel
	t.Status = "running"
	spec := *t
	a.mu.Unlock()
	err := a.execute(ctx, spec)
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.transfers[id] != t || t.attempt != attempt {
		return
	}
	t.cancel = nil
	if err != nil {
		t.Status = "failed"
		t.Error = err.Error()
		var cleanupErr *multipartCleanupError
		if ctx.Err() != nil && !errors.As(err, &cleanupErr) {
			t.Status = "cancelled"
			t.Error = ""
		}
	} else {
		t.Status = "completed"
		t.Done = t.Total
		if t.destination != nil {
			t.SavedPath = t.destination.Path
			t.SavedAt = time.Now()
			t.SaveError = ""
		}
		if t.Direction == "upload" {
			_ = os.Remove(t.Path)
		}
	}
}

// Multipart cleanup must still reach S3 after the transfer context is cancelled.
// The SDK otherwise reuses the cancelled context and silently leaves remote parts.
type cleanupUploadClient struct {
	*s3.Client
	abortErr error
}
type multipartCleanupError struct{}

func (*multipartCleanupError) Error() string {
	return "Upload stopped, but remote multipart cleanup failed. Incomplete upload parts may remain; check the bucket’s incomplete uploads before retrying."
}

func (c *cleanupUploadClient) AbortMultipartUpload(ctx context.Context, in *s3.AbortMultipartUploadInput, opts ...func(*s3.Options)) (*s3.AbortMultipartUploadOutput, error) {
	cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	out, err := c.Client.AbortMultipartUpload(cleanupCtx, in, opts...)
	c.abortErr = err
	return out, err
}

func (a *App) execute(ctx context.Context, t Transfer) error {
	c, e := a.client(ctx, t.ProfileID)
	if e != nil {
		return e
	}
	update := func(n int64) {
		a.mu.Lock()
		defer a.mu.Unlock()
		if current := a.transfers[t.ID]; current != nil && current.attempt == t.attempt && current.Status == "running" {
			current.Done += n
		}
	}
	total := func(n int64) {
		a.mu.Lock()
		defer a.mu.Unlock()
		if current := a.transfers[t.ID]; current != nil && current.attempt == t.attempt && current.Status == "running" {
			current.Total = n
		}
	}
	if t.Direction == "upload" {
		f, e := os.Open(t.Path)
		if e != nil {
			return e
		}
		defer f.Close()
		st, e := f.Stat()
		if e != nil {
			return e
		}
		total(st.Size())
		uploadClient := &cleanupUploadClient{Client: c}
		_, e = manager.NewUploader(uploadClient).Upload(ctx, &s3.PutObjectInput{Bucket: aws.String(t.Bucket), Key: aws.String(t.Key), Body: &progressReader{Reader: f, update: update}})
		if uploadClient.abortErr != nil {
			return &multipartCleanupError{}
		}
		return e
	}
	v, e := c.GetObject(ctx, &s3.GetObjectInput{Bucket: aws.String(t.Bucket), Key: aws.String(t.Key)})
	if e != nil {
		return e
	}
	defer v.Body.Close()
	total(aws.ToInt64(v.ContentLength))
	directory := filepath.Dir(t.Path)
	if t.destination != nil {
		directory = filepath.Dir(t.destination.Path)
	}
	f, e := os.CreateTemp(directory, ".harbor-download-*")
	if e != nil {
		return e
	}
	tmp := f.Name()
	defer os.Remove(tmp)
	_, e = io.Copy(f, &progressReader{Reader: v.Body, update: update})
	if e == nil {
		e = f.Sync()
	}
	closeErr := f.Close()
	if e != nil {
		return e
	}
	if closeErr != nil {
		return closeErr
	}
	if e = ctx.Err(); e != nil {
		return e
	}
	if t.destination != nil {
		if t.destination.Overwrite {
			return os.Rename(tmp, t.destination.Path)
		}
		return os.Link(tmp, t.destination.Path)
	}
	return os.Rename(tmp, t.Path)
}

// CreateFolder writes an S3 folder marker without replacing an existing object.
func (a *App) CreateFolder(profile, bucket, prefix, name string) (string, error) {
	name = strings.TrimSpace(name)
	if bucket == "" {
		return "", errors.New("Choose a bucket first")
	}
	if name == "" || name == "." || name == ".." || strings.ContainsAny(name, "/\\") || strings.ContainsAny(name, "\r\n\t") {
		return "", errors.New("Enter a folder name without slashes or control characters")
	}
	if prefix != "" && !strings.HasSuffix(prefix, "/") {
		return "", errors.New("The current folder path must end with a slash")
	}
	key := prefix + name + "/"
	if len(key) > 1024 {
		return "", errors.New("Folder path is too long")
	}
	ctx, cancel := context.WithTimeout(a.ctx, 30*time.Second)
	defer cancel()
	c, err := a.client(ctx, profile)
	if err != nil {
		return "", err
	}
	_, err = c.PutObject(ctx, &s3.PutObjectInput{Bucket: aws.String(bucket), Key: aws.String(key), Body: strings.NewReader(""), ContentLength: aws.Int64(0), IfNoneMatch: aws.String("*")})
	if err != nil {
		return "", fmt.Errorf("Could not create folder (it may already exist): %w", err)
	}
	return key, nil
}

// DeleteFile deletes exactly one selected object, never a prefix or bucket.
func (a *App) DeleteFile(profile, bucket, key, etag string) (string, error) {
	if bucket == "" || key == "" || strings.HasSuffix(key, "/") {
		return "", errors.New("Select a file; folder deletion is not supported")
	}
	if etag == "" || etag == "*" {
		return "", errors.New("Refresh the listing and select the file again")
	}
	ctx, cancel := context.WithTimeout(a.ctx, 30*time.Second)
	defer cancel()
	c, err := a.client(ctx, profile)
	if err != nil {
		return "", err
	}
	current, err := c.HeadObject(ctx, &s3.HeadObjectInput{Bucket: aws.String(bucket), Key: aws.String(key), IfMatch: aws.String(etag)})
	if err != nil {
		return "", fmt.Errorf("Could not verify selected file; refresh before deleting: %w", err)
	}
	if aws.ToString(current.ETag) != etag {
		return "", errors.New("The file changed after selection; refresh before deleting")
	}
	_, err = c.DeleteObject(ctx, &s3.DeleteObjectInput{Bucket: aws.String(bucket), Key: aws.String(key), IfMatch: aws.String(etag)})
	if err != nil {
		return "", fmt.Errorf("Delete failed; refresh the listing before trying again: %w", err)
	}
	return key, nil
}

// DeleteProfile removes only local metadata and vault credentials, never remote objects.
func (a *App) DeleteProfile(profileID string) error {
	a.profileMu.Lock()
	defer a.profileMu.Unlock()
	index := -1
	for i, p := range a.profiles {
		if p.ID == profileID {
			index = i
			break
		}
	}
	if index < 0 {
		return errors.New("Connection not found")
	}
	a.mu.Lock()
	for _, t := range a.transfers {
		if t.ProfileID == profileID && (t.Status == "running" || t.Status == "queued") {
			a.mu.Unlock()
			return errors.New("Wait for this connection’s transfers to finish or cancel them before deleting it")
		}
	}
	a.mu.Unlock()
	db, err := a.openStore()
	if err != nil {
		return err
	}
	defer db.Close()
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err = tx.Exec("DELETE FROM profiles WHERE id=?", profileID); err != nil {
		return err
	}
	ref := credentialID(a.profiles[index])
	if _, err = tx.Exec("DELETE FROM profile_credentials WHERE profile_id=?", profileID); err != nil {
		return err
	}
	if _, err = tx.Exec("INSERT OR IGNORE INTO credential_cleanup(id) VALUES(?)", ref); err != nil {
		return err
	}
	if err = a.commitProfile(tx); err != nil {
		return err
	}
	a.profiles = append(a.profiles[:index], a.profiles[index+1:]...)
	if err = a.cleanupCredentials(db); err != nil {
		return &credentialCleanupError{Operation: "deleted"}
	}
	return nil
}

func (a *App) DeleteBucket(profileID, bucket string) error {
	if bucket == "" {
		return errors.New("Choose a bucket first")
	}
	a.mu.Lock()
	for _, t := range a.transfers {
		if t.ProfileID == profileID && t.Bucket == bucket && (t.Status == "running" || t.Status == "queued") {
			a.mu.Unlock()
			return errors.New("Wait for this bucket’s transfers to finish or cancel them first")
		}
	}
	a.mu.Unlock()
	ctx, cancel := context.WithTimeout(a.ctx, 30*time.Second)
	defer cancel()
	c, err := a.client(ctx, profileID)
	if err != nil {
		return err
	}
	// DeleteBucket itself rejects any remaining objects, versions or delete markers.
	// Never empty a bucket automatically.
	_, err = c.DeleteBucket(ctx, &s3.DeleteBucketInput{Bucket: aws.String(bucket)})
	if err != nil {
		return fmt.Errorf("Could not delete bucket. It must be empty, including all versions and delete markers, and you must have permission: %w", err)
	}
	return nil
}
