package storage

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// IC-BUG-39: deploy/scripts/init-minio.sh hardcodes a copy of the STS session
// policy (STS_SESSION_POLICY) that must stay in lockstep with
// BuildSessionPolicy — the script's AssumeRole self-check only passes for a
// policy the Control Plane will actually mint. The two texts are written in
// different places with no other mechanism linking them, so a future Action
// added to policy.go but not to the script would be silently stripped at the
// session-policy intersection: the script's self-assertion stays green while
// real agents lose the permission. This guard fails the build the moment the
// two drift.

// scriptSessionPolicy extracts the STS_SESSION_POLICY assignment from
// deploy/scripts/init-minio.sh and unmarshals it.
func scriptSessionPolicy(t *testing.T) policyDocument {
	t.Helper()
	script, err := os.ReadFile(filepath.Join("..", "..", "..", "deploy", "scripts", "init-minio.sh"))
	require.NoError(t, err, "read init-minio.sh")

	const marker = "STS_SESSION_POLICY='"
	start := strings.Index(string(script), marker)
	require.NotEqual(t, -1, start, "STS_SESSION_POLICY assignment not found in init-minio.sh")
	rest := string(script)[start+len(marker):]
	end := strings.Index(rest, "'")
	require.NotEqual(t, -1, end, "unterminated STS_SESSION_POLICY assignment")

	var doc policyDocument
	require.NoError(t, json.Unmarshal([]byte(rest[:end]), &doc),
		"STS_SESSION_POLICY in init-minio.sh must be valid JSON")
	return doc
}

// TestSessionPolicyMatchesInitScript compares the script's hardcoded session
// policy with what BuildSessionPolicy actually mints for the bucket the script
// names. Bucket-for-bucket: today the script pins exactly one bucket
// (data-sensor), so the minted policy is built for that bucket.
func TestSessionPolicyMatchesInitScript(t *testing.T) {
	scriptDoc := scriptSessionPolicy(t)

	minted, err := BuildSessionPolicy([]BucketAccess{{BucketName: "data-sensor"}})
	require.NoError(t, err)
	var mintedDoc policyDocument
	require.NoError(t, json.Unmarshal([]byte(minted), &mintedDoc))

	assert.Equal(t, scriptDoc, mintedDoc,
		"init-minio.sh's STS_SESSION_POLICY drifted from BuildSessionPolicy — update both together "+
			"(deploy/scripts/init-minio.sh STS_SESSION_POLICY and controlplane/internal/storage/policy.go), "+
			"otherwise the script's self-check validates a policy the Control Plane no longer mints")
}

// TestSessionPolicyActionSetsMatch pins the Action sets individually so a
// drift report names the missing action instead of dumping whole documents.
func TestSessionPolicyActionSetsMatch(t *testing.T) {
	scriptDoc := scriptSessionPolicy(t)
	require.Len(t, scriptDoc.Statement, 2, "script policy must have exactly two statements")

	minted, err := BuildSessionPolicy([]BucketAccess{{BucketName: "data-sensor"}})
	require.NoError(t, err)
	var mintedDoc policyDocument
	require.NoError(t, json.Unmarshal([]byte(minted), &mintedDoc))
	require.Len(t, mintedDoc.Statement, 2)

	for i := range scriptDoc.Statement {
		scriptActions := strings.Join(scriptDoc.Statement[i].Action, ",")
		mintedActions := strings.Join(mintedDoc.Statement[i].Action, ",")
		assert.Equal(t, scriptActions, mintedActions,
			fmt.Sprintf("statement %d Action set drifted between init-minio.sh and policy.go", i))
	}
}
