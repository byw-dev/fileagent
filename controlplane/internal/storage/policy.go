// Package storage provides STS credential management for agent uploads.
package storage

import (
	"encoding/json"
	"fmt"
	"sort"
)

// BucketAccess names one bucket an STS session is allowed to write to.
//
// There is deliberately no path prefix: D-030 §8 grants agents write access to
// the whole bucket. Narrowing the policy by prefix was rejected because the
// object key is produced entirely by the rule's dest_path_template, which is
// unconstrained — any prefix the Control Plane guessed at issue time would
// either be empty (and so degenerate to the whole bucket anyway) or drift away
// from the real key and reject valid uploads. Reconciliation cost is bounded by
// sharded auditing instead, not by the grant's prefix.
type BucketAccess struct {
	BucketName string
}

// policyDocument represents a minimal AWS/MinIO IAM policy.
type policyDocument struct {
	Version   string            `json:"Version"`
	Statement []policyStatement `json:"Statement"`
}

type policyStatement struct {
	Effect   string   `json:"Effect"`
	Action   []string `json:"Action"`
	Resource []string `json:"Resource"`
}

// bucketActions are bucket-level S3 actions. Their resource ARN must be the
// bucket itself, without a "/*" object suffix — an object-level ARN silently
// makes the grant a no-op rather than raising an error (that was IC-BUG-4).
var bucketActions = []string{
	"s3:ListBucketMultipartUploads",
}

// objectActions are object-level S3 actions, scoped to "{bucket}/*".
//
// Write-only by design: the agent only ever calls PutObject and the multipart
// APIs, so s3:GetObject / s3:ListBucket would be pure over-grant — with a
// bucket-wide resource they would let any approved agent enumerate and download
// the entire data lake. s3:DeleteObject is omitted for the same reason: a
// compromised agent must not be able to erase archived data (D-030 §8).
var objectActions = []string{
	"s3:PutObject",
	"s3:AbortMultipartUpload",
	"s3:ListMultipartUploadParts",
}

// BuildSessionPolicy creates an IAM-style session policy granting write-only
// access to the whole of each named bucket.
//
// It returns an error when buckets is empty: an empty policy previously fell
// back to "arn:aws:s3:::*", which grants every bucket in the deployment. That
// fallback is strictly wider than the decision it sat under, so callers must
// pass at least one bucket instead.
func BuildSessionPolicy(buckets []BucketAccess) (string, error) {
	names := make([]string, 0, len(buckets))
	seen := make(map[string]struct{}, len(buckets))
	for _, b := range buckets {
		if b.BucketName == "" {
			continue
		}
		if _, dup := seen[b.BucketName]; dup {
			continue
		}
		seen[b.BucketName] = struct{}{}
		names = append(names, b.BucketName)
	}
	if len(names) == 0 {
		return "", fmt.Errorf("storage: session policy requires at least one bucket")
	}
	// Stable ordering keeps the generated policy (and its tests) deterministic.
	sort.Strings(names)

	bucketARNs := make([]string, 0, len(names))
	objectARNs := make([]string, 0, len(names))
	for _, name := range names {
		bucketARNs = append(bucketARNs, fmt.Sprintf("arn:aws:s3:::%s", name))
		objectARNs = append(objectARNs, fmt.Sprintf("arn:aws:s3:::%s/*", name))
	}

	doc := policyDocument{
		Version: "2012-10-17",
		Statement: []policyStatement{
			{Effect: "Allow", Action: bucketActions, Resource: bucketARNs},
			{Effect: "Allow", Action: objectActions, Resource: objectARNs},
		},
	}

	data, err := json.Marshal(doc)
	if err != nil {
		return "", fmt.Errorf("storage: marshal session policy: %w", err)
	}
	return string(data), nil
}
