{
  "identities": [
    {
      "name": "admin",
      "credentials": [
        {
          "accessKey": "__SW_ADMIN_ACCESS_KEY__",
          "secretKey": "__SW_ADMIN_SECRET_KEY__"
        }
      ],
      "actions": ["Admin"]
    },
    {
      "name": "fileagent-controlplane",
      "credentials": [
        {
          "accessKey": "__SW_CP_ACCESS_KEY__",
          "secretKey": "__SW_CP_SECRET_KEY__"
        }
      ],
      "policyNames": ["fileagent-cp"]
    }
  ],
  "policies": [
    {
      "name": "fileagent-cp",
      "content": "{\"Version\": \"2012-10-17\", \"Statement\": [{\"Effect\": \"Allow\", \"Action\": [\"s3:CreateBucket\", \"s3:ListBucketMultipartUploads\"], \"Resource\": [\"arn:aws:s3:::*\"]}, {\"Effect\": \"Allow\", \"Action\": [\"s3:GetObject\", \"s3:PutObject\", \"s3:AbortMultipartUpload\", \"s3:ListMultipartUploadParts\"], \"Resource\": [\"arn:aws:s3:::*/*\"]}]}"
    }
  ]
}
