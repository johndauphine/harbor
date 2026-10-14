package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

type SaveResult struct {
	Path           string
	NeedsOverwrite bool
}
type SaveLocations struct {
	Path    string
	Parent  string
	Home    string
	Folders []string
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
func (a *App) SaveDownload(ctx context.Context, id, directory, name string, overwrite bool) (SaveResult, error) {
	if !a.saveMu.TryLock() {
		return SaveResult{}, errors.New("Another download is being saved")
	}
	defer a.saveMu.Unlock()
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
