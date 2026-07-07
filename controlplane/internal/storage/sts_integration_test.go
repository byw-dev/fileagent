//go:build integration

package storage

import (
	"bytes"
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	miniogo "github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

const (
	itMinIOEndpoint = "localhost:9000"
	itMinIOUser     = "minioadmin"
	itMinIOPassword = "minioadmin"
	itRoleARN       = "arn:aws:iam:::role/test"
)

// newRootClient creates a MinIO client with the test-environment root credentials.
func newRootClient(t *testing.T) *miniogo.Client {
	t.Helper()
	c, err := miniogo.New(itMinIOEndpoint, &miniogo.Options{
		Creds:  credentials.NewStaticV4(itMinIOUser, itMinIOPassword, ""),
		Secure: false,
	})
	require.NoError(t, err, "create root MinIO client")
	return c
}

// uniqueBucket returns a unique bucket name and creates it, registering a
// cleanup to delete the bucket and all its objects at test end.
func uniqueBucket(t *testing.T, root *miniogo.Client, prefix string) string {
	t.Helper()
	bucket := fmt.Sprintf("%s-%d", prefix, time.Now().UnixNano())
	ctx := context.Background()
	err := root.MakeBucket(ctx, bucket, miniogo.MakeBucketOptions{})
	require.NoError(t, err, "create test bucket %q", bucket)
	t.Cleanup(func() {
		for obj := range root.ListObjects(context.Background(), bucket, miniogo.ListObjectsOptions{Recursive: true}) {
			_ = root.RemoveObject(context.Background(), bucket, obj.Key, miniogo.RemoveObjectOptions{})
		}
		_ = root.RemoveBucket(context.Background(), bucket)
	})
	return bucket
}

// TestIssueCredentials_Integration verifies that STSManager.IssueCredentials
// returns usable temporary credentials that can actually write to MinIO.
func TestIssueCredentials_Integration(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	root := newRootClient(t)
	bucket := uniqueBucket(t, root, "sts-it")

	logger, _ := zap.NewDevelopment()
	mgr := NewSTSManager(itMinIOEndpoint, itMinIOUser, itMinIOPassword, itRoleARN, false, logger)

	cred, err := mgr.IssueCredentials(ctx, "agent-integration-1", []BucketAccess{
		{BucketName: bucket, PathPrefix: "uploads/"},
	})
	if err != nil {
		if strings.Contains(err.Error(), "not supported") ||
			strings.Contains(err.Error(), "not configured") ||
			strings.Contains(err.Error(), "AccessDenied") {
			t.Skipf("STS AssumeRole not available on test MinIO: %v", err)
		}
		t.Fatalf("IssueCredentials failed: %v", err)
	}

	// --- credential fields ---
	require.NotEmpty(t, cred.GetAccessKey(), "AccessKey must be non-empty")
	require.NotEmpty(t, cred.GetSecretKey(), "SecretKey must be non-empty")
	require.NotEmpty(t, cred.GetSessionToken(), "SessionToken must be non-empty")
	assert.Equal(t, itMinIOEndpoint, cred.GetEndpoint())
	assert.False(t, cred.GetUseSsl())
	assert.True(t, cred.GetExpiresAt().AsTime().After(time.Now()),
		"ExpiresAt should be in the future")

	// --- public endpoint override surfaces in the returned payload (D-024) ---
	// AssumeRole still dials the internal endpoint; only the payload endpoint
	// handed to the agent reflects the configured public value.
	pubMgr := NewSTSManager(itMinIOEndpoint, itMinIOUser, itMinIOPassword, itRoleARN, false, logger).
		WithPublicEndpoint("minio.public.example:443", true)
	pubCred, err := pubMgr.IssueCredentials(ctx, "agent-integration-public", []BucketAccess{
		{BucketName: bucket, PathPrefix: "uploads/"},
	})
	require.NoError(t, err, "IssueCredentials with public endpoint override")
	assert.Equal(t, "minio.public.example:443", pubCred.GetEndpoint())
	assert.True(t, pubCred.GetUseSsl())

	// --- STS credentials can write to the scoped bucket ---
	stsClient, err := miniogo.New(itMinIOEndpoint, &miniogo.Options{
		Creds: credentials.NewStaticV4(
			cred.GetAccessKey(),
			cred.GetSecretKey(),
			cred.GetSessionToken(),
		),
		Secure: false,
	})
	require.NoError(t, err, "create STS MinIO client")

	objectName := "uploads/integration-test.txt"
	content := []byte("hello from STS integration test")
	_, err = stsClient.PutObject(ctx, bucket, objectName, bytes.NewReader(content),
		int64(len(content)), miniogo.PutObjectOptions{ContentType: "text/plain"})
	require.NoError(t, err, "STS client must be able to PutObject in scoped bucket")

	// --- root client can verify the object ---
	info, err := root.StatObject(ctx, bucket, objectName, miniogo.StatObjectOptions{})
	require.NoError(t, err, "root client must see the uploaded object")
	assert.Equal(t, int64(len(content)), info.Size)
}

// TestIssueCredentials_ScopingDeniesOtherBuckets verifies that STS credentials
// scoped to one bucket cannot write to a different bucket.
func TestIssueCredentials_ScopingDeniesOtherBuckets(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	root := newRootClient(t)
	allowedBucket := uniqueBucket(t, root, "sts-allowed")
	otherBucket := uniqueBucket(t, root, "sts-other")

	logger, _ := zap.NewDevelopment()
	mgr := NewSTSManager(itMinIOEndpoint, itMinIOUser, itMinIOPassword, itRoleARN, false, logger)

	cred, err := mgr.IssueCredentials(ctx, "agent-integration-2", []BucketAccess{
		{BucketName: allowedBucket, PathPrefix: ""},
	})
	if err != nil {
		if strings.Contains(err.Error(), "not supported") ||
			strings.Contains(err.Error(), "not configured") ||
			strings.Contains(err.Error(), "AccessDenied") {
			t.Skipf("STS AssumeRole not available on test MinIO: %v", err)
		}
		t.Fatalf("IssueCredentials failed: %v", err)
	}

	stsClient, err := miniogo.New(itMinIOEndpoint, &miniogo.Options{
		Creds: credentials.NewStaticV4(
			cred.GetAccessKey(),
			cred.GetSecretKey(),
			cred.GetSessionToken(),
		),
		Secure: false,
	})
	require.NoError(t, err)

	content := []byte("should be denied")
	_, err = stsClient.PutObject(ctx, otherBucket, "test.txt", bytes.NewReader(content),
		int64(len(content)), miniogo.PutObjectOptions{})
	assert.Error(t, err, "STS credentials scoped to allowedBucket must not write to otherBucket")
}
