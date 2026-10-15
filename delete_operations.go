package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/smithy-go"
)

const deletePlanLimit = 10000
const deletePlanLifetime = 10 * time.Minute

type DeletePreview struct {
	PlanID           string
	Kind             string
	Bucket           string
	Prefix           string
	Confirmation     string
	Objects          int
	Versions         int
	DeleteMarkers    int
	MultipartUploads int
	TotalBytes       int64
	Keys             []string
	ExpiresAt        time.Time
}
type DeleteFailure struct {
	Key   string
	Error string
}
type DeleteResult struct {
	Deleted       int
	Aborted       int
	Failures      []DeleteFailure
	Completed     bool
	BucketDeleted bool
	Remaining     string
}
type deleteItem struct {
	Key, VersionID, ETag, UploadID string
	Marker                         bool
	IsLatest                       bool
	Size                           int64
}
type deletePlan struct {
	DeletePreview
	ProfileID, Identity string
	Items               []deleteItem
}
type storageMutation struct{ ProfileID, Bucket, Key string }

// Called with profileMu held, preserving the global profileMu -> mu order.
func (a *App) profileDeletionActive(profileID string) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	for _, p := range a.activeDeletes {
		if p.ProfileID == profileID {
			return true
		}
	}
	return false
}
func (a *App) beginStorageMutation(profile, bucket, key string) (func(), error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.closing {
		return nil, errShuttingDown
	}
	for _, p := range a.activeDeletes {
		if p.ProfileID == profile && p.Bucket == bucket && (key == "" || p.Kind == "bucket" || strings.HasPrefix(key, p.Prefix)) {
			return nil, errors.New("Deletion is running in this scope; wait before changing storage")
		}
	}
	if a.storageMutations == nil {
		a.storageMutations = map[string]storageMutation{}
	}
	token := id()
	a.storageMutations[token] = storageMutation{profile, bucket, key}
	return func() { a.mu.Lock(); delete(a.storageMutations, token); a.mu.Unlock() }, nil
}

func validDeleteScope(kind, bucket, prefix string) error {
	if bucket == "" || strings.TrimSpace(bucket) != bucket || strings.ContainsAny(bucket, "/\\\r\n\x00") {
		return errors.New("Choose a valid bucket")
	}
	switch kind {
	case "folder":
		if prefix == "" || prefix == "/" || !strings.HasSuffix(prefix, "/") || len(prefix) > 1024 || strings.ContainsAny(prefix, "\x00\r\n") {
			return errors.New("Choose an exact, non-root folder prefix ending with a slash")
		}
	case "bucket":
		if prefix != "" {
			return errors.New("Bucket deletion cannot include a folder prefix")
		}
	default:
		return errors.New("Choose folder or bucket deletion")
	}
	return nil
}
func (a *App) deletionIdentity(profileID string) (string, error) {
	a.profileMu.Lock()
	defer a.profileMu.Unlock()
	for _, p := range a.profiles {
		if p.ID == profileID {
			raw, _ := json.Marshal([]any{p.ID, p.Endpoint, p.Region, p.PathStyle, p.CAFile, credentialID(p)})
			return string(raw), nil
		}
	}
	return "", errors.New("Connection no longer exists")
}

