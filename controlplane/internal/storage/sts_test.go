package storage

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

func zapNoopLogger() *zap.Logger {
	l, _ := zap.NewDevelopment()
	return l
}

// decodePolicy unmarshals a session policy and returns its two statements,
// bucket-level first.
func decodePolicy(t *testing.T, policy string) (bucketStmt, objectStmt map[string]interface{}) {
	t.Helper()
	var doc map[string]interface{}
	require.NoError(t, json.Unmarshal([]byte(policy), &doc))
	assert.Equal(t, "2012-10-17", doc["Version"])
	stmts, ok := doc["Statement"].([]interface{})
	require.True(t, ok)
	require.Len(t, stmts, 2, "policy must split bucket-level and object-level actions")
	return stmts[0].(map[string]interface{}), stmts[1].(map[string]interface{})
}

func strSlice(t *testing.T, v interface{}) []string {
	t.Helper()
	raw, ok := v.([]interface{})
	require.True(t, ok)
	out := make([]string, 0, len(raw))
	for _, item := range raw {
		out = append(out, item.(string))
	}
	return out
}

func TestBuildSessionPolicy_SingleBucket(t *testing.T) {
	policy, err := BuildSessionPolicy([]BucketAccess{{BucketName: "my-bucket"}})
	require.NoError(t, err)
	require.NotEmpty(t, policy)

	bucketStmt, objectStmt := decodePolicy(t, policy)

	assert.Equal(t, "Allow", bucketStmt["Effect"])
	assert.Equal(t, "Allow", objectStmt["Effect"])

	// Bucket-level actions must carry a bucket ARN with no object suffix;
	// pairing them with "{bucket}/*" makes the grant silently inert (IC-BUG-4).
	assert.Equal(t, []string{"arn:aws:s3:::my-bucket"}, strSlice(t, bucketStmt["Resource"]))
	assert.Equal(t, []string{"s3:ListBucketMultipartUploads"}, strSlice(t, bucketStmt["Action"]))

	assert.Equal(t, []string{"arn:aws:s3:::my-bucket/*"}, strSlice(t, objectStmt["Resource"]))
	assert.ElementsMatch(t, []string{
		"s3:PutObject", "s3:AbortMultipartUpload", "s3:ListMultipartUploadParts",
	}, strSlice(t, objectStmt["Action"]))
}

// The agent only ever writes. Granting reads or listings on a bucket-wide
// resource would let any approved agent enumerate and download the whole data
// lake, which is wider than the write access the policy is meant to convey.
func TestBuildSessionPolicy_WriteOnly(t *testing.T) {
	policy, err := BuildSessionPolicy([]BucketAccess{{BucketName: "my-bucket"}})
	require.NoError(t, err)

	bucketStmt, objectStmt := decodePolicy(t, policy)
	granted := append(strSlice(t, bucketStmt["Action"]), strSlice(t, objectStmt["Action"])...)

	// Exact match, not substring: "s3:ListBucket" is a prefix of the legitimate
	// "s3:ListBucketMultipartUploads".
	for _, forbidden := range []string{"s3:GetObject", "s3:DeleteObject", "s3:ListBucket"} {
		assert.NotContains(t, granted, forbidden)
	}
}

// An empty bucket set used to fall back to "arn:aws:s3:::*", granting every
// bucket in the deployment — strictly wider than the per-bucket decision above.
func TestBuildSessionPolicy_NoBuckets(t *testing.T) {
	_, err := BuildSessionPolicy(nil)
	require.Error(t, err)

	_, err = BuildSessionPolicy([]BucketAccess{{BucketName: ""}})
	require.Error(t, err)
}

func TestBuildSessionPolicy_MultipleBuckets(t *testing.T) {
	policy, err := BuildSessionPolicy([]BucketAccess{
		{BucketName: "bucket-b"},
		{BucketName: "bucket-a"},
		{BucketName: "bucket-a"}, // duplicate rules may target the same bucket
	})
	require.NoError(t, err)

	bucketStmt, objectStmt := decodePolicy(t, policy)
	assert.Equal(t, []string{"arn:aws:s3:::bucket-a", "arn:aws:s3:::bucket-b"},
		strSlice(t, bucketStmt["Resource"]))
	assert.Equal(t, []string{"arn:aws:s3:::bucket-a/*", "arn:aws:s3:::bucket-b/*"},
		strSlice(t, objectStmt["Resource"]))
}

func TestSTSManager_IssueCredentials_NoMinIO(t *testing.T) {
	t.Skip("requires live MinIO with STS configuration")
}

func TestNewSTSManager_ReturnsManager(t *testing.T) {
	mgr := NewSTSManager("localhost:9000", "access", "secret", "arn:minio:sts:::role", false, zapNoopLogger())
	require.NotNil(t, mgr)
}

func TestNewSTSManager_PublicEndpointDefaultsToInternal(t *testing.T) {
	// Without WithPublicEndpoint the payload endpoint must mirror the internal
	// one so single-endpoint deployments keep the pre-split behavior (D-024).
	mgr := NewSTSManager("minio:9000", "access", "secret", "arn:minio:sts:::role", true, zapNoopLogger())
	assert.Equal(t, "minio:9000", mgr.publicEndpoint)
	assert.True(t, mgr.publicUseSSL)
}

func TestSTSManager_WithPublicEndpoint_OverridesPublicOnly(t *testing.T) {
	// The public override changes only the client-facing fields; the internal
	// endpoint used for AssumeRole stays as constructed.
	mgr := NewSTSManager("minio:9000", "access", "secret", "arn:minio:sts:::role", false, zapNoopLogger()).
		WithPublicEndpoint("cdn.example.com:443", true)

	assert.Equal(t, "minio:9000", mgr.endpoint, "internal endpoint must be untouched")
	assert.False(t, mgr.useSSL, "internal TLS flag must be untouched")
	assert.Equal(t, "cdn.example.com:443", mgr.publicEndpoint)
	assert.True(t, mgr.publicUseSSL)
}

func TestSTSManager_IssueCredentials_ReturnsError(t *testing.T) {
	// No real MinIO — should fail at credential exchange
	mgr := NewSTSManager("127.0.0.1:19999", "access", "secret", "arn:minio:sts:::role", false, zapNoopLogger())
	_, err := mgr.IssueCredentials(context.Background(), "agent-1", []BucketAccess{
		{BucketName: "test"},
	})
	require.Error(t, err)
}

func TestSTSManager_IssueCredentials_UseSSL_ReturnsError(t *testing.T) {
	// Exercises the useSSL=true code path (scheme="https").
	// No real MinIO — should still fail at credential exchange.
	mgr := NewSTSManager("127.0.0.1:19999", "access", "secret", "arn:minio:sts:::role", true, zapNoopLogger())
	_, err := mgr.IssueCredentials(context.Background(), "agent-ssl", []BucketAccess{
		{BucketName: "test"},
	})
	require.Error(t, err)
}
