package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/smithy-go"
)

// Only non-secret transfer metadata is journaled. Upload bodies live in the
// database's private cache; download destinations retain their overwrite policy.
type transferRecord struct {
	Transfer
	Attempt        uint64
	Destination    *downloadTarget
	MultipartID    string
	MultipartScope string
	PartialPath    string
}

func (a *App) transferCacheDir() (string, error) {
	dir := a.transferCache
	if !a.journalEnabled {
		base, err := os.UserCacheDir()
		if err != nil {
			return "", err
		}
		dir = filepath.Join(base, "HarborS3")
	}
	if err := os.MkdirAll(dir, 0700); err != nil {
		return "", err
	}
	if info, err := os.Lstat(dir); err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return "", errors.New("Transfer cache must be a private local directory")
	}
	return dir, nil
}

// Caller holds a.mu. State is committed before admitting a worker or releasing
// a remote multipart ID to the SDK. Progress counters need not be fsynced.
func (a *App) persistTransferLocked(t *Transfer) error {
	if !a.journalEnabled {
		return nil
	}
	raw, err := json.Marshal(transferRecord{Transfer: *t, Attempt: t.attempt, Destination: t.destination, MultipartID: t.multipartID, MultipartScope: t.multipartScope, PartialPath: t.partialPath})
	if err != nil {
		return err
	}
	db, err := a.openStore()
	if err != nil {
		return err
	}
	defer db.Close()
	_, err = db.Exec("INSERT INTO transfers(id,profile_id,metadata) VALUES(?,?,?) ON CONFLICT(id) DO UPDATE SET metadata=excluded.metadata", t.ID, t.ProfileID, string(raw))
	if err != nil {
		return fmt.Errorf("Save transfer recovery state: %w", err)
	}
	return nil
}

func (a *App) partialDownloadPath(t Transfer) string {
	directory := filepath.Dir(t.Path)
	if t.destination != nil {
		directory = filepath.Dir(t.destination.Path)
	}
	return filepath.Join(directory, ".harbor-download-"+t.ID+"-"+strconv.FormatUint(t.attempt, 10))
}

func validTransferID(s string) bool {
	if len(s) != 32 {
		return false
	}
	for _, r := range s {
		if !(r >= '0' && r <= '9' || r >= 'a' && r <= 'f') {
			return false
		}
	}
	return true
}

