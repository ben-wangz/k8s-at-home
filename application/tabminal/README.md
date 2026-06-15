# Tabminal

[Tabminal](https://github.com/leask/tabminal) is a browser-based terminal and ACP agent workspace. It exposes an HTTP server and WebSocket endpoint on port `9846`.

## Helm Chart

### Prerequisites

- Kubernetes 1.23+
- Helm 3.8+
- Bitnami common chart dependency (`helm dependency update` before installing)

### Installing

```console
helm dependency update chart/
helm install tabminal chart/ --namespace tabminal --create-namespace
```

### Required Configuration

Tabminal **will not start** until you accept the upstream security warning:

```yaml
# values.yaml
tabminal:
  acceptTerms: true
```

You must also set a stable password — the image-generated temporary password is not suitable for Kubernetes:

```yaml
tabminal:
  password: "choose-a-strong-password"
```

For production, prefer a pre-existing Secret:

```yaml
tabminal:
  existingSecret: tabminal-secret
  existingSecretPasswordKey: password
```

```yaml
# Pre-created Secret
apiVersion: v1
kind: Secret
metadata:
  name: tabminal-secret
type: Opaque
stringData:
  password: choose-a-strong-password
```

### Minimal Example

```yaml
tabminal:
  acceptTerms: true
  password: "change-me"

ingress:
  enabled: false
```

### Production Example

```yaml
tabminal:
  acceptTerms: true
  existingSecret: tabminal-secret
  existingSecretPasswordKey: password

persistence:
  enabled: true
  size: 5Gi

ingress:
  enabled: true
  hostname: tabminal.example.com
  annotations:
    nginx.ingress.kubernetes.io/proxy-read-timeout: "3600"
    nginx.ingress.kubernetes.io/proxy-send-timeout: "3600"
```

## Security Warnings

Tabminal provides browser-based shell access to the container. Treat it as a high-privilege component:

- **Accept Terms**: The application refuses to start without `tabminal.acceptTerms: true`. This is an intentional safety gate.
- **Password Required**: Deployments without an explicit password are unreliable in Kubernetes. Always configure `tabminal.password` or `tabminal.existingSecret`.
- **Ingress**: Keep ingress disabled by default. When enabled, protect the endpoint with an external access control layer — VPN, Tailscale, Cloudflare Access, or similar Zero Trust proxy.
- **Persistent Volume**: The PVC stores session snapshots, auth state, agent configuration, and command history. Treat it as sensitive application state.
- **AI Provider Keys**: If AI provider keys are configured, terminal context, file paths, or command history may be sent to the configured provider. Review upstream documentation for details.

## AI Provider Notes

- `openrouterKey` and `openaiKey` are mutually exclusive. Only configure one.
- All key and credential values are rendered into a chart-managed Secret. Production deployments should use `existingSecret` instead.

## Configuration

See [values.yaml](chart/values.yaml) for the full set of configuration options. Notable values:

| Parameter | Default | Description |
|-----------|---------|-------------|
| `tabminal.acceptTerms` | `false` | Accept upstream security warning |
| `tabminal.password` | `""` | Plain-text password (rendered into Secret) |
| `tabminal.existingSecret` | `""` | Pre-existing Secret name |
| `tabminal.host` | `0.0.0.0` | Listen address in the pod |
| `persistence.enabled` | `true` | Enable PVC for `/root/.tabminal` |
| `persistence.size` | `1Gi` | PVC size |
| `ingress.enabled` | `false` | Enable ingress |
