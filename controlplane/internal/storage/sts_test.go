package storage

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

func zapNoopLogger() *zap.Logger {
	l, _ := zap.NewDevelopment()
	return l
}

func TestBuildSessionPolicy_SingleBucket(t *testing.T) {
	policy, err := BuildSessionPolicy([]BucketAccess{
		{BucketName: "my-bucket", PathPrefix: "uploads/agent-1"},
	})
	require.NoError(t, err)
	require.NotEmpty(t, policy)

	var doc map[string]interface{}
	err := json.Unmarshal([]byte(policy), &doc)
	require.NoError(t, err)

	assert.Equal(t, "2012-10-17", doc["Version"])
	stmts, ok := doc["Statement"].([]interface{})
	require.True(t, ok)
	require.Len(t, stmts, 1)

	stmt := stmts[0].(map[string]interface{})
	assert.Equal(t, "Allow", stmt["Effect"])

	resources := stmt["Resource"].([]interface{})
	require.Len(t, resources, 1)
	assert.Contains(t, resources[0].(string), "my-bucket")
	assert.Contains(t, resources[0].(string), "uploads/agent-1")
}

func TestBuildSessionPolicy_NoBuckets(t *testing.T) {
	policy, err := BuildSessionPolicy(nil)
	require.NoError(t, err)
	require.NotEmpty(t, policy)
	assert.Contains(t, policy, "arn:aws:s3:::*")
}

func TestBuildSessionPolicy_MultipleBuckets(t *testing.T) {
	policy, err := BuildSessionPolicy([]BucketAccess{
		{BucketName: "bucket-a", PathPrefix: ""},
		{BucketName: "bucket-b", PathPrefix: "path/"},
	})
	require.NoError(t, err)

	var doc map[string]interface{}
	require.NoError(t, json.Unmarshal([]byte(policy), &doc))

	stmts := doc["Statement"].([]interface{})
	stmt := stmts[0].(map[string]interface{})
	resources := stmt["Resource"].([]interface{})
	assert.Len(t, resources, 2)

	var resourceStrings []string
	for _, r := range resources {
		resourceStrings = append(resourceStrings, r.(string))
	}
	assert.True(t, strings.Contains(strings.Join(resourceStrings, ","), "bucket-a"))
	assert.True(t, strings.Contains(strings.Join(resourceStrings, ","), "bucket-b"))
}

func TestSTSManager_IssueCredentials_NoMinIO(t *testing.T) {
	t.Skip("requires live MinIO with STS configuration")
}

func TestNewSTSManager_ReturnsManager(t *testing.T) {
	mgr := NewSTSManager("localhost:9000", "access", "secret", "arn:minio:sts:::role", false, zapNoopLogger())
	require.NotNil(t, mgr)
}

func TestSTSManager_IssueCredentials_ReturnsError(t *testing.T) {
	// No real MinIO — should fail at credential exchange
	mgr := NewSTSManager("127.0.0.1:19999", "access", "secret", "arn:minio:sts:::role", false, zapNoopLogger())
	_, err := mgr.IssueCredentials(context.Background(), "agent-1", []BucketAccess{
		{BucketName: "test", PathPrefix: "uploads/"},
	})
	require.Error(t, err)
}
