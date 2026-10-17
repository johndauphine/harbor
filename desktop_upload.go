package main

import (
	"context"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
)

// Only metadata crosses WebView2. QueueLocalUpload stages the selected bytes
// using bounded Go I/O, rather than a large WebView2 request body.
type desktopUploadFile struct {
	Name      string `json:"name"`
	Size      int64  `json:"size"`
	Path      string `json:"path"`
	LocalPath string `json:"localPath"`
}

func collectDesktopUploads(ctx context.Context, paths []string, folder bool) ([]desktopUploadFile, error) {
	files := []desktopUploadFile{}
	add := func(path, key string) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		info, err := os.Lstat(path)
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("Select regular files; %s is not a regular file", path)
		}
		if info.Size() > 5<<30 {
			return fmt.Errorf("%s exceeds the 5 GiB file limit", key)
		}
		if len(files) >= 1000 {
			return fmt.Errorf("Select up to 1,000 files per upload")
		}
		files = append(files, desktopUploadFile{Name: info.Name(), Size: info.Size(), Path: filepath.ToSlash(key), LocalPath: path})
		return nil
	}
	for _, path := range paths {
		absolute, err := filepath.Abs(path)
		if err != nil {
			return nil, err
		}
		if !folder {
			if err := add(absolute, filepath.Base(absolute)); err != nil {
				return nil, err
			}
			continue
		}
		info, err := os.Lstat(absolute)
		if err != nil {
			return nil, err
		}
		if !info.IsDir() {
			return nil, fmt.Errorf("Select a regular folder")
		}
		err = filepath.WalkDir(absolute, func(path string, entry fs.DirEntry, walkErr error) error {
			if walkErr != nil {
				return walkErr
			}
			if err := ctx.Err(); err != nil {
				return err
			}
			if entry.IsDir() {
				return nil
			}
			relative, err := filepath.Rel(filepath.Dir(absolute), path)
			if err != nil {
				return err
			}
			return add(path, relative)
		})
		if err != nil {
			return nil, err
		}
	}
	return files, nil
}
