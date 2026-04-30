// Package storage provides STS credential management for agent uploads.
package storage

import (
	"encoding/json"
	"fmt"
	"strings"
)

// BucketAccess defines a bucket and path prefix for an STS session policy.
type BucketAccess struct {
	BucketName string
	PathPrefix string
}

// policyDocument represents a minimal AWS/MinIO IAM policy.
type policyDocument struct {
	Version   string          `json:"Version"`
	Statement []policyStatement `json:"Statement"`
}

type policyStatement struct {
	Effect   string   `json:"Effect"`
	Action   []string `json:"Action"`
	Resource []string `json:"Resource"`
}

// BuildSessionPolicy creates an IAM-style policy JSON that grants PutObject,
// GetObject and DeleteObject access to the specified buckets and path prefixes.
// Returns an empty string if JSON marshaling fails (should never happen given the fixed schema).
func BuildSessionPolicy(buckets []BucketAccess) (string, error) {
	var resources []string
	for _, b := range buckets {
		prefix := strings.TrimSuffix(b.PathPrefix, "/")
		if prefix == "" {
			resources = append(resources, fmt.Sprintf("arn:aws:s3:::%s/*", b.BucketName))
		} else {
			resources = append(resources, fmt.Sprintf("arn:aws:s3:::%s/%s/*", b.BucketName, prefix))
		}
	}

	if len(resources) == 0 {
		resources = []string{"arn:aws:s3:::*"}
	}

	doc := policyDocument{
		Version: "2012-10-17",
		Statement: []policyStatement{
			{
				Effect: "Allow",
				Action: []string{
					"s3:PutObject",
					"s3:GetObject",
					"s3:DeleteObject",
					"s3:ListBucket",
				},
				Resource: resources,
			},
		},
	}

	data, err := json.Marshal(doc)
	if err != nil {
		return "", fmt.Errorf("storage: marshal session policy: %w", err)
	}
	return string(data), nil
}
