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
  type: local
  local:
    mountPath: /backup
    existingClaim: ""
    persistence:
      enabled: true
      size: 1Gi
