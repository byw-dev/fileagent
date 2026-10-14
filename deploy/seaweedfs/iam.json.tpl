{
  "sts": {
    "issuer": "fileagent-sts",
    "signingKey": "__SW_STS_SIGNING_KEY__",
    "tokenDuration": "1h",
    "maxSessionLength": "12h"
  },
  "policy": {
    "defaultEffect": "Deny",
    "storeType": "memory"
  },
  "roles": [
    {
      "roleName": "agent-role",
      "roleArn": "__SW_AGENT_ROLE_ARN__",
      "trustPolicy": {
        "Version": "2012-10-17",
        "Statement": [
          {
            "Effect": "Allow",
            "Principal": {
              "AWS": "arn:aws:iam::000000000000:user/fileagent-controlplane"
            },
            "Action": ["sts:AssumeRole"]
          }
        ]
      },
      "attachedPolicies": ["AgentRolePolicy"]
    }
  ],
  "policies": [
    {
      "name": "AgentRolePolicy",
      "document": {
        "version": "2012-10-17",
        "statement": [
          {
            "effect": "Allow",
            "action": ["s3:CreateBucket", "s3:ListBucketMultipartUploads"],
            "resource": ["arn:aws:s3:::*"]
          },
          {
            "effect": "Allow",
            "action": [
              "s3:GetObject",
              "s3:PutObject",
              "s3:AbortMultipartUpload",
              "s3:ListMultipartUploadParts"
            ],
            "resource": ["arn:aws:s3:::*/*"]
          }
        ]
      }
    }
  ],
  "providers": []
}
