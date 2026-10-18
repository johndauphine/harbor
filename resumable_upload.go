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

const resumableUploadPartSize int64 = 16 << 20
const resumableUploadConcurrency = 8

func validUploadPartSize(size int64) bool { return size == 5<<20 || size == 16<<20 || size == 32<<20 }

// Durable staging remains the source of truth. Only parts acknowledged by both
// the journal and S3 may be skipped, and the whole staged source is rehashed.
func (a *App) resumableUpload(ctx context.Context, c *s3.Client, t Transfer, file *os.File, size int64) error {
	return a.uploadMultipart(ctx, c, t, file, size, resumableUploadPartSize, resumableUploadConcurrency)
}

// Settings are selected when an upload begins. A recovered upload retains its
// journaled part size, so tuning a later build cannot invalidate older parts.
func (a *App) uploadMultipart(ctx context.Context, c *s3.Client, t Transfer, file *os.File, size, partSize int64, concurrency int) (resultErr error) {
	if !validUploadPartSize(partSize) || concurrency < 1 || concurrency > 8 {
		return errors.New("Invalid multipart settings")
	}
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
			if err := a.cleanupMultipartWithClient(cleanup, c, t); err != nil {
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
		return fmt.Errorf("%w or exceeds the 5 GiB limit; choose the local file again", errStagedUploadChanged)
	}
	release, err := a.acquireTransferPreparation(ctx)
	if err != nil {
		return err
	}
	verifyStarted := time.Now()
	h := sha256.New()
	if _, err = io.Copy(h, &contextReader{ctx: ctx, reader: io.NewSectionReader(file, 0, size)}); err != nil {
		release()
		return err
	}
	release()
	a.recordTransferPhase("source verification", verifyStarted)
	sourceHash := hex.EncodeToString(h.Sum(nil))
	if cp != nil && (cp.Size != size || cp.SourceHash != sourceHash) {
		if err = a.cleanupMultipartWithClient(ctx, c, t); err != nil {
			return err
		}
		if err = a.storeCheckpoint(t, nil, ""); err != nil {
			return err
		}
		t.multipartID, t.multipartScope = "", ""
		cp = nil
	}
	if size < parallelDownloadThreshold {
		return fmt.Errorf("%w size; Retry starts a fresh transfer", errStagedUploadChanged)
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
		cp = &transferCheckpoint{Version: 1, Kind: "upload", Scope: scope, Size: size, ChunkSize: partSize, FileAttempt: t.attempt, SourceHash: sourceHash, Blocks: map[int]string{}}
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
			return errors.Join(err, a.cleanupMultipartWithClient(cleanup, c, t))
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
	for worker := 0; worker < min(concurrency, checkpointChunks(cp)); worker++ {
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
				if workCtx.Err() != nil {
					return
				}
				started := time.Now()
				start := int64(index) * cp.ChunkSize
				length := min(cp.ChunkSize, size-start)
				out, e := c.UploadPart(workCtx, &s3.UploadPartInput{Bucket: aws.String(t.Bucket), Key: aws.String(t.Key), UploadId: aws.String(t.multipartID), PartNumber: aws.Int32(int32(index + 1)), ContentLength: aws.Int64(length), Body: io.NewSectionReader(file, start, length)})
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
				a.observeUsefulTransfer(workCtx, length, started)
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
		return fmt.Errorf("%w during transfer; retry to verify the source again", errStagedUploadChanged)
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
