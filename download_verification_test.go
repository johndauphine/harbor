package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
)

// Cancel at a known context check inside hashing rather than racing a large
// file with a wall-clock delay. The backing context still supplies Done/Err.
type hashingCancelContext struct {
	context.Context
	cancel           context.CancelFunc
	checks, cancelAt int
}

func (c *hashingCancelContext) Err() error {
	c.checks++
	if c.checks == c.cancelAt {
		c.cancel()
	}
	return c.Context.Err()
}
func cancelDuringHashing(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	return &hashingCancelContext{Context: ctx, cancel: cancel, cancelAt: 6}
}

func TestDigestRangeChecksCancellationBetweenReads(t *testing.T) {
	file, err := os.Create(filepath.Join(t.TempDir(), "sparse"))
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	if err = file.Truncate(downloadChunkSize); err != nil {
		t.Fatal(err)
	}
	ctx := cancelDuringHashing(t)
	if _, err = digestRange(ctx, file, 0, downloadChunkSize); !errors.Is(err, context.Canceled) {
		t.Fatalf("hash ignored cancellation: %v", err)
	}
	got, err := digestRange(context.Background(), file, 0, downloadChunkSize)
	if err != nil || got == "" {
		t.Fatalf("hash could not retry after cancellation: %v", err)
	}
}

func TestDownloadVerificationCancellationPreservesRecovery(t *testing.T) {
	for _, published := range []bool{false, true} {
		t.Run(map[bool]string{false: "retained", true: "published"}[published], func(t *testing.T) {
			a := NewApp()
			client := s3.New(s3.Options{Region: "us-east-1"})
			tr := &Transfer{ID: id(), attempt: 1, Direction: "download", Path: filepath.Join(t.TempDir(), "download"), Status: "running"}
			cp := &transferCheckpoint{Version: 1, Kind: "download", Scope: multipartClientScope(client), Size: 96 << 20, ChunkSize: downloadChunkSize, ETag: "fixture", FileAttempt: 1, Blocks: map[int]string{}}
			tr.checkpoint = cp
			tr.partialPath = a.partialDownloadPath(*tr)
			file, err := os.Create(tr.partialPath)
			if err != nil {
				t.Fatal(err)
			}
			if err = file.Truncate(cp.Size); err != nil {
				t.Fatal(err)
			}
			count := checkpointChunks(cp)
			if !published {
				count--
			}
			for index := 0; index < count; index++ {
				start := int64(index) * cp.ChunkSize
				cp.Blocks[index], err = digestRange(context.Background(), file, start, min(cp.ChunkSize, cp.Size-start))
				if err != nil {
					t.Fatal(err)
				}
			}
			file.Close()
			if published {
				if err = os.Rename(tr.partialPath, tr.Path); err != nil {
					t.Fatal(err)
				}
			}
			a.transfers[tr.ID] = tr
			before := cloneCheckpoint(cp)
			ctx := cancelDuringHashing(t)
			handled, err := a.parallelDownload(ctx, client, *tr, &s3.GetObjectOutput{ContentLength: aws.Int64(cp.Size), ETag: aws.String(cp.ETag)})
			if !handled || !errors.Is(err, context.Canceled) {
				t.Fatalf("cancelled verification returned handled=%v err=%v", handled, err)
			}
			if !reflect.DeepEqual(cp, before) || tr.checkpoint != cp {
				t.Fatal("cancellation mutated durable checkpoint")
			}
			path := tr.partialPath
			if published {
				path = tr.Path
			}
			if _, err = os.Stat(path); err != nil {
				t.Fatalf("cancellation discarded recovery file: %v", err)
			}
			if len(a.tuning().preparation) != 0 {
				t.Fatal("verification retained preparation permit")
			}
			if published {
				valid, err := verifyPublishedDownload(context.Background(), path, cp)
				if err != nil || !valid {
					t.Fatalf("published verification could not retry: valid=%v err=%v", valid, err)
				}
			} else {
				file, err = os.OpenFile(path, os.O_RDWR, 0600)
				if err != nil {
					t.Fatal(err)
				}
				err = a.verifyRetainedDownload(context.Background(), file, cloneCheckpoint(cp))
				file.Close()
				if err != nil {
					t.Fatalf("retained verification could not retry: %v", err)
				}
			}
		})
	}
}

func TestDownloadVerificationPreparationWaitCancels(t *testing.T) {
	a := NewApp()
	for i := 0; i < cap(a.tuning().preparation); i++ {
		a.tuning().preparation <- struct{}{}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := a.verifyRetainedDownload(ctx, nil, &transferCheckpoint{}); !errors.Is(err, context.Canceled) {
		t.Fatalf("preparation ignored cancellation: %v", err)
	}
}
