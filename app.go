package main

import (
	"context"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
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
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

type Profile struct {
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
type Transfer struct {
	ID        string
	Direction string
	Key       string
	Path      string
	ProfileID string
	Bucket    string
	Done      int64
	Total     int64
	Status    string
	Error     string
	cancel    context.CancelFunc
}
type App struct {
	ctx        context.Context
	mu         sync.Mutex
	profiles   []Profile
	transfers  map[string]*Transfer
	configFile string
	secretGet  func(string) (string, error)
	secretSet  func(string, string) error
}

func NewApp() *App {
	return &App{transfers: map[string]*Transfer{}, secretGet: func(id string) (string, error) { return keyring.Get("HarborS3", id) }, secretSet: func(id, v string) error { return keyring.Set("HarborS3", id, v) }}
}
func (a *App) startup(ctx context.Context) {
	a.ctx = ctx
	dir, err := os.UserConfigDir()
	if err != nil {
		return
	}
	a.configFile = filepath.Join(dir, "HarborS3", "profiles.json")
	b, err := os.ReadFile(a.configFile)
	if err == nil {
		_ = json.Unmarshal(b, &a.profiles)
	}
}
func (a *App) shutdown(context.Context) {
	a.mu.Lock()
	defer a.mu.Unlock()
	for _, t := range a.transfers {
		if t.cancel != nil {
			t.cancel()
		}
	}
}
func id() string { b := make([]byte, 16); _, _ = rand.Read(b); return hex.EncodeToString(b) }
func (a *App) Profiles() []Profile {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]Profile{}, a.profiles...)
}
func validate(p Profile) error {
	if strings.TrimSpace(p.Name) == "" || strings.TrimSpace(p.Region) == "" {
		return errors.New("Name and region are required")
	}
	if p.Endpoint != "" {
		u, e := url.Parse(p.Endpoint)
		if e != nil || u.Host == "" || u.Scheme != "https" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
			return errors.New("Endpoint must be an HTTP(S) URL without credentials, query or fragment")
		}
	}
	return nil
}
func (a *App) SaveProfile(p Profile, secret Secret) (Profile, error) {
	if err := validate(p); err != nil {
		return Profile{}, err
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if p.ID == "" {
		p.ID = id()
	}
	if secret.AccessKey != "" || secret.SecretKey != "" {
		if secret.AccessKey == "" || secret.SecretKey == "" {
			return Profile{}, errors.New("Both access key and secret key are required")
		}
		b, _ := json.Marshal(secret)
		if e := a.secretSet(p.ID, string(b)); e != nil {
			return Profile{}, fmt.Errorf("Credential vault: %w", e)
		}
	} else {
		if _, e := a.secretGet(p.ID); e != nil {
			return Profile{}, errors.New("Enter credentials for this connection")
		}
	}
	next := append([]Profile{}, a.profiles...)
	found := false
	for i := range next {
		if next[i].ID == p.ID {
			next[i] = p
			found = true
		}
	}
	if !found {
		next = append(next, p)
	}
	b, _ := json.MarshalIndent(next, "", "  ")
	if err := os.MkdirAll(filepath.Dir(a.configFile), 0700); err != nil {
		return Profile{}, err
	}
	tmp := a.configFile + ".tmp"
	if err := os.WriteFile(tmp, b, 0600); err != nil {
		return Profile{}, err
	}
	if err := os.Rename(tmp, a.configFile); err != nil {
		return Profile{}, err
	}
	a.profiles = next
	return p, nil
}
func (a *App) client(ctx context.Context, profileID string) (*s3.Client, error) {
	a.mu.Lock()
	var p Profile
	for _, v := range a.profiles {
		if v.ID == profileID {
			p = v
		}
	}
	a.mu.Unlock()
	if p.ID == "" {
		return nil, errors.New("Connection not found")
	}
	raw, err := a.secretGet(p.ID)
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
	cfg, e := config.LoadDefaultConfig(ctx, config.WithRegion(p.Region), config.WithCredentialsProvider(credentials.NewStaticCredentialsProvider(s.AccessKey, s.SecretKey, s.SessionToken)), config.WithHTTPClient(&http.Client{Transport: transport}))
	if e != nil {
		return nil, e
	}
	return s3.NewFromConfig(cfg, func(o *s3.Options) {
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
		if key == prefix {
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
func (a *App) queue(direction, profile, bucket, key, path string) string {
	t := &Transfer{ID: id(), Direction: direction, ProfileID: profile, Bucket: bucket, Key: key, Path: path, Status: "queued"}
	a.mu.Lock()
	a.transfers[t.ID] = t
	a.mu.Unlock()
	go a.run(t.ID)
	return t.ID
}
func (a *App) Transfers() []Transfer {
	a.mu.Lock()
	defer a.mu.Unlock()
	out := []Transfer{}
	for _, t := range a.transfers {
		out = append(out, *t)
	}
	return out
}
func (a *App) CancelTransfer(id string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if t := a.transfers[id]; t != nil && t.cancel != nil {
		t.cancel()
	}
}
func (a *App) RetryTransfer(id string) error {
	a.mu.Lock()
	t := a.transfers[id]
	if t == nil || (t.Status != "failed" && t.Status != "cancelled") {
		a.mu.Unlock()
		return errors.New("Only failed or cancelled transfers can be retried")
	}
	t.Status = "queued"
	t.Done = 0
	t.Error = ""
	a.mu.Unlock()
	go a.run(id)
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
func (a *App) run(id string) {
	ctx, cancel := context.WithCancel(a.ctx)
	defer cancel()
	a.mu.Lock()
	t := a.transfers[id]
	t.cancel = cancel
	t.Status = "running"
	spec := *t
	a.mu.Unlock()
	err := a.execute(ctx, spec)
	a.mu.Lock()
	defer a.mu.Unlock()
	t.cancel = nil
	if err != nil {
		t.Status = "failed"
		t.Error = err.Error()
		if ctx.Err() != nil {
			t.Status = "cancelled"
			t.Error = ""
		}
	} else {
		t.Status = "completed"
		t.Done = t.Total
	}
}
func (a *App) execute(ctx context.Context, t Transfer) error {
	c, e := a.client(ctx, t.ProfileID)
	if e != nil {
		return e
	}
	update := func(n int64) { a.mu.Lock(); a.transfers[t.ID].Done += n; a.mu.Unlock() }
	total := func(n int64) { a.mu.Lock(); a.transfers[t.ID].Total = n; a.mu.Unlock() }
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
		_, e = manager.NewUploader(c).Upload(ctx, &s3.PutObjectInput{Bucket: aws.String(t.Bucket), Key: aws.String(t.Key), Body: &progressReader{Reader: f, update: update}})
		return e
	}
	v, e := c.GetObject(ctx, &s3.GetObjectInput{Bucket: aws.String(t.Bucket), Key: aws.String(t.Key)})
	if e != nil {
		return e
	}
	defer v.Body.Close()
	total(aws.ToInt64(v.ContentLength))
	f, e := os.CreateTemp(filepath.Dir(t.Path), ".harbor-download-*")
	if e != nil {
		return e
	}
	tmp := f.Name()
	defer os.Remove(tmp)
	_, e = io.Copy(f, &progressReader{Reader: v.Body, update: update})
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
	return os.Rename(tmp, t.Path)
}