// deletionBlockedLocked shares transfer admission's mutex, so a queued transfer
// cannot slip between execution's active-transfer check and its reservation.
func (a *App) deletionBlockedLocked(profile, bucket, key string) bool {
	for _, p := range a.activeDeletes {
		if p.ProfileID == profile && p.Bucket == bucket && (p.Kind == "bucket" || strings.HasPrefix(key, p.Prefix)) {
			return true
		}
	}
	return false
}
func (a *App) reserveDelete(p *deletePlan) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.closing {
		return errShuttingDown
	}
	for _, other := range a.activeDeletes {
		if other.ProfileID == p.ProfileID && other.Bucket == p.Bucket && (p.Kind == "bucket" || other.Kind == "bucket" || strings.HasPrefix(p.Prefix, other.Prefix) || strings.HasPrefix(other.Prefix, p.Prefix)) {
			return errors.New("An overlapping deletion is already running")
		}
	}
	for _, t := range a.transfers {
		if t.ProfileID == p.ProfileID && t.Bucket == p.Bucket && (p.Kind == "bucket" || strings.HasPrefix(t.Key, p.Prefix)) && (t.Status == "running" || t.Status == "queued") {
			return errors.New("Wait for transfers in this scope to finish or cancel them first")
		}
	}
	for _, m := range a.storageMutations {
		if m.ProfileID == p.ProfileID && m.Bucket == p.Bucket && (m.Key == "" || p.Kind == "bucket" || strings.HasPrefix(m.Key, p.Prefix)) {
			return errors.New("A storage change is still running in this scope; wait before reviewing deletion")
		}
	}
	if a.activeDeletes == nil {
		a.activeDeletes = map[string]*deletePlan{}
	}
	a.activeDeletes[p.PlanID] = p
	return nil
}
func deleteOperationContext(ctx context.Context, a *App) (context.Context, func(), error) {
	if ctx == nil {
		ctx = context.Background()
	}
	ctx, done, err := a.beginOperation(ctx)
	if err != nil {
		return nil, nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Minute)
	return ctx, func() { cancel(); done() }, nil
}
func (a *App) PreviewDelete(ctx context.Context, profile, bucket, prefix, kind string) (DeletePreview, error) {
	var empty DeletePreview
	if err := validDeleteScope(kind, bucket, prefix); err != nil {
		return empty, err
	}
	ctx, done, err := deleteOperationContext(ctx, a)
	if err != nil {
		return empty, err
	}
	defer done()
	identity, err := a.deletionIdentity(profile)
	if err != nil {
		return empty, err
	}
	c, err := a.client(ctx, profile)
	if err != nil {
		return empty, err
	}
	current, err := a.deletionIdentity(profile)
	if err != nil || current != identity {
		return empty, errors.New("Connection changed; review deletion again")
	}
	p := &deletePlan{ProfileID: profile, Identity: identity, DeletePreview: DeletePreview{Kind: kind, Bucket: bucket, Prefix: prefix, Confirmation: prefix, Keys: []string{}}}
	if kind == "bucket" {
		p.Confirmation = bucket
	}
	// Refuse previews with active transfers as well as refusing execution.
	p.PlanID = "preview-" + id()
	if err = a.reserveDelete(p); err != nil {
		return empty, err
	}
	reservationID := p.PlanID
	defer func() { a.mu.Lock(); delete(a.activeDeletes, reservationID); a.mu.Unlock() }()
	current, err = a.deletionIdentity(profile)
	if err != nil || current != identity {
		return empty, errors.New("Connection changed; review deletion again")
	}
	p.Items, err = a.enumerateDelete(ctx, c, p)
	if err != nil {
		return empty, fmt.Errorf("Could not build complete deletion preview; nothing deleted: %w", err)
	}
	keys := map[string]bool{}
	for _, item := range p.Items {
		if item.UploadID != "" {
			p.MultipartUploads++
		} else if item.Marker {
			p.DeleteMarkers++
		} else {
			p.Versions++
			keys[item.Key] = true
			p.TotalBytes += item.Size
		}
	}
	p.Objects = len(keys)
	sample := map[string]bool{}
	for _, item := range p.Items {
		if !sample[item.Key] && len(p.Keys) < 8 {
			p.Keys = append(p.Keys, item.Key)
			sample[item.Key] = true
		}
	}
	token := make([]byte, 24)
	if _, err = rand.Read(token); err != nil {
		return empty, err
	}
	p.PlanID = hex.EncodeToString(token)
	p.ExpiresAt = time.Now().Add(deletePlanLifetime)
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.deletePlans == nil {
		a.deletePlans = map[string]*deletePlan{}
	}
	for id, old := range a.deletePlans {
		if time.Now().After(old.ExpiresAt) {
			delete(a.deletePlans, id)
		}
	}
	if len(a.deletePlans) >= 32 {
		return empty, errors.New("Too many pending deletion reviews; wait for an earlier preview to expire")
	}
	a.deletePlans[p.PlanID] = p
	return p.DeletePreview, nil
}

