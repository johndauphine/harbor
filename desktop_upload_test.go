package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDesktopFolderMetadata(t *testing.T) {
	root := filepath.Join(t.TempDir(), "Downloads")
	if err := os.MkdirAll(filepath.Join(root, "nested"), 0700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "nested", "file.txt")
	if err := os.WriteFile(path, []byte("test"), 0600); err != nil {
		t.Fatal(err)
	}
	files, err := collectDesktopUploads(context.Background(), []string{root}, true)
	if err != nil || len(files) != 1 {
		t.Fatalf("%v, %v", files, err)
	}
	if files[0].Path != "Downloads/nested/file.txt" || files[0].LocalPath != path || files[0].Size != 4 {
		t.Fatalf("wrong metadata: %+v", files[0])
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := collectDesktopUploads(ctx, []string{root}, true); err != context.Canceled {
		t.Fatalf("cancellation: %v", err)
	}
	if _, err := collectDesktopUploads(context.Background(), []string{path + "missing"}, false); err == nil {
		t.Fatal("missing file accepted")
	}
	if _, err := collectDesktopUploads(context.Background(), []string{root}, false); err == nil {
		t.Fatal("directory accepted as file")
	}
	link := filepath.Join(root, "link.txt")
	if err := os.Symlink(path, link); err == nil {
		if _, err := collectDesktopUploads(context.Background(), []string{root}, true); err == nil {
			t.Fatal("symlink accepted")
		}
	}
}

func TestDesktopFolderLimits(t *testing.T) {
	root := t.TempDir()
	for i := 0; i < 1001; i++ {
		if err := os.WriteFile(filepath.Join(root, fmt.Sprintf("%04d.txt", i)), nil, 0600); err != nil {
			t.Fatal(err)
		}
	}
	if files, err := collectDesktopUploads(context.Background(), []string{root}, true); err == nil || files != nil || !strings.Contains(err.Error(), "1,000") {
		t.Fatalf("partial oversized selection: %d, %v", len(files), err)
	}
	path := filepath.Join(t.TempDir(), "large.bin")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	err = f.Truncate((5 << 30) + 1)
	f.Close()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := collectDesktopUploads(context.Background(), []string{path}, false); err == nil {
		t.Fatal("oversize file accepted")
	}
}
