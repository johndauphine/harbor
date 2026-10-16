package main

import (
	"context"
	"os"
	"testing"
	"time"
)

func TestMinIOAdaptiveRecovery(t *testing.T) {
	for _, direction := range []string{"upload", "download"} {
		t.Run(direction, func(t *testing.T) {
			f := newMinIOResilience(t)
			f.payload = make([]byte, 96<<20)
			for i := range f.payload {
				f.payload[i] = byte((i*31 + i/7919) % 251)
			}
			if err := os.WriteFile(f.source, f.payload, 0600); err != nil {
				t.Fatal(err)
			}
			if err := f.a.SetTransferTuningMode("auto"); err != nil {
				t.Fatal(err)
			}
			if direction == "download" {
				f.seedDownload()
			}
			fault := "hold-upload"
			if direction == "download" {
				fault = "hold-ranges"
			}
			f.setFault(fault)
			var tid, path string
			var original []byte
			if direction == "upload" {
				tid = f.upload("adaptive.bin")
			} else {
				result, prior := f.download()
				tid, path, original = result.ID, result.Path, prior
			}
			f.awaitFault()
			deadline := time.Now().Add(10 * time.Second)
			for {
				f.a.mu.Lock()
				tr := *f.a.transfers[tid]
				f.a.mu.Unlock()
				if tr.Resumable {
					break
				}
				if time.Now().After(deadline) {
					t.Fatal("no verified progress before adaptive downshift")
				}
				time.Sleep(time.Millisecond)
			}
			f.a.tuning().scheduler.SetResourceCeiling(1)
			if f.a.tuning().scheduler.Snapshot().BulkLimit != 1 {
				t.Fatal("resource downshift ignored")
			}
			if err := f.a.shutdown(context.Background()); err != nil {
				t.Fatal(err)
			}
			if direction == "download" {
				assertMinIOFile(t, path, original)
			}
			f.setFault("")
			previous := f.a
			f.a = NewApp()
			f.a.configFile = previous.configFile
			f.a.secretGet = previous.secretGet
			f.a.secretSet = previous.secretSet
			f.a.secretDelete = previous.secretDelete
			if err := f.a.startup(context.Background()); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				if err := f.a.shutdown(context.Background()); err != nil {
					t.Error(err)
				}
			})
			if f.a.TransferTuningStatus().Scheduler.Mode != "auto" {
				t.Fatal("automatic preference lost")
			}
			if err := f.a.RetryTransfer(tid); err != nil {
				t.Fatal(err)
			}
			tr := minioWait(t, f.a, tid)
			if tr.Status != "completed" || tr.ResumedBytes <= 0 {
				t.Fatalf("adaptive recovery failed: %+v", tr)
			}
			if direction == "upload" {
				f.verifyObject("adaptive.bin")
			} else {
				assertMinIOFile(t, path, f.payload)
			}
			f.assertNoMultipart()
			status := f.a.TransferTuningStatus().Scheduler
			if status.ActiveBulk != 0 || status.WaitingBulk != 0 || status.ActiveControl != 0 {
				t.Fatalf("restart left admission work: %+v", status)
			}
		})
	}
}