func (a *App) enumerateDelete(ctx context.Context, c *s3.Client, p *deletePlan) ([]deleteItem, error) {
	items := []deleteItem{}
	seen := map[string]bool{}
	add := func(item deleteItem) error {
		if item.Key == "" || !strings.HasPrefix(item.Key, p.Prefix) {
			return errors.New("Storage returned an object outside the reviewed scope")
		}
		if item.UploadID == "" && !item.Marker && (item.ETag == "" || item.ETag == "*") {
			return errors.New("An object has no usable ETag; deletion cannot be reviewed safely")
		}
		raw, _ := json.Marshal([]any{item.Key, item.VersionID, item.UploadID, item.Marker})
		identity := string(raw)
		if seen[identity] {
			return errors.New("Storage returned duplicate entries across deletion pages")
		}
		seen[identity] = true
		items = append(items, item)
		if len(items) > deletePlanLimit {
			return fmt.Errorf("Scope exceeds the %d-operation safety limit; choose a smaller folder", deletePlanLimit)
		}
		return nil
	}
	var keyMarker, versionMarker *string
	tokens := map[string]bool{}
	for {
		page, err := c.ListObjectVersions(ctx, &s3.ListObjectVersionsInput{Bucket: aws.String(p.Bucket), Prefix: aws.String(p.Prefix), KeyMarker: keyMarker, VersionIdMarker: versionMarker, MaxKeys: aws.Int32(1000)})
		if err != nil {
			return nil, err
		}
		for _, v := range page.Versions {
			vid := aws.ToString(v.VersionId)
			if vid == "" {
				vid = "null"
			}
			if err = add(deleteItem{Key: aws.ToString(v.Key), VersionID: vid, ETag: aws.ToString(v.ETag), IsLatest: aws.ToBool(v.IsLatest), Size: aws.ToInt64(v.Size)}); err != nil {
				return nil, err
			}
		}
		for _, v := range page.DeleteMarkers {
			vid := aws.ToString(v.VersionId)
			if vid == "" {
				vid = "null"
			}
			if err = add(deleteItem{Key: aws.ToString(v.Key), VersionID: vid, Marker: true, IsLatest: aws.ToBool(v.IsLatest)}); err != nil {
				return nil, err
			}
		}
		if !aws.ToBool(page.IsTruncated) {
			break
		}
		keyMarker, versionMarker = page.NextKeyMarker, page.NextVersionIdMarker
		token := aws.ToString(keyMarker) + "\x00" + aws.ToString(versionMarker)
		if aws.ToString(keyMarker) == "" || tokens[token] {
			return nil, errors.New("Storage returned an invalid version pagination cursor")
		}
		tokens[token] = true
	}
	var uploadKey, uploadID *string
	tokens = map[string]bool{}
	for {
		page, err := c.ListMultipartUploads(ctx, &s3.ListMultipartUploadsInput{Bucket: aws.String(p.Bucket), Prefix: aws.String(p.Prefix), KeyMarker: uploadKey, UploadIdMarker: uploadID, MaxUploads: aws.Int32(1000)})
		if err != nil {
			return nil, err
		}
		for _, v := range page.Uploads {
			if aws.ToString(v.UploadId) == "" {
				return nil, errors.New("Storage returned a multipart upload without an ID")
			}
			if err = add(deleteItem{Key: aws.ToString(v.Key), UploadID: aws.ToString(v.UploadId)}); err != nil {
				return nil, err
			}
		}
		if !aws.ToBool(page.IsTruncated) {
			break
		}
		uploadKey, uploadID = page.NextKeyMarker, page.NextUploadIdMarker
		token := aws.ToString(uploadKey) + "\x00" + aws.ToString(uploadID)
		if aws.ToString(uploadKey) == "" || tokens[token] {
			return nil, errors.New("Storage returned an invalid multipart pagination cursor")
		}
		tokens[token] = true
	}
	// MinIO can omit persisted multipart uploads from an unprefixed listing after
	// restart. Check exact journal-owned IDs against the remote server as well.
	a.mu.Lock()
	owned := []deleteItem{}
	for _, t := range a.transfers {
		if t.ProfileID == p.ProfileID && t.Bucket == p.Bucket && strings.HasPrefix(t.Key, p.Prefix) && t.multipartID != "" && t.multipartScope == multipartClientScope(c) {
			owned = append(owned, deleteItem{Key: t.Key, UploadID: t.multipartID})
		}
	}
	a.mu.Unlock()
	for _, item := range owned {
		raw, _ := json.Marshal([]any{item.Key, item.VersionID, item.UploadID, item.Marker})
		if seen[string(raw)] {
			continue
		}
		_, err := c.ListParts(ctx, &s3.ListPartsInput{Bucket: aws.String(p.Bucket), Key: aws.String(item.Key), UploadId: aws.String(item.UploadID), MaxParts: aws.Int32(1)})
		if err != nil {
			var api smithy.APIError
			if errors.As(err, &api) && api.ErrorCode() == "NoSuchUpload" {
				continue
			}
			return nil, err
		}
		if err = add(item); err != nil {
			return nil, err
		}
	}
	sort.Slice(items, func(i, j int) bool {
		left, _ := json.Marshal(items[i])
		right, _ := json.Marshal(items[j])
		return string(left) < string(right)
	})
	return items, nil
}
func equalDeleteItems(a, b []deleteItem) bool {
	left, _ := json.Marshal(a)
	right, _ := json.Marshal(b)
	return string(left) == string(right)
}

