package main

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
)

// These opt-in cases use the same isolated real MinIO fixture as the transfer
// resilience suite. No saved user connection or existing bucket is accessed.
func TestMinIODeleteFolderSnapshotAndSiblingProtection(t *testing.T) {
	f := newMinIOResilience(t)
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	put := func(key, value string) {
		t.Helper()
		if _, err := f.client.PutObject(ctx, &s3.PutObjectInput{Bucket: aws.String(f.bucket), Key: aws.String(key), Body: strings.NewReader(value)}); err != nil {
			t.Fatal(err)
		}
	}
	put("remove/", "")
	put("remove/child.txt", "original")
	put("remove/nested/data.txt", "nested")
	put("remove-keep.txt", "sibling")
	plan, err := f.a.PreviewDelete(ctx, f.profile.ID, f.bucket, "remove/", "folder")
	if err != nil {
		t.Fatal(err)
	}
	if plan.Objects != 3 || plan.Confirmation != "remove/" {
		t.Fatalf("wrong exact-prefix preview: %+v", plan)
	}
	// The reviewed prefix changes before confirmation: absolutely no mutation is
	// permitted until the user reviews and confirms a fresh snapshot.
	put("remove/child.txt", "replacement")
	result, err := f.a.ExecuteDelete(ctx, plan.PlanID, "remove/")
	if err != nil {
		t.Fatal(err)
	}
	if result.Completed || result.Deleted != 0 || result.Remaining == "" {
		t.Fatalf("changed preview was executed: %+v", result)
	}
	plan, err = f.a.PreviewDelete(ctx, f.profile.ID, f.bucket, "remove/", "folder")
	if err != nil {
		t.Fatal(err)
	}
	result, err = f.a.ExecuteDelete(ctx, plan.PlanID, "remove/")
	if err != nil || !result.Completed || result.Deleted != 3 {
		t.Fatalf("folder delete: %+v, %v", result, err)
	}
	page, err := f.client.ListObjectsV2(ctx, &s3.ListObjectsV2Input{Bucket: aws.String(f.bucket)})
	if err != nil || len(page.Contents) != 1 || aws.ToString(page.Contents[0].Key) != "remove-keep.txt" {
		t.Fatalf("sibling not preserved: %+v, %v", page, err)
	}
	t.Log("real MinIO rejected changed preview without deleting; fresh confirmation removed folder marker and nested contents, preserving similarly prefixed sibling")
}

func TestMinIODeleteNonemptyVersionedBucket(t *testing.T) {
	f := newMinIOResilience(t)
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	_, err := f.client.PutBucketVersioning(ctx, &s3.PutBucketVersioningInput{Bucket: aws.String(f.bucket), VersioningConfiguration: &types.VersioningConfiguration{Status: types.BucketVersioningStatusEnabled}})
	if err != nil {
		t.Fatal(err)
	}
	for _, value := range []string{"older", "newer"} {
		_, err = f.client.PutObject(ctx, &s3.PutObjectInput{Bucket: aws.String(f.bucket), Key: aws.String("hidden.txt"), Body: strings.NewReader(value)})
		if err != nil {
			t.Fatal(err)
		}
	}
	_, err = f.client.DeleteObject(ctx, &s3.DeleteObjectInput{Bucket: aws.String(f.bucket), Key: aws.String("hidden.txt")})
	if err != nil {
		t.Fatal(err)
	}
	_, err = f.client.PutObject(ctx, &s3.PutObjectInput{Bucket: aws.String(f.bucket), Key: aws.String("current.txt"), Body: strings.NewReader("current")})
	if err != nil {
		t.Fatal(err)
	}
	upload, err := f.client.CreateMultipartUpload(ctx, &s3.CreateMultipartUploadInput{Bucket: aws.String(f.bucket), Key: aws.String("unfinished.bin")})
	if err != nil {
		t.Fatal(err)
	}
	_, err = f.client.UploadPart(ctx, &s3.UploadPartInput{Bucket: aws.String(f.bucket), Key: aws.String("unfinished.bin"), UploadId: upload.UploadId, PartNumber: aws.Int32(1), Body: strings.NewReader(strings.Repeat("x", 5<<20))})
	if err != nil {
		t.Fatal(err)
	}
	plan, err := f.a.PreviewDelete(ctx, f.profile.ID, f.bucket, "", "bucket")
	if err != nil {
		t.Fatal(err)
	}
	if plan.Versions != 3 || plan.DeleteMarkers != 1 || plan.MultipartUploads != 1 || plan.Confirmation != f.bucket {
		t.Fatalf("incomplete bucket preview: %+v", plan)
	}
	if _, err = f.a.ExecuteDelete(ctx, plan.PlanID, "wrong-name"); err == nil {
		t.Fatal("wrong bucket confirmation accepted")
	}
	result, err := f.a.ExecuteDelete(ctx, plan.PlanID, f.bucket)
	if err != nil || !result.Completed || !result.BucketDeleted || result.Deleted != 4 || result.Aborted != 1 || len(result.Failures) != 0 {
		t.Fatalf("versioned bucket delete: %+v, %v", result, err)
	}
	if _, err = f.client.HeadBucket(ctx, &s3.HeadBucketInput{Bucket: aws.String(f.bucket)}); err == nil {
		t.Fatal("bucket survived confirmed deletion")
	}
	t.Log("real MinIO: current and historical versions plus delete marker removed, incomplete multipart aborted, and nonempty versioned bucket deleted after exact confirmation")
}
