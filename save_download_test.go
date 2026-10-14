package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"
)

func TestSaveDownloadLocationAndOverwrite(t *testing.T) {
	a := NewApp()
	dir := t.TempDir()
	source := filepath.Join(dir, "cache")
	os.WriteFile(source, []byte("fixture download"), 0600)
	a.transfers["done"] = &Transfer{ID: "done", Key: "folder/file.txt", Path: source, Direction: "download", Status: "completed"}
	result, err := a.SaveDownload(context.Background(), "done", dir, "chosen.txt", false)
	if err != nil || result.NeedsOverwrite {
		t.Fatalf("save: %+v %v", result, err)
	}
	if a.transfers["done"].SavedPath != result.Path || a.transfers["done"].SavedAt.IsZero() {
		t.Fatal("saved location and timestamp missing")
	}
	b, _ := os.ReadFile(result.Path)
	if string(b) != "fixture download" {
		t.Fatal("incorrect saved content")
	}
	os.WriteFile(result.Path, []byte("existing"), 0600)
	result, err = a.SaveDownload(context.Background(), "done", dir, "chosen.txt", false)
	if err != nil || !result.NeedsOverwrite {
		t.Fatal("overwrite confirmation missing")
	}
	b, _ = os.ReadFile(result.Path)
	if string(b) != "existing" {
		t.Fatal("file replaced before confirmation")
	}
	if _, err = a.SaveDownload(context.Background(), "done", dir, "chosen.txt", true); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"", "..", "../escape", "nested/file", "nested\\file"} {
		if _, err = a.SaveDownload(context.Background(), "done", dir, name, false); err == nil {
			t.Errorf("accepted invalid filename %q", name)
		}
	}
	if _, err = a.SaveDownload(context.Background(), "unknown", dir, "unknown.txt", false); err == nil {
		t.Fatal("unknown transfer accepted")
	}
	if a.transfers["done"].SaveError == "" {
		t.Fatal("save failure not recorded")
	}
	if _, err = a.SaveDownload(context.Background(), "done", dir, "retry.txt", false); err != nil {
		t.Fatal(err)
	}
	if a.transfers["done"].SaveError != "" {
		t.Fatal("successful save did not clear error")
	}
	a.saveMu.Lock()
	if _, err = a.SaveDownload(context.Background(), "done", dir, "second.txt", false); err == nil {
		t.Fatal("concurrent save permitted")
	}
	a.saveMu.Unlock()
}

type brokenSaveReader struct{}

func (brokenSaveReader) Read([]byte) (int, error) { return 0, errors.New("fixture failure") }
func TestSaveFailurePreservesDestination(t *testing.T) {
	dir := t.TempDir()
	dest := filepath.Join(dir, "existing.txt")
	os.WriteFile(dest, []byte("original"), 0600)
	if err := copyDownload(context.Background(), io.MultiReader(bytes.NewBufferString("partial"), brokenSaveReader{}), dest, true); err == nil {
		t.Fatal("expected copy failure")
	}
	b, _ := os.ReadFile(dest)
	if string(b) != "original" {
		t.Fatal("failed save replaced destination")
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 1 {
		t.Fatal("temporary save file left behind")
	}
	if err := copyDownload(context.Background(), bytes.NewReader([]byte("replacement")), dest, false); err == nil {
		t.Fatal("unconfirmed overwrite permitted")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := copyDownload(ctx, bytes.NewReader([]byte("replacement")), dest, true); err == nil {
		t.Fatal("cancel ignored")
	}
}
func TestSaveLocationListsOnlyFolders(t *testing.T) {
	dir := t.TempDir()
	os.Mkdir(filepath.Join(dir, "Folder"), 0700)
	os.WriteFile(filepath.Join(dir, "private-file"), []byte("not listed"), 0600)
	v, err := saveLocations(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(v.Folders) != 1 || v.Folders[0] != "Folder" || v.Path != dir {
		t.Fatalf("unexpected locations: %+v", v)
	}
}
