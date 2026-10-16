package main

import (
	"bytes"
	"context"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// Full process termination, real MinIO parts/ranges, and a fresh Harbor journal
// reader establish that recovery skips data rather than merely retrying it.
func TestMinIOCheckpointProcessRecovery(t *testing.T) {
	for _, direction := range []string{"upload", "download"} {
		for _, stop := range []string{"kill", "graceful", "cancel"} {
			t.Run(direction+"_"+stop, func(t *testing.T) {
				f := newMinIOResilience(t)
				f.payload = make([]byte, 96<<20)
				for i := range f.payload {
					f.payload[i] = byte((i*31 + i/7919) % 251)
				}
				if err := os.WriteFile(f.source, f.payload, 0600); err != nil {
					t.Fatal(err)
				}
				if direction == "download" {
					f.seedDownload()
				}
				if err := f.a.shutdown(context.Background()); err != nil {
					t.Fatal(err)
				}
				worker := startMinIOWorker(t, f)
				fault := "hold-upload"
				if direction == "download" {
					fault = "hold-ranges"
				}
				f.setFault(fault)
				var tid string
				destination := filepath.Join(f.root, "saved-large.bin")
				original := []byte("keep existing destination")
				if direction == "upload" {
					f.keys = append(f.keys, "large-crash.bin")
					worker.api(t, "upload-local", map[string]string{"ProfileID": f.profile.ID, "Bucket": f.bucket, "Key": "large-crash.bin", "Path": f.source}, &tid)
				} else {
					if err := os.WriteFile(destination, original, 0600); err != nil {
						t.Fatal(err)
					}
					var result SaveResult
					worker.api(t, "download", map[string]any{"ProfileID": f.profile.ID, "Bucket": f.bucket, "Key": "download.bin", "Path": f.root, "Name": "saved-large.bin", "Overwrite": true}, &result)
					tid = result.ID
				}
				f.awaitFault()
				deadline := time.Now().Add(15 * time.Second)
				for {
					var transfers []Transfer
					worker.api(t, "transfers", map[string]string{}, &transfers)
					ready := false
					for _, tr := range transfers {
						if tr.ID == tid && tr.Resumable {
							ready = true
						}
					}
					if ready {
						break
					}
					if time.Now().After(deadline) {
						t.Fatal("no verified checkpoint before stop")
					}
					time.Sleep(20 * time.Millisecond)
				}
				if stop == "cancel" {
					worker.api(t, "cancel", map[string]string{"ID": tid}, nil)
					tr := worker.wait(t, tid)
					if tr.Status != "cancelled" || tr.Resumable {
						t.Fatalf("cancel did not discard progress: %+v", tr)
					}
					f.assertNoMultipart()
					if direction == "download" {
						assertNoMinIOPartials(t, f.root)
						assertMinIOFile(t, destination, original)
					}
					f.setFault("")
					worker.api(t, "retry", map[string]string{"ID": tid}, nil)
					tr = worker.wait(t, tid)
					if tr.Status != "completed" || tr.ResumedBytes != 0 {
						t.Fatalf("cancel retry: %+v", tr)
					}
				} else {
					if stop == "kill" {
						if err := worker.cmd.Process.Kill(); err != nil {
							t.Fatal(err)
						}
					} else {
						if err := worker.cmd.Process.Signal(os.Interrupt); err != nil {
							t.Fatal(err)
						}
					}
					err := worker.cmd.Wait()
					if stop == "graceful" && err != nil {
						t.Fatal(err)
					}
					f.setFault("")
					if direction == "download" {
						assertMinIOFile(t, destination, original)
					}
					worker = startMinIOWorker(t, f)
					tr := worker.wait(t, tid)
					if !tr.Resumable {
						t.Fatalf("restart lost resumable state: %+v", tr)
					}
					worker.api(t, "retry", map[string]string{"ID": tid}, nil)
					tr = worker.wait(t, tid)
					if tr.Status != "completed" || tr.ResumedBytes <= 0 {
						t.Fatalf("retry did not resume: %+v", tr)
					}
					t.Logf("resumed %d verified bytes after %s", tr.ResumedBytes, stop)
				}
				if direction == "upload" {
					f.verifyObject("large-crash.bin")
				} else {
					assertMinIOFile(t, destination, f.payload)
					assertNoMinIOPartials(t, f.root)
				}
				f.assertNoMultipart()
				if err := worker.cmd.Process.Signal(os.Interrupt); err != nil {
					t.Fatal(err)
				}
				if err := worker.cmd.Wait(); err != nil {
					t.Fatal(err)
				}
			})
		}
	}
}

func TestMinIOUploadCheckpointFailures(t *testing.T) {
	for _, scenario := range []string{"network", "expired", "changed-part", "changed-source", "shrunk-source", "completion", "cancel-cleanup"} {
		t.Run(scenario, func(t *testing.T) {
			f := newMinIOResilience(t)
			f.payload = make([]byte, 64<<20)
			for i := range f.payload {
				f.payload[i] = byte((i*31 + i/7919) % 251)
			}
			if err := os.WriteFile(f.source, f.payload, 0600); err != nil {
				t.Fatal(err)
			}
			fault := "drop-part-always"
			if scenario == "completion" {
				fault = "lose-completion"
			}
			if scenario == "cancel-cleanup" {
				fault = "hold-upload"
			}
			f.setFault(fault)
			tid := f.upload("checkpoint.bin")
			if scenario == "cancel-cleanup" {
				f.awaitFault()
				f.awaitStoredPart()
				f.setFault("offline")
				f.a.CancelTransfer(tid)
			}
			tr := minioWait(t, f.a, tid)
			if tr.Status != "failed" || (scenario != "cancel-cleanup" && !tr.Resumable) {
				t.Fatalf("failure lost acknowledged parts: %+v", tr)
			}
			f.setFault("")
			f.a.mu.Lock()
			spec := *f.a.transfers[tid]
			f.a.mu.Unlock()
			if scenario == "expired" {
				_, err := f.client.AbortMultipartUpload(context.Background(), &s3.AbortMultipartUploadInput{Bucket: aws.String(f.bucket), Key: aws.String(spec.Key), UploadId: aws.String(spec.multipartID)})
				if err != nil {
					t.Fatal(err)
				}
			}
			if scenario == "changed-part" {
				_, err := f.client.UploadPart(context.Background(), &s3.UploadPartInput{Bucket: aws.String(f.bucket), Key: aws.String(spec.Key), UploadId: aws.String(spec.multipartID), PartNumber: aws.Int32(1), Body: bytes.NewReader(make([]byte, 5<<20))})
				if err != nil {
					t.Fatal(err)
				}
			}
			if scenario == "changed-source" {
				f.payload[0] ^= 0xff
				file, err := os.OpenFile(spec.Path, os.O_WRONLY, 0600)
				if err != nil {
					t.Fatal(err)
				}
				_, err = file.WriteAt(f.payload[:1], 0)
				file.Close()
				if err != nil {
					t.Fatal(err)
				}
			}
			if scenario == "shrunk-source" {
				f.payload = f.payload[:32<<20]
				if err := os.Truncate(spec.Path, int64(len(f.payload))); err != nil {
					t.Fatal(err)
				}
			}
			if err := f.a.RetryTransfer(tid); err != nil {
				t.Fatal(err)
			}
			tr = minioWait(t, f.a, tid)
			if scenario == "shrunk-source" {
				if tr.Status != "failed" || tr.Resumable {
					t.Fatal("shrunk source retained incompatible checkpoint")
				}
				if err := f.a.RetryTransfer(tid); err != nil {
					t.Fatal(err)
				}
				tr = minioWait(t, f.a, tid)
			}
			if tr.Status != "completed" {
				t.Fatalf("retry failed: %+v", tr)
			}
			if scenario == "network" && tr.ResumedBytes <= 0 {
				t.Fatal("network retry did not resume")
			}
			if (scenario == "expired" || scenario == "changed-source" || scenario == "cancel-cleanup") && tr.ResumedBytes != 0 {
				t.Fatal("reused invalid or cancelled parts")
			}
			if scenario == "completion" && tr.ResumedBytes != int64(len(f.payload)) {
				t.Fatal("lost completion not recognized")
			}
			f.verifyObject("checkpoint.bin")
			f.assertNoMultipart()
		})
	}
}
