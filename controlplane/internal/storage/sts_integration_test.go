//go:build integration

package storage

import (
	"bytes"
	"context"
	"io"
	"os"
	"testing"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

// minioCfgIT reads MinIO connection settings from environment variables,
// falling back to the docker-compose.test.yml defaults.
func minioCfgIT() (endpoint, accessKey, secretKey string) {
	endpoint = os.Getenv("TEST_MINIO_ENDPOINT")
	if endpoint == "" {
		endpoint = "localhost:9000"
	}
	accessKey = os.Getenv("TEST_MINIO_ACCESS_KEY")
	if accessKey == "" {
		accessKey = "minioadmin"
	}
	secretKey = os.Getenv("TEST_MINIO_SECRET_KEY")
	if secretKey == "" {
		secretKey = "minioadmin"
	}
	return
}

// TestIssueCredentials_Integration verifies that IssueCredentials contacts a real
// MinIO STS endpoint and returns temporary credentials that can be used to read
// and write objects in the target bucket.
//
// Prerequisites: docker compose -f deploy/docker-compose.test.yml up -d
func TestIssueCredentials_Integration(t *testing.T) {
	const (
		bucket    = "sts-integration-test"
		objectKey = "probe/sts-probe.txt"
		// MinIO accepts any valid ARN-format string for AssumeRole when the
		// caller uses root/admin credentials.
		roleARN = "arn:minio:iam:::role/agent-role"
	)

	endpoint, accessKey, secretKey := minioCfgIT()
	ctx := context.Background()

	// Build a root MinIO client to create/clean up the test bucket.
	rootClient, err := minio.New(endpoint, &minio.Options{
		Creds:  credentials.NewStaticV4(accessKey, secretKey, ""),
		Secure: false,
	})
	require.NoError(t, err, "failed to create root MinIO client")

	// Ensure test bucket exists.
	exists, err := rootClient.BucketExists(ctx, bucket)
	require.NoError(t, err)
	if !exists {
		require.NoError(t, rootClient.MakeBucket(ctx, bucket, minio.MakeBucketOptions{}))
	}
	t.Cleanup(func() {
		_ = rootClient.RemoveObject(ctx, bucket, objectKey, minio.RemoveObjectOptions{})
	})

	// Call IssueCredentials against the live MinIO server.
	logger := zap.NewNop()
	mgr := NewSTSManager(endpoint, accessKey, secretKey, roleARN, false, logger)

	creds, err := mgr.IssueCredentials(ctx, "integration-agent-001", []BucketAccess{
		{BucketName: bucket, PathPrefix: "probe"},
	})
	require.NoError(t, err, "IssueCredentials must succeed against a live MinIO server")
	assert.NotEmpty(t, creds.AccessKey, "AccessKey must be non-empty")
	assert.NotEmpty(t, creds.SecretKey, "SecretKey must be non-empty")
	assert.NotEmpty(t, creds.SessionToken, "SessionToken must be non-empty")
	assert.False(t, creds.ExpiresAt.AsTime().IsZero(), "ExpiresAt must be set")
	assert.Equal(t, endpoint, creds.Endpoint)
	assert.False(t, creds.UseSsl)

	// Verify the STS credentials can write an object to the scoped bucket.
	stsClient, err := minio.New(endpoint, &minio.Options{
		Creds:  credentials.NewStaticV4(creds.AccessKey, creds.SecretKey, creds.SessionToken),
		Secure: false,
	})
	require.NoError(t, err)

	data := []byte("STS integration-test probe payload")
	_, err = stsClient.PutObject(ctx, bucket, objectKey,
		bytes.NewReader(data), int64(len(data)),
		minio.PutObjectOptions{ContentType: "text/plain"})
	require.NoError(t, err, "STS credentials should allow writing to the bucket")

	// Verify the object is readable via the root client.
	obj, err := rootClient.GetObject(ctx, bucket, objectKey, minio.GetObjectOptions{})
	require.NoError(t, err)
	defer obj.Close()

	got, err := io.ReadAll(obj)
	require.NoError(t, err)
	assert.Equal(t, data, got, "read-back data must match the written payload")
}