func (a *App) ExecuteDelete(ctx context.Context, planID, confirmation string) (DeleteResult, error) {
	result := DeleteResult{Failures: []DeleteFailure{}}
	ctx, done, err := deleteOperationContext(ctx, a)
	if err != nil {
		return result, err
	}
	defer done()
	a.mu.Lock()
	p := a.deletePlans[planID]
	if p == nil || time.Now().After(p.ExpiresAt) {
		delete(a.deletePlans, planID)
		a.mu.Unlock()
		return result, errors.New("Deletion preview expired or was already used; review the remaining contents again")
	}
	if confirmation != p.Confirmation {
		a.mu.Unlock()
		return result, errors.New("Type the exact reviewed folder prefix or bucket name")
	}
	delete(a.deletePlans, planID)
	a.mu.Unlock() // Single-use even if execution fails.
	identity, err := a.deletionIdentity(p.ProfileID)
	if err != nil || identity != p.Identity {
		return result, errors.New("Connection changed after review; nothing deleted")
	}
	c, err := a.client(ctx, p.ProfileID)
	if err != nil {
		return result, err
	}
	identity, err = a.deletionIdentity(p.ProfileID)
	if err != nil || identity != p.Identity {
		return result, errors.New("Connection changed after review; nothing deleted")
	}
	if err = a.reserveDelete(p); err != nil {
		return result, err
	}
	defer func() { a.mu.Lock(); delete(a.activeDeletes, p.PlanID); a.mu.Unlock() }()
	identity, err = a.deletionIdentity(p.ProfileID)
	if err != nil || identity != p.Identity {
		return result, errors.New("Connection changed after review; nothing deleted")
	}
	current, err := a.enumerateDelete(ctx, c, p)
	if err != nil {
		return result, fmt.Errorf("Could not verify deletion preview; nothing deleted: %w", err)
	}
	if !equalDeleteItems(p.Items, current) {
		result.Remaining = "Contents changed since review. Nothing was deleted; review the current contents again."
		return result, nil
	}
	// AWS rejects If-Match together with versionId. Ordinary, never-versioned
	// objects can instead use a conditional DELETE without versionId. Mutable
	// null versions in enabled/suspended buckets cannot be purged atomically.
	nullObjectsUnversioned := false
	for _, item := range p.Items {
		if !item.Marker && item.UploadID == "" && item.VersionID == "null" {
			versioning, versionErr := c.GetBucketVersioning(ctx, &s3.GetBucketVersioningInput{Bucket: aws.String(p.Bucket)})
			if versionErr != nil {
				return result, fmt.Errorf("Could not verify bucket versioning; nothing deleted: %w", versionErr)
			}
			nullObjectsUnversioned = versioning.Status == ""
			break
		}
	}
	// Remove content before delete markers. A marker must continue hiding any
	// retained or newly introduced versions when part of an operation fails.
	ordered := append([]deleteItem(nil), p.Items...)
	rank := func(item deleteItem) int {
		if item.Marker {
			return 2
		}
		if item.IsLatest {
			return 1
		}
		return 0
	}
	sort.SliceStable(ordered, func(i, j int) bool { return rank(ordered[i]) < rank(ordered[j]) })
	blockedMarkers := map[string]bool{}
	checkedMarkers := false
	for _, item := range ordered {
		if err = ctx.Err(); err != nil {
			result.Failures = append(result.Failures, DeleteFailure{Key: item.Key, Error: "Deletion interrupted: " + err.Error()})
			break
		}
		if item.Marker && !checkedMarkers {
			left, verifyErr := a.enumerateDelete(ctx, c, p)
			checkedMarkers = true
			if verifyErr != nil {
				for _, m := range ordered {
					if m.Marker {
						blockedMarkers[m.Key] = true
					}
				}
			} else {
				for _, m := range left {
					if !m.Marker && m.UploadID == "" {
						blockedMarkers[m.Key] = true
					}
				}
			}
		}
		if item.UploadID != "" {
			_, err = c.AbortMultipartUpload(ctx, &s3.AbortMultipartUploadInput{Bucket: aws.String(p.Bucket), Key: aws.String(item.Key), UploadId: aws.String(item.UploadID)})
			if err == nil {
				result.Aborted++
			}
		} else if !item.Marker && item.IsLatest && blockedMarkers[item.Key] {
			err = errors.New("Current version preserved because an older content version could not be deleted; retained older data stays hidden")
		} else if item.Marker && blockedMarkers[item.Key] {
			err = errors.New("Delete marker preserved because content versions remain or could not be verified; retained data stays hidden")
		} else if item.Marker && item.VersionID == "null" {
			err = errors.New("A mutable null-version delete marker cannot be conditionally removed safely; remove it in your storage console and review again")
		} else {
			in := &s3.DeleteObjectInput{Bucket: aws.String(p.Bucket), Key: aws.String(item.Key), VersionId: aws.String(item.VersionID)}
			err = nil
			if !item.Marker {
				if item.VersionID == "null" && (!nullObjectsUnversioned || !item.IsLatest) {
					err = errors.New("Mutable null version preserved: this bucket has versioning history or the object is not current; AWS cannot conditionally purge this version. Review it in your storage console")
				} else if item.VersionID == "null" {
					in.VersionId = nil
					in.IfMatch = aws.String(item.ETag)
					// Retain a preflight even for providers that ignore If-Match on
					// DELETE. They cannot offer atomic protection against external writes.
					head, headErr := c.HeadObject(ctx, &s3.HeadObjectInput{Bucket: in.Bucket, Key: in.Key, VersionId: aws.String(item.VersionID), IfMatch: in.IfMatch})
					if headErr != nil {
						err = fmt.Errorf("Could not verify null-version object: %w", headErr)
					} else if aws.ToString(head.ETag) != item.ETag {
						err = errors.New("Object changed after review; replacement was preserved")
					}
				}
			}
			if err == nil {
				_, err = c.DeleteObject(ctx, in)
			}
			if err == nil {
				result.Deleted++
			}
		}
		if err != nil {
			result.Failures = append(result.Failures, DeleteFailure{Key: item.Key, Error: err.Error()})
			if !item.Marker && item.UploadID == "" {
				blockedMarkers[item.Key] = true
			}
		}
	}
	remaining, verifyErr := a.enumerateDelete(ctx, c, p)
	if verifyErr != nil {
		result.Remaining = "Deletion made partial progress, but remaining contents could not be verified: " + verifyErr.Error()
		return result, nil
	}
	if len(remaining) > 0 {
		result.Remaining = fmt.Sprintf("%d object versions, delete markers, or multipart uploads remain. New or changed contents were preserved; review remaining contents before deleting again.", len(remaining))
		return result, nil
	}
	if len(result.Failures) > 0 {
		result.Remaining = "Some operations failed; review remaining contents before deleting again."
		return result, nil
	}
	if p.Kind == "bucket" {
		_, err = c.DeleteBucket(ctx, &s3.DeleteBucketInput{Bucket: aws.String(p.Bucket)})
		if err != nil {
			result.Failures = append(result.Failures, DeleteFailure{Key: p.Bucket, Error: err.Error()})
			result.Remaining = "Bucket was not deleted. It may contain new objects, retained versions, or uploads; review remaining contents."
			return result, nil
		}
		result.BucketDeleted = true
	}
	result.Completed = true
	return result, nil
}
