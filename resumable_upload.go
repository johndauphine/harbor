package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/smithy-go"
)

const resumableUploadPartSize int64 = 5 << 20

// Durable staging remains the source of truth. Only parts acknowledged by both
// the journal and S3 may be skipped, and the whole staged source is rehashed.
func (a *App) resumableUpload(ctx context.Context, c *s3.Client, t Transfer, file *os.File, size int64) (resultErr error) {
	defer func() {
		if resultErr == nil {
			return
		}
		a.mu.Lock()
		current := a.transfers[t.ID]
		cancelled := current != nil && current.attempt == t.attempt && current.userCancelled
		if current != nil {
			t.multipartID, t.multipartScope = current.multipartID, current.multipartScope
		}
		a.mu.Unlock()
		if cancelled {
			cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
			defer cancel()
			if err := a.cleanupMultipart(cleanup, t); err != nil {
				a.mu.Lock()
				if current := a.transfers[t.ID]; current != nil && current.attempt == t.attempt {
					current.Resumable = false
				}
				a.mu.Unlock()
				resultErr = errors.Join(resultErr, &multipartCleanupError{})
				return
			}
			resultErr = errors.Join(resultErr, a.storeCheckpoint(t, nil, ""))
		}
	}()
	scope := multipartClientScope(c)
	cp := cloneCheckpoint(t.checkpoint)
	if cp != nil && cp.Scope != scope {
		return errors.New("Restore the original connection settings before resuming this upload")
	}
	info, err := file.Stat()
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() || size > 5<<30 {
		return errors.New("Staged upload changed or exceeds the 5 GiB limit; choose the local file again")
	}
	h := sha256.New()
	if _, err = io.Copy(h, &contextReader{ctx: ctx, reader: io.NewSectionReader(file, 0, size)}); err != nil {
		return err
	}
	sourceHash := hex.EncodeToString(h.Sum(nil))
	if cp != nil && (cp.Size != size || cp.SourceHash != sourceHash) {
		if err = a.cleanupMultipart(ctx, t); err != nil {
			return err
		}
		if err = a.storeCheckpoint(t, nil, ""); err != nil {
			return err
		}
		t.multipartID, t.multipartScope = "", ""
		cp = nil
	}
	if size < parallelDownloadThreshold {
		return errors.New("Staged upload size changed; Retry starts a fresh transfer")
	}
	if cp != nil && t.multipartID != "" {
		// Completion may have succeeded remotely before its response or the journal
		// commit was lost. A unique transfer marker plus source digest identifies it.
		head, e := c.HeadObject(ctx, &s3.HeadObjectInput{Bucket: aws.String(t.Bucket), Key: aws.String(t.Key)})
		if e == nil && aws.ToInt64(head.ContentLength) == size && head.Metadata["harbor-transfer"] == t.ID && head.Metadata["harbor-sha256"] == sourceHash {
			t.Total = size
			a.checkpointProgress(t, size, size)
			return a.finishCheckpointedTransfer(t, size)
		}
		// Missing objects are normal while uploading; other HEAD failures cannot
		// prove completion, so ListParts remains authoritative for an active upload.
		verified := map[int]string{}
		pages := s3.NewListPartsPaginator(c, &s3.ListPartsInput{Bucket: aws.String(t.Bucket), Key: aws.String(t.Key), UploadId: aws.String(t.multipartID)})
		for pages.HasMorePages() {
			page, e := pages.NextPage(ctx)
			if e != nil {
				var api smithy.APIError
				if errors.As(e, &api) && api.ErrorCode() == "NoSuchUpload" {
					if err = a.setMultipart(t, "", ""); err != nil {
						return err
					}
					if err = a.storeCheckpoint(t, nil, ""); err != nil {
						return err
					}
					t.multipartID, t.multipartScope = "", ""
					cp = nil
					break
				}
				return e
			}
			for _, part := range page.Parts {
				index := int(aws.ToInt32(part.PartNumber)) - 1
				if index < 0 || index >= checkpointChunks(cp) {
					continue
				}
				want := min(cp.ChunkSize, size-int64(index)*cp.ChunkSize)
				if etag, ok := cp.Blocks[index]; ok && etag == aws.ToString(part.ETag) && aws.ToInt64(part.Size) == want {
					verified[index] = etag
				}
			}
		}
		if cp != nil {
			cp.Blocks = verified
			if err = a.storeCheckpoint(t, cp, ""); err != nil {
				return err
			}
		}
	}
	if cp == nil || t.multipartID == "" {
		cp = &transferCheckpoint{Version: 1, Kind: "upload", Scope: scope, Size: size, ChunkSize: resumableUploadPartSize, FileAttempt: t.attempt, SourceHash: sourceHash, Blocks: map[int]string{}}
		owner := &cleanupUploadClient{Client: c, app: a, transfer: t, scope: scope}
		created, e := owner.CreateMultipartUpload(ctx, &s3.CreateMultipartUploadInput{Bucket: aws.String(t.Bucket), Key: aws.String(t.Key), Metadata: map[string]string{"harbor-transfer": t.ID, "harbor-sha256": sourceHash}})
		if e != nil {
			return e
		}
		t.multipartID, t.multipartScope = aws.ToString(created.UploadId), scope
		if t.multipartID == "" {
			return errors.New("Storage returned an empty multipart upload ID")
		}
		if err = a.storeCheckpoint(t, cp, ""); err != nil {
			cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
			defer cancel()
			return errors.Join(err, a.cleanupMultipart(cleanup, t))
		}
	}
	t.Total = size
	resumed := checkpointBytes(cp)
	a.checkpointProgress(t, resumed, resumed)
	var acknowledged atomic.Int64
	acknowledged.Store(resumed)
	workCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	var once sync.Once
	var firstErr error
	var next atomic.Int64
	var group sync.WaitGroup
	fail := func(err error) { once.Do(func() { firstErr = err; cancel() }) }
	slots := a.transferSlots()
	for worker := 0; worker < min(5, checkpointChunks(cp)); worker++ {
		group.Add(1)
		go func() {
			defer group.Done()
			for {
				index := int(next.Add(1) - 1)
				if index >= checkpointChunks(cp) {
					return
				}
				if _, ok := cp.Blocks[index]; ok {
					continue
				}
				select {
				case slots <- struct{}{}:
				case <-workCtx.Done():
					return
				}
				start := int64(index) * cp.ChunkSize
				length := min(cp.ChunkSize, size-start)
				out, e := c.UploadPart(workCtx, &s3.UploadPartInput{Bucket: aws.String(t.Bucket), Key: aws.String(t.Key), UploadId: aws.String(t.multipartID), PartNumber: aws.Int32(int32(index + 1)), ContentLength: aws.Int64(length), Body: io.NewSectionReader(file, start, length)})
				<-slots
				if e == nil {
					if aws.ToString(out.ETag) == "" {
						e = errors.New("Storage did not acknowledge the uploaded part")
					} else {
						e = a.saveCheckpointBlock(t, index, aws.ToString(out.ETag))
					}
				}
				if e != nil {
					fail(e)
					return
				}
				a.checkpointProgress(t, acknowledged.Add(length), resumed)
			}
		}()
	}
	group.Wait()
	if firstErr != nil {
		return firstErr
	}
	if err = ctx.Err(); err != nil {
		return err
	}
	after, err := file.Stat()
	if err != nil {
		return err
	}
	if after.Size() != size || !after.ModTime().Equal(info.ModTime()) {
		return errors.New("Staged upload changed during transfer; retry to verify the source again")
	}
	a.mu.Lock()
	completed := cloneCheckpoint(a.transfers[t.ID].checkpoint)
	a.mu.Unlock()
	parts := make([]types.CompletedPart, checkpointChunks(cp))
	for i := range parts {
		etag, ok := completed.Blocks[i]
		if !ok {
			return fmt.Errorf("Upload part %d is not acknowledged", i+1)
		}
		parts[i] = types.CompletedPart{PartNumber: aws.Int32(int32(i + 1)), ETag: aws.String(etag)}
	}
	_, err = c.CompleteMultipartUpload(ctx, &s3.CompleteMultipartUploadInput{Bucket: aws.String(t.Bucket), Key: aws.String(t.Key), UploadId: aws.String(t.multipartID), MultipartUpload: &types.CompletedMultipartUpload{Parts: parts}})
	if err != nil {
		return err
	}
	return a.finishCheckpointedTransfer(t, size)
}