func (a *App) recoverTransfers(db *sql.DB) error {
	if _, err := db.Exec("CREATE TABLE IF NOT EXISTS transfers(id TEXT PRIMARY KEY,profile_id TEXT NOT NULL,metadata TEXT NOT NULL)"); err != nil {
		return err
	}
	var canonical string
	if err := db.QueryRow("SELECT path FROM store_location WHERE singleton=1").Scan(&canonical); err != nil {
		return err
	}
	a.transferCache = canonical + ".transfers"
	a.journalEnabled = true
	if _, err := a.transferCacheDir(); err != nil {
		return err
	}
	rows, err := db.Query("SELECT id,metadata FROM transfers ORDER BY rowid")
	if err != nil {
		return err
	}
	for rows.Next() {
		var key, raw string
		if err = rows.Scan(&key, &raw); err != nil {
			rows.Close()
			return err
		}
		var r transferRecord
		if err = json.Unmarshal([]byte(raw), &r); err != nil {
			rows.Close()
			return fmt.Errorf("Invalid transfer journal: %w", err)
		}
		t := r.Transfer
		t.attempt, t.destination, t.multipartID, t.multipartScope, t.partialPath = r.Attempt, r.Destination, r.MultipartID, r.MultipartScope, r.PartialPath
		if !validTransferID(t.ID) || t.ID != key || (t.Direction != "upload" && t.Direction != "download") || t.attempt == 0 || filepath.Dir(t.Path) != a.transferCache || (t.Direction == "upload" && !strings.HasPrefix(filepath.Base(t.Path), "upload-")) || (t.Direction == "download" && !strings.HasPrefix(filepath.Base(t.Path), "download-")) || (t.partialPath != "" && t.partialPath != a.partialDownloadPath(t)) {
			rows.Close()
			return errors.New("Invalid transfer journal paths or transfer metadata; preserve the configuration for recovery")
		}
		switch t.Status {
		case "queued", "running":
			t.Status = "failed"
			t.Error = "Interrupted when Harbor stopped. Retry to restart this transfer."
		case "failed", "cancelled", "completed":
		default:
			rows.Close()
			return errors.New("Invalid transfer journal status")
		}
		a.transfers[t.ID] = &t
		a.transferOrder = append(a.transferOrder, t.ID)
	}
	if err = rows.Err(); err != nil {
		rows.Close()
		return err
	}
	rows.Close()
	// Remove only recorded partial files and unreferenced files in this store's
	// cache. Never scan a shared cache or delete arbitrary destination files.
	referenced := map[string]bool{}
	for _, t := range a.transfers {
		if t.Direction == "upload" && t.Status != "completed" || t.Direction == "download" && t.Status == "completed" && t.destination == nil {
			referenced[t.Path] = true
		}
		t.CleanupError = ""
		if err = a.removePartial(t); err != nil {
			t.CleanupError = err.Error()
		}
		if err = a.persistTransferLocked(t); err != nil {
			return err
		}
	}
	entries, err := os.ReadDir(a.transferCache)
	if err != nil {
		return err
	}
	for _, e := range entries {
		p := filepath.Join(a.transferCache, e.Name())
		if !e.IsDir() && !referenced[p] && (strings.HasPrefix(e.Name(), "upload-") || strings.HasPrefix(e.Name(), "download-") || strings.HasPrefix(e.Name(), ".harbor-download-")) {
			if err = os.Remove(p); err != nil && !os.IsNotExist(err) {
				return fmt.Errorf("Clean transfer cache: %w", err)
			}
		}
	}
	// Best effort, with a single startup budget. Offline servers and locked vaults
	// do not discard recovery records. Retry repeats cleanup before any new upload.
	ctx, cancel := context.WithTimeout(a.ctx, 5*time.Second)
	defer cancel()
	for _, transferID := range a.transferOrder {
		t := a.transfers[transferID]
		if t.multipartID != "" {
			if err = a.cleanupMultipart(ctx, *t); err != nil {
				t.CleanupError = "Remote multipart cleanup pending; Retry when the connection is available."
				if err = a.persistTransferLocked(t); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

func (a *App) removePartial(t *Transfer) error {
	if t.partialPath == "" {
		return nil
	}
	if t.partialPath != a.partialDownloadPath(*t) {
		return errors.New("Invalid partial download path")
	}
	if err := os.Remove(t.partialPath); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("Remove interrupted download: %w", err)
	}
	t.partialPath = ""
	return nil
}

func multipartScope(endpoint, region string, pathStyle bool) string {
	raw, _ := json.Marshal(struct {
		Endpoint, Region string
		PathStyle        bool
	}{endpoint, region, pathStyle})
	return string(raw)
}
func multipartClientScope(c *s3.Client) string {
	options := c.Options()
	return multipartScope(aws.ToString(options.BaseEndpoint), options.Region, options.UsePathStyle)
}
func (a *App) multipartScopeFor(profileID string) string {
	for _, p := range a.Profiles() {
		if p.ID == profileID {
			return multipartScope(p.Endpoint, p.Region, p.PathStyle)
		}
	}
	return ""
}

func (a *App) setMultipart(t Transfer, uploadID, scope string) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	current := a.transfers[t.ID]
	if current == nil || current.attempt != t.attempt {
		return errors.New("Transfer attempt changed")
	}
	previousID, previousScope, previousCleanup := current.multipartID, current.multipartScope, current.CleanupError
	current.multipartID, current.multipartScope = uploadID, scope
	if uploadID == "" {
		current.CleanupError = ""
	}
	if err := a.persistTransferLocked(current); err != nil {
		current.multipartID, current.multipartScope, current.CleanupError = previousID, previousScope, previousCleanup
		return err
	}
	return nil
}

func (a *App) cleanupMultipart(ctx context.Context, t Transfer) error {
	if t.multipartID == "" {
		return nil
	}
	c, err := a.client(ctx, t.ProfileID)
	if err != nil {
		return err
	}
	if t.multipartScope == "" || multipartClientScope(c) != t.multipartScope {
		return errors.New("Restore this transfer's original connection settings before cleaning up its multipart upload")
	}
	_, err = c.AbortMultipartUpload(ctx, &s3.AbortMultipartUploadInput{Bucket: aws.String(t.Bucket), Key: aws.String(t.Key), UploadId: aws.String(t.multipartID)})
	var apiErr smithy.APIError
	if err != nil && !(errors.As(err, &apiErr) && apiErr.ErrorCode() == "NoSuchUpload") {
		return fmt.Errorf("Remote multipart cleanup pending: %w", err)
	}
	return a.setMultipart(t, "", "")
}

func (a *App) prepareTransfer(ctx context.Context, t Transfer) error {
	cleanupCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := a.cleanupMultipart(cleanupCtx, t); err != nil {
		return err
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	current := a.transfers[t.ID]
	if current == nil || current.attempt != t.attempt {
		return errors.New("Transfer attempt changed")
	}
	// A partial from the previous attempt must be removed before increasing the
	// attempt number (RetryTransfer performs that cleanup while holding a.mu).
	if err := a.removePartial(current); err != nil {
		return err
	}
	current.CleanupError = ""
	return a.persistTransferLocked(current)
}

// Record the upload ID before the SDK can send parts. Successful completion or
// abort retires it; failed cleanup retains the exact ID for startup/retry.
func (c *cleanupUploadClient) CreateMultipartUpload(ctx context.Context, in *s3.CreateMultipartUploadInput, opts ...func(*s3.Options)) (*s3.CreateMultipartUploadOutput, error) {
	out, err := c.Client.CreateMultipartUpload(ctx, in, opts...)
	if err != nil || c.app == nil {
		return out, err
	}
	if err = c.app.setMultipart(c.transfer, aws.ToString(out.UploadId), c.scope); err != nil {
		cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		_, abortErr := c.Client.AbortMultipartUpload(cleanupCtx, &s3.AbortMultipartUploadInput{Bucket: in.Bucket, Key: in.Key, UploadId: out.UploadId})
		if abortErr != nil {
			c.abortErr = abortErr
			persistErr := c.app.retainMultipart(c.transfer, aws.ToString(out.UploadId), c.scope)
			return nil, errors.Join(err, abortErr, persistErr)
		}
		return nil, err
	}
	return out, nil
}
func (c *cleanupUploadClient) CompleteMultipartUpload(ctx context.Context, in *s3.CompleteMultipartUploadInput, opts ...func(*s3.Options)) (*s3.CompleteMultipartUploadOutput, error) {
	out, err := c.Client.CompleteMultipartUpload(ctx, in, opts...)
	if err == nil && c.app != nil {
		err = c.app.setMultipart(c.transfer, "", "")
	}
	return out, err
}

func (a *App) setPartial(t Transfer, path string) error {
	if !a.journalEnabled {
		return nil
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	current := a.transfers[t.ID]
	if current == nil || current.attempt != t.attempt {
		return errors.New("Transfer attempt changed")
	}
	previous := current.partialPath
	current.partialPath = path
	if err := a.persistTransferLocked(current); err != nil {
		current.partialPath = previous
		return err
	}
	return nil
}

// If both the initial checkpoint and its fallback abort fail, retain ownership
// in memory even while storage is unavailable. No parts are sent; subsequent
// checkpoints, restart recovery, or Retry can retire the exact upload ID.
func (a *App) retainMultipart(t Transfer, uploadID, scope string) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	current := a.transfers[t.ID]
	if current == nil || current.attempt != t.attempt {
		return errors.New("Transfer attempt changed")
	}
	current.multipartID, current.multipartScope = uploadID, scope
	current.CleanupError = "Remote multipart cleanup pending; Retry when the connection is available."
	return a.persistTransferLocked(current)
}
