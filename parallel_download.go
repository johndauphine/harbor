package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
)

const parallelDownloadThreshold int64 = 64 << 20
const downloadChunkSize int64 = 40 << 20
const maxParallelChunks = 100000

// Checkpoints are immutable after publication. Workers clone before updating,
// so a transfer attempt's snapshot never races another worker's journal commit.
type transferCheckpoint struct {
	Version     int
	Kind        string
	Scope       string
	Size        int64
	ChunkSize   int64
	ETag        string
	VersionID   string
	FileAttempt uint64
	Blocks      map[int]string // SHA-256 for downloads; acknowledged ETags for uploads.
	SourceHash  string
}

func cloneCheckpoint(c *transferCheckpoint) *transferCheckpoint {
	if c == nil {
		return nil
	}
	out := *c
	out.Blocks = make(map[int]string, len(c.Blocks))
	for k, v := range c.Blocks {
		out.Blocks[k] = v
	}
	return &out
}
func checkpointChunks(c *transferCheckpoint) int { return int((c.Size-1)/c.ChunkSize + 1) }
func checkpointBytes(c *transferCheckpoint) int64 {
	if c == nil {
		return 0
	}
	var n int64
	for block := range c.Blocks {
		n += min(c.ChunkSize, c.Size-int64(block)*c.ChunkSize)
	}
	return n
}
func validateTransferCheckpoint(t Transfer) error {
	c := t.checkpoint
	if c == nil {
		return nil
	}
	if c.Version != 1 || c.Kind != t.Direction || c.Size < parallelDownloadThreshold || c.ChunkSize <= 0 || (c.Size-1)/c.ChunkSize+1 > maxParallelChunks || c.Scope == "" || c.FileAttempt == 0 || c.FileAttempt > t.attempt {
		return errors.New("Invalid transfer checkpoint; preserve the journal for recovery")
	}
	if c.Kind == "download" && (c.ChunkSize != downloadChunkSize || c.ETag == "") {
		return errors.New("Invalid download checkpoint")
	}
	if c.Kind == "upload" {
		decoded, err := hex.DecodeString(c.SourceHash)
		if !validUploadPartSize(c.ChunkSize) || err != nil || len(decoded) != sha256.Size || checkpointChunks(c) > 10000 {
			return errors.New("Invalid upload checkpoint")
		}
	}
	for index, digest := range c.Blocks {
		if index < 0 || index >= checkpointChunks(c) {
			return errors.New("Invalid checkpoint range")
		}
		if c.Kind == "download" {
			decoded, e := hex.DecodeString(digest)
			if e != nil || len(decoded) != sha256.Size {
				return errors.New("Invalid checkpoint digest")
			}
		}
	}
	return nil
}
func (a *App) transferSlots() chan struct{} {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.rangeSlots == nil {
		a.rangeSlots = make(chan struct{}, 8)
	}
	return a.rangeSlots
}
func (a *App) storeCheckpoint(t Transfer, c *transferCheckpoint, path string) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	current := a.transfers[t.ID]
	if current == nil || current.attempt != t.attempt {
		return errors.New("Transfer attempt changed")
	}
	old, oldPath, oldResume, oldAck := current.checkpoint, current.partialPath, current.Resumable, current.Acknowledged
	current.checkpoint = c
	if c != nil {
		current.Acknowledged = c.Kind == "upload"
	}
	current.partialPath = path
	current.Resumable = c != nil && len(c.Blocks) > 0
	if e := a.persistTransferLocked(current); e != nil {
		current.checkpoint, current.partialPath, current.Resumable, current.Acknowledged = old, oldPath, oldResume, oldAck
		return e
	}
	return nil
}
func (a *App) saveCheckpointBlock(t Transfer, index int, digest string) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	current := a.transfers[t.ID]
	if current == nil || current.attempt != t.attempt || current.checkpoint == nil {
		return errors.New("Transfer checkpoint changed")
	}
	previous := current.checkpoint
	next := cloneCheckpoint(previous)
	next.Blocks[index] = digest
	current.checkpoint = next
	current.Resumable = true
	if e := a.persistTransferLocked(current); e != nil {
		current.checkpoint = previous
		current.Resumable = len(previous.Blocks) > 0
		return e
	}
	return nil
}
func (a *App) checkpointProgress(t Transfer, done, resumed int64) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if current := a.transfers[t.ID]; current != nil && current.attempt == t.attempt {
		current.Done = done
		current.Total = t.Total
		current.ResumedBytes = resumed
	}
}
func (a *App) discardDownloadCheckpoint(t Transfer) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	current := a.transfers[t.ID]
	if current == nil || current.attempt != t.attempt {
		return errors.New("Transfer attempt changed")
	}
	if e := a.removePartial(current); e != nil {
		return e
	}
	old := current.checkpoint
	current.checkpoint = nil
	current.Resumable = false
	current.ResumedBytes = 0
	if e := a.persistTransferLocked(current); e != nil {
		current.checkpoint = old
		return e
	}
	return nil
}
func digestRange(file *os.File, start, size int64) (string, error) {
	h := sha256.New()
	n, e := io.Copy(h, io.NewSectionReader(file, start, size))
	if e != nil {
		return "", e
	}
	if n != size {
		return "", io.ErrUnexpectedEOF
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

var errRangesUnsupported = errors.New("Storage does not support byte ranges")
var errDownloadIdentity = errors.New("Remote object changed during download; retry to fetch the current object")

// Returns false only for a provider that ignores Range, allowing a safe fallback
// to the original single-request path after all writers stop and state is retired.
func (a *App) parallelDownload(ctx context.Context, c *s3.Client, t Transfer, initial *s3.GetObjectOutput) (handled bool, resultErr error) {
	defer func() {
		if resultErr == nil {
			return
		}
		a.mu.Lock()
		current := a.transfers[t.ID]
		cancelled := current != nil && current.attempt == t.attempt && current.userCancelled
		a.mu.Unlock()
		if cancelled {
			resultErr = errors.Join(resultErr, a.discardDownloadCheckpoint(t))
		}
	}()
	cp := cloneCheckpoint(t.checkpoint)
	scope := multipartClientScope(c)
	if cp != nil && cp.Scope != scope {
		return true, errors.New("Restore the original connection settings before resuming this download")
	}
	// A crash may occur after publishing a complete file but before recording the
	// completed state. Recognize that exact file without overwriting anything.
	if cp != nil && len(cp.Blocks) == checkpointChunks(cp) {
		published := t.Path
		if t.destination != nil {
			published = t.destination.Path
		}
		partialInfo, partialErr := os.Lstat(t.partialPath)
		publishedInfo, publishedErr := os.Lstat(published)
		linked := partialErr == nil && publishedErr == nil && os.SameFile(partialInfo, publishedInfo)
		if (os.IsNotExist(partialErr) || linked) && verifyPublishedDownload(published, cp) {
			if linked {
				if e := os.Remove(t.partialPath); e != nil {
					return true, e
				}
			}
			t.Total = cp.Size
			a.checkpointProgress(t, cp.Size, cp.Size)
			return true, a.finishCheckpointedTransfer(t, cp.Size)
		}
	}
	var size int64
	var etag, version string
	if initial != nil {
		size = aws.ToInt64(initial.ContentLength)
		etag = aws.ToString(initial.ETag)
		version = aws.ToString(initial.VersionId)
	} else {
		head, e := c.HeadObject(ctx, &s3.HeadObjectInput{Bucket: aws.String(t.Bucket), Key: aws.String(t.Key)})
		if e != nil {
			return true, e
		}
		size = aws.ToInt64(head.ContentLength)
		etag = aws.ToString(head.ETag)
		version = aws.ToString(head.VersionId)
	}
	if size < parallelDownloadThreshold || etag == "" || (size-1)/downloadChunkSize+1 > maxParallelChunks {
		if cp != nil {
			if e := a.discardDownloadCheckpoint(t); e != nil {
				return true, e
			}
		}
		return false, nil
	}
	if cp != nil && (cp.Size != size || cp.ETag != etag || cp.VersionID != version) {
		if e := a.discardDownloadCheckpoint(t); e != nil {
			return true, e
		}
		cp = nil
		t.checkpoint = nil
		t.partialPath = ""
	}
	t.Total = size
	var file *os.File
	var path string
	if cp != nil {
		path = t.partialPath
		info, e := os.Lstat(path)
		if e == nil && !info.Mode().IsRegular() {
			return true, errors.New("Download checkpoint is not a regular private file")
		}
		if e == nil && info.Size() == size {
			file, e = os.OpenFile(path, os.O_RDWR, 0600)
			if e != nil {
				return true, e
			}
			opened, e := file.Stat()
			if e != nil || !os.SameFile(info, opened) {
				file.Close()
				return true, errors.New("Download checkpoint file changed")
			}
			for index, want := range cp.Blocks {
				start := int64(index) * cp.ChunkSize
				got, e := digestRange(file, start, min(cp.ChunkSize, size-start))
				if e != nil || got != want {
					delete(cp.Blocks, index)
				}
			}
		} else {
			if e != nil && !os.IsNotExist(e) {
				return true, e
			}
			if e = a.discardDownloadCheckpoint(t); e != nil {
				return true, e
			}
			cp = nil
			t.checkpoint = nil
			t.partialPath = ""
		}
	}
	if cp == nil {
		cp = &transferCheckpoint{Version: 1, Kind: "download", Scope: scope, Size: size, ChunkSize: downloadChunkSize, ETag: etag, VersionID: version, FileAttempt: t.attempt, Blocks: map[int]string{}}
		t.checkpoint = cp
		path = a.partialDownloadPath(t)
		if e := a.storeCheckpoint(t, cp, path); e != nil {
			return true, e
		}
		var e error
		file, e = os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0600)
		if e != nil {
			_ = a.storeCheckpoint(t, nil, "")
			return true, e
		}
		if e = file.Truncate(size); e != nil {
			file.Close()
			_ = a.discardDownloadCheckpoint(t)
			return true, e
		}
	} else {
		if e := a.storeCheckpoint(t, cp, path); e != nil {
			file.Close()
			return true, e
		}
	}
	defer file.Close()
	resumed := checkpointBytes(cp)
	a.checkpointProgress(t, resumed, resumed)
	var live atomic.Int64
	live.Store(resumed)
	workCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	var next atomic.Int64
	var once sync.Once
	var firstErr error
	var group sync.WaitGroup
	slots := a.transferSlots()
	count := checkpointChunks(cp)
	fail := func(e error) { once.Do(func() { firstErr = e; cancel() }) }
	for worker := 0; worker < min(8, count); worker++ {
		group.Add(1)
		go func() {
			defer group.Done()
			for {
				index := int(next.Add(1) - 1)
				if index >= count {
					return
				}
				if _, done := cp.Blocks[index]; done {
					continue
				}
				select {
				case slots <- struct{}{}:
				case <-workCtx.Done():
					return
				}
				start := int64(index) * cp.ChunkSize
				length := min(cp.ChunkSize, size-start)
				e := func() error {
					defer func() { <-slots }()
					input := &s3.GetObjectInput{Bucket: aws.String(t.Bucket), Key: aws.String(t.Key), Range: aws.String(fmt.Sprintf("bytes=%d-%d", start, start+length-1)), IfMatch: aws.String(etag)}
					if version != "" {
						input.VersionId = aws.String(version)
					}
					object, e := c.GetObject(workCtx, input)
					if e != nil {
						return e
					}
					defer object.Body.Close()
					if aws.ToString(object.ContentRange) == "" {
						return errRangesUnsupported
					}
					if aws.ToString(object.ETag) != etag || aws.ToString(object.VersionId) != version {
						return errDownloadIdentity
					}
					expected := "bytes " + strconv.FormatInt(start, 10) + "-" + strconv.FormatInt(start+length-1, 10) + "/" + strconv.FormatInt(size, 10)
					if aws.ToString(object.ContentRange) != expected || aws.ToInt64(object.ContentLength) != length {
						return errors.New("Storage returned an invalid download range")
					}
					h := sha256.New()
					reader := &progressReader{Reader: object.Body, update: func(n int64) { a.checkpointProgress(t, live.Add(n), resumed) }}
					n, e := io.Copy(io.MultiWriter(io.NewOffsetWriter(file, start), h), io.LimitReader(reader, length))
					if e != nil {
						return e
					}
					if n != length {
						return io.ErrUnexpectedEOF
					}
					var extra [1]byte
					if n, err := object.Body.Read(extra[:]); n != 0 || err != io.EOF {
						return errors.New("Storage returned excess range data")
					}
					if e = workCtx.Err(); e != nil {
						return e
					}
					if e = file.Sync(); e != nil {
						return e
					}
					return a.saveCheckpointBlock(t, index, hex.EncodeToString(h.Sum(nil)))
				}()
				if e != nil {
					fail(e)
					return
				}
			}
		}()
	}
	group.Wait()
	if firstErr == nil {
		firstErr = ctx.Err()
	}
	if firstErr != nil {
		a.mu.Lock()
		current := a.transfers[t.ID]
		cancelled := current == nil || current.userCancelled
		var retained *transferCheckpoint
		if current != nil {
			retained = current.checkpoint
		}
		a.mu.Unlock()
		if cancelled || errors.Is(firstErr, errRangesUnsupported) || errors.Is(firstErr, errDownloadIdentity) || checkpointBytes(retained) == 0 {
			file.Close()
			if e := a.discardDownloadCheckpoint(t); e != nil {
				return true, e
			}
		} else {
			a.checkpointProgress(t, checkpointBytes(retained), resumed)
		}
		if errors.Is(firstErr, errRangesUnsupported) {
			return false, nil
		}
		return true, firstErr
	}
	if e := file.Sync(); e != nil {
		return true, e
	}
	if e := file.Close(); e != nil {
		return true, e
	}
	if e := ctx.Err(); e != nil {
		return true, e
	}
	var e error
	if t.destination != nil {
		if t.destination.Overwrite {
			e = os.Rename(path, t.destination.Path)
		} else {
			e = os.Link(path, t.destination.Path)
			if e == nil {
				e = os.Remove(path)
			}
		}
	} else {
		e = os.Rename(path, t.Path)
	}
	if e != nil {
		return true, e
	}
	return true, a.finishCheckpointedTransfer(t, size)
}
func verifyPublishedDownload(path string, c *transferCheckpoint) bool {
	info, e := os.Lstat(path)
	if e != nil || !info.Mode().IsRegular() || info.Size() != c.Size {
		return false
	}
	file, e := os.Open(path)
	if e != nil {
		return false
	}
	defer file.Close()
	opened, e := file.Stat()
	if e != nil || !os.SameFile(info, opened) {
		return false
	}
	for index, want := range c.Blocks {
		start := int64(index) * c.ChunkSize
		got, e := digestRange(file, start, min(c.ChunkSize, c.Size-start))
		if e != nil || got != want {
			return false
		}
	}
	return true
}

// Commit completion and retire recovery state together. If the journal write
// fails, the old checkpoint can recognize the already published file on Retry.
func (a *App) finishCheckpointedTransfer(t Transfer, size int64) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	current := a.transfers[t.ID]
	if current == nil || current.attempt != t.attempt {
		return errors.New("Transfer attempt changed")
	}
	previous := *current
	current.Status = "completed"
	current.Done, current.Total = size, size
	current.checkpoint, current.partialPath, current.Resumable = nil, "", false
	current.multipartID, current.multipartScope = "", ""
	if current.destination != nil {
		current.SavedPath, current.SavedAt, current.SaveError = current.destination.Path, time.Now(), ""
	}
	if err := a.persistTransferLocked(current); err != nil {
		*current = previous
		return err
	}
	return nil
}
