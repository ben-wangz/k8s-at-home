# clash

Clash is a rule-based proxy service. This chart runs the Mihomo core and
builds its runtime configuration from a single subscription URL.

## Subscription Secret

The chart requires an existing Secret with exactly one key named
`subscription-url`:

```yaml
apiVersion: v1
kind: Secret
metadata:
  name: clash-subscription
type: Opaque
stringData:
  subscription-url: "<SUBSCRIPTION_URL>"
```

The chart does not accept a complete `config.yaml` Secret. It generates the
Mihomo configuration from the URL and the chart defaults at pod startup.

## Helm Values

The required Secret is selected with `subscription.existingSecret`.

Provider settings are configurable without changing the Secret:

```yaml
subscription:
  existingSecret: clash-subscription

provider:
  cachePath: providers/subscription.yaml
  interval: 3600
```

`provider.cachePath` is relative to `config.dir`. The default configuration
uses the HTTP proxy port and controller port defined by `ports.http` and
`ports.controller`, and connects the generated Provider to the default
`PROXY` group.

## Install

```shell
export CHART_VERSION=$(forgekit --project-root ../.. --output text version get clash)

helm upgrade --install clash oci://ghcr.io/ben-wangz/k8s-at-home-charts/clash \
  --atomic \
  --version "${CHART_VERSION}" \
  --namespace basic-components \
  --create-namespace \
  --set subscription.existingSecret=clash-subscription
```
