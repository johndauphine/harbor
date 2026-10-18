package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
)

type SaveResult struct {
	ID             string
	Path           string
	NeedsOverwrite bool
}
type SaveLocations struct {
	Path    string
	Parent  string
	Home    string
	Folders []string
	Files   []LocalFile `json:",omitempty"`
}

func saveLocations(directory string) (SaveLocations, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return SaveLocations{}, err
	}
	if directory == "" {
		directory = filepath.Join(home, "Downloads")
		if st, e := os.Stat(directory); e != nil || !st.IsDir() {
			directory = home
		}
	}
	if !filepath.IsAbs(directory) {
		return SaveLocations{}, errors.New("Enter an absolute folder path")
	}
	directory = filepath.Clean(directory)
	entries, err := os.ReadDir(directory)
	if err != nil {
		return SaveLocations{}, fmt.Errorf("Cannot open folder: %w", err)
	}
	result := SaveLocations{Path: directory, Parent: filepath.Dir(directory), Home: home, Folders: []string{}}
	for _, entry := range entries {
		if entry.IsDir() {
			result.Folders = append(result.Folders, entry.Name())
		}
	}
	return result, nil
}
func (a *App) SaveDownload(ctx context.Context, id, directory, name string, overwrite bool) (result SaveResult, saveErr error) {
	var done func()
	ctx, done, saveErr = a.beginOperation(ctx)
	if saveErr != nil {
		return SaveResult{}, saveErr
	}
	defer done()
	if !a.saveMu.TryLock() {
		return SaveResult{}, errors.New("Another download is being saved")
	}
	defer a.saveMu.Unlock()
	defer func() {
		a.mu.Lock()
		defer a.mu.Unlock()
		if t := a.transfers[id]; t != nil && t.Direction == "download" && t.Status == "completed" {
			if saveErr != nil {
				t.SaveError = saveErr.Error()
			} else if !result.NeedsOverwrite {
				t.SaveError = ""
				t.SavedPath = result.Path
				t.SavedAt = time.Now()
			}
			if err := a.persistTransferLocked(t); err != nil {
				saveErr = errors.Join(saveErr, err)
			}
		}
	}()
	if name == "" || name == "." || name == ".." || strings.ContainsAny(name, "/\\\x00") || !filepath.IsAbs(directory) {
		return SaveResult{}, errors.New("Choose a folder and enter a filename without slashes")
	}
	a.mu.Lock()
	t := a.transfers[id]
	var source string
	if t != nil && t.Direction == "download" && t.Status == "completed" {
		source = t.Path
	}
	a.mu.Unlock()
	if source == "" {
		return SaveResult{}, errors.New("Completed download not found")
	}
	destination := filepath.Join(directory, name)
	if st, err := os.Lstat(destination); err == nil {
		if st.IsDir() {
			return SaveResult{}, errors.New("A folder already has this name")
		}
		if !overwrite {
			return SaveResult{Path: destination, NeedsOverwrite: true}, nil
		}
	} else if !os.IsNotExist(err) {
		return SaveResult{}, err
	}
	f, err := os.Open(source)
	if err != nil {
		return SaveResult{}, fmt.Errorf("Downloaded file unavailable: %w", err)
	}
	defer f.Close()
	if err = copyDownload(ctx, f, destination, overwrite); err != nil {
		return SaveResult{}, fmt.Errorf("Could not save file: %w", err)
	}
	return SaveResult{Path: destination}, nil
}

type contextReader struct {
	ctx    context.Context
	reader io.Reader
}

