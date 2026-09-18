schedule: "0 0 1 1 *"
suspend: true
backoffLimit: 0
activeDeadlineSeconds: 1800
successfulJobsHistoryLimit: 1
failedJobsHistoryLimit: 1
image:
${IMAGE_REGISTRY_LINE}
  repository: ${IMAGE_REPOSITORY}
  tag: "0.0.0"
  digest: ${IMAGE_DIGEST}
  pullPolicy: Always
repositories:
  - name: ${REPOSITORY_NAME}
    url: ${REPOSITORY_URL}
${SSH_CONFIG}
retention:
  enabled: false
backup:
  compressionLevel: 6
  maxRunDuration: 10m
  gitTimeout: 5m
  minFreeBytes: 1
storage:
  type: s3
  s3:
    endpoint: ${S3_ENDPOINT}
    region: "us-east-1"
    bucket: ${S3_BUCKET}
    prefix: ${S3_PREFIX}
    forcePathStyle: true
    credentialsMode: secret
    existingSecret: ${S3_SECRET}
    accessKeyIdKey: "AWS_ACCESS_KEY_ID"
    secretAccessKeyKey: "AWS_SECRET_ACCESS_KEY"
customCA:
  existingSecret: ${CA_SECRET}
  key: "ca.crt"
