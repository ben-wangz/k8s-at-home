# sub2api chart

Minimal Helm chart usage for `sub2api`.

## Install

```sh
# get latest chart version from this repo
export CHART_VERSION=$(forgekit --project-root ../../.. version get chart sub2api)

helm upgrade --install sub2api oci://ghcr.io/ben-wangz/k8s-at-home-charts/sub2api \
  --atomic \
  --version "${CHART_VERSION}" \
  --namespace ai \
  --create-namespace \
  --set ingress.enabled=true \
  --set ingress.hostname=sub2api.example.com \
  --set sub2api.auth.adminPassword="change-me" \
  --set sub2api.auth.jwtSecret="change-me" \
  --set sub2api.auth.totpEncryptionKey="change-me"
```

## Verify

```sh
kubectl -n ai get pods,svc,ingress
```