func (r contextReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.reader.Read(p)
}
func copyDownload(ctx context.Context, source io.Reader, destination string, overwrite bool) error {
	f, err := os.CreateTemp(filepath.Dir(destination), ".harbor-save-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer os.Remove(tmp)
	defer f.Close()
	if _, err = io.Copy(f, contextReader{ctx, source}); err != nil {
		return err
	}
	if err = f.Sync(); err != nil {
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	if err = ctx.Err(); err != nil {
		return err
	}
	if overwrite {
		return os.Rename(tmp, destination)
	}
	// Link publishes the complete file atomically without replacing a file created concurrently.
	return os.Link(tmp, destination)
}

// QueueDownloadTo confirms the destination before starting any transfer.
func (a *App) QueueDownloadTo(profile, bucket, key, directory, name string, overwrite bool, expected ...string) (SaveResult, error) {
	if name == "" || name == "." || name == ".." || strings.ContainsAny(name, "/\\\x00") || !filepath.IsAbs(directory) {
		return SaveResult{}, errors.New("Choose a folder and enter a filename without slashes")
	}
	st, err := os.Stat(directory)
	if err != nil {
		return SaveResult{}, err
	}
	if !st.IsDir() {
		return SaveResult{}, errors.New("Choose a folder")
	}
	destination := filepath.Join(directory, name)
	if st, err := os.Lstat(destination); err == nil {
		if st.IsDir() {
			return SaveResult{}, errors.New("A folder already has this name")
		}
		if !overwrite {
			return SaveResult{Path: destination, NeedsOverwrite: true}, nil
		}
	} else if !os.IsNotExist(err) {
		return SaveResult{}, err
	}
	identity := ""
	if len(expected) > 0 {
		identity = expected[0]
	}
	id, err := a.QueueDownload(profile, bucket, key, downloadTarget{Path: destination, Overwrite: overwrite, ConnectionIdentity: identity})
	return SaveResult{ID: id, Path: destination}, err
}

type LocalFile struct {
	Name string
	Size int64
}

func uploadLocations(directory string) (SaveLocations, error) {
	result, err := saveLocations(directory)
	if err != nil {
		return result, err
	}
	entries, err := os.ReadDir(result.Path)
	if err != nil {
		return result, err
	}
	result.Files = []LocalFile{}
	for _, entry := range entries {
		if entry.Type().IsRegular() {
			if info, err := entry.Info(); err == nil && info.Mode().IsRegular() {
				result.Files = append(result.Files, LocalFile{Name: entry.Name(), Size: info.Size()})
			}
		}
	}
	return result, nil
}
func (a *App) QueueLocalUpload(ctx context.Context, profile, bucket, key, path string, expected ...string) (string, error) {
	ctx, done, err := a.beginOperation(ctx)
	if err != nil {
		return "", err
	}
	defer done()
	if bucket == "" || key == "" || strings.HasSuffix(key, "/") {
		return "", errors.New("Choose a destination file")
	}
	if !filepath.IsAbs(path) {
		return "", errors.New("Choose a local file")
	}
	identity := ""
	if len(expected) > 0 {
		identity = expected[0]
	}
	_, identity, err = a.clientSnapshot(ctx, profile, identity)
	if err != nil {
		return "", err
	}
	st, err := os.Lstat(path)
	if err != nil {
		return "", err
	}
	if !st.Mode().IsRegular() {
		return "", errors.New("Select a regular file")
	}
	const limit = 5 << 30
	if st.Size() > limit {
		return "", errors.New("Uploads are limited to 5 GiB")
	}
	release, err := a.acquireTransferPreparation(ctx)
	if err != nil {
		return "", err
	}
	defer release()
	stageStarted := time.Now()
	source, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer source.Close()
	dir, err := a.transferCacheDir()
	if err != nil {
		return "", err
	}
	if err = os.MkdirAll(dir, 0700); err != nil {
		return "", err
	}
	staged, err := os.CreateTemp(dir, "upload-*")
	if err != nil {
		return "", err
	}
	stagedPath := staged.Name()
	n, copyErr := io.Copy(staged, io.LimitReader(contextReader{ctx, source}, limit+1))
	if copyErr == nil {
		copyErr = staged.Sync()
	}
	closeErr := staged.Close()
	if copyErr != nil || closeErr != nil || n > limit {
		os.Remove(stagedPath)
		return "", errors.New("Could not stage upload; check file access and the 5 GiB limit")
	}
	a.recordTransferPhase("staging", stageStarted)
	transferID, err := a.queueTransferBound("upload", profile, bucket, key, stagedPath, identity)
	if err != nil {
		os.Remove(stagedPath)
		return "", err
	}
	return transferID, nil
}
