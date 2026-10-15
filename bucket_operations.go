package main

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	smithyhttp "github.com/aws/smithy-go/transport/http"
)

var bucketNamePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9.-]{1,61}[a-z0-9]$`)
var bucketIPPattern = regexp.MustCompile(`^[0-9]+\.[0-9]+\.[0-9]+\.[0-9]+$`)

func validateBucketName(name string) error {
	if !bucketNamePattern.MatchString(name) || strings.Contains(name, "..") || bucketIPPattern.MatchString(name) {
		return errors.New("Use 3–63 lowercase letters, numbers, periods or hyphens. Start and end with a letter or number; adjacent periods and IP addresses are not allowed")
	}
	for _, prefix := range []string{"xn--", "sthree-", "amzn-s3-demo-"} {
		if strings.HasPrefix(name, prefix) {
			return errors.New("This bucket name uses an S3-reserved prefix")
		}
	}
	for _, suffix := range []string{"-s3alias", "--ol-s3", ".mrap", "--x-s3", "--table-s3", "-an"} {
		if strings.HasSuffix(name, suffix) {
			return errors.New("This bucket name uses an S3-reserved suffix; choose a general purpose bucket name")
		}
	}
	return nil
}

func (a *App) CreateBucket(profileID, name string) (string, error) {
	if err := validateBucketName(name); err != nil {
		return "", err
	}
	release, err := a.beginStorageMutation(profileID, name, "")
	if err != nil {
		return "", err
	}
	defer release()
	ctx, cancel := context.WithTimeout(a.ctx, 30*time.Second)
	defer cancel()
	c, err := a.client(ctx, profileID)
	if err != nil {
		return "", err
	}
	// In us-east-1, creating an already owned bucket can reset its ACLs. Refuse
	// existing buckets and uncertain availability checks rather than modifying them.
	_, err = c.HeadBucket(ctx, &s3.HeadBucketInput{Bucket: aws.String(name)})
	if err == nil {
		return "", errors.New("A bucket with this name already exists. Choose another name")
	}
	var response *smithyhttp.ResponseError
	if !errors.As(err, &response) || response.HTTPStatusCode() != 404 {
		return "", fmt.Errorf("Could not check bucket availability; no bucket was created: %w", err)
	}
	input := &s3.CreateBucketInput{Bucket: aws.String(name)}
	region := c.Options().Region
	if region != "" && region != "us-east-1" {
		input.CreateBucketConfiguration = &types.CreateBucketConfiguration{LocationConstraint: types.BucketLocationConstraint(region)}
	}
	// Do not replay an ambiguous CreateBucket response: it is not harmless on all
	// providers. A subsequent user attempt must first pass the availability check.
	_, err = c.CreateBucket(ctx, input, func(o *s3.Options) { o.RetryMaxAttempts = 1 })
	if err != nil {
		return "", fmt.Errorf("Bucket creation was not confirmed. Refresh the bucket list before retrying: %w", err)
	}
	return name, nil
}
