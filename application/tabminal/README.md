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
kubectl create namespace tabminal
kubectl create secret generic tabminal-secret \
  --namespace tabminal \
  --from-literal=password='choose-a-strong-password'
helm install tabminal chart/ \
  --namespace tabminal \
  --set tabminal.acceptTerms=true \
  --set tabminal.existingSecret=tabminal-secret
```

### Required Configuration

Tabminal **will not start** until you accept the upstream security warning:

```yaml
# values.yaml
tabminal:
  acceptTerms: true
```

You must also provide a pre-existing Secret with a stable password. The chart does not accept credentials through Helm values and does not generate a random Secret because random values cause perpetual drift in GitOps renderers such as Argo CD.

```yaml
apiVersion: v1
kind: Secret
metadata:
  name: tabminal-secret
type: Opaque
stringData:
  password: choose-a-strong-password
```

```yaml
tabminal:
  existingSecret: tabminal-secret
  existingSecretPasswordKey: password
```

### Minimal Example

```yaml
tabminal:
  acceptTerms: true
  existingSecret: tabminal-secret
  existingSecretPasswordKey: password

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
- **Password Required**: `tabminal.existingSecret` is required and must contain a stable password. The chart never stores credentials in Helm values.
- **Ingress**: Keep ingress disabled by default. When enabled, protect the endpoint with an external access control layer — VPN, Tailscale, Cloudflare Access, or similar Zero Trust proxy.
- **Persistent Volume**: The PVC stores session snapshots, auth state, agent configuration, and command history. Treat it as sensitive application state.
- **Generated SSH Keys**: A generated private key is protected only as strongly as its passphrase. The generated Secret is intentionally retained across syncs and uninstall operations.
- **AI Provider Keys**: If AI provider keys are configured, terminal context, file paths, or command history may be sent to the configured provider. Review upstream documentation for details.

## Optional Credentials

- Store optional provider credentials in `tabminal.existingSecret` and configure the corresponding `existingSecret*Key` value with the Secret key name.
- `existingSecretOpenrouterKey` and `existingSecretOpenaiKey` are mutually exclusive. Only configure one.
- `extraEnvVarsSecret` can expose another Secret whose keys already use the upstream environment variable names.

## Existing SSH Client Key

The chart can mount an SSH client key from a pre-existing Secret. It copies the selected files to an in-memory `/root/.ssh` directory with permissions accepted by OpenSSH.

```console
kubectl create secret generic tabminal-ssh \
  --namespace tabminal \
  --type=kubernetes.io/ssh-auth \
  --from-file=ssh-privatekey="$HOME/.ssh/id_ed25519" \
  --from-file=known_hosts="$HOME/.ssh/known_hosts"
```

```yaml
ssh:
  enabled: true
  existingSecret: tabminal-ssh
  privateKeyKey: ssh-privatekey
  privateKeyFilename: id_ed25519
  publicKeyKey: ""
  knownHostsKey: known_hosts
```

`knownHostsKey` and `configKey` are optional. When `knownHostsKey` is empty, interactive SSH sessions can add host keys to the Pod-local `known_hosts` file. Restart the Pod after rotating the source Secret.

## Generated SSH Client Key

The chart can generate a passphrase-protected Ed25519 key pair from a passphrase stored in a pre-existing Secret. The passphrase is exposed only to the generation Job and is not injected into the Tabminal container.

Create the passphrase Secret before installing or synchronizing the application:

```console
kubectl create secret generic tabminal-ssh-passphrase \
  --namespace tabminal \
  --from-literal=password='choose-a-passphrase'
```

```yaml
ssh:
  enabled: true
  keyGeneration:
    enabled: true
    outputSecretName: tabminal-ssh
    passphraseSecret:
      name: tabminal-ssh-passphrase
      key: password
    serviceAccountTokenExpirationSeconds: 600
```

The pre-install/pre-upgrade Job creates an `Opaque` Secret containing `id_ed25519` and `id_ed25519.pub`. On later runs it decrypts the existing private key with the configured passphrase and verifies that the public key matches. It fails the Helm operation or Argo CD sync instead of replacing an existing key when verification fails.

The generated Secret is not owned by the Hook Job or rendered as a Helm resource. It remains stable across renders, syncs, and uninstall operations. Delete it explicitly before a deliberate key rotation, then perform a full sync and restart the Tabminal Pod.

Argo CD runs the generator as a `PreSync` Hook. The passphrase Secret must therefore exist before the sync and should be managed outside the same Argo CD Application. Selective sync does not run hooks.

The dedicated Job ServiceAccount can `get` only the configured output Secret. Kubernetes RBAC cannot restrict `create` by resource name, so its `create` permission covers Secrets in the release namespace. The Job receives a projected ServiceAccount token for the configured lifetime, with a minimum and default of 600 seconds. It has no `list`, `watch`, `update`, `patch`, or `delete` permission.

OpenSSH does not automatically read a passphrase from an environment variable. Interactive SSH asks for the passphrase when the key is used.

## Configuration

See [values.yaml](chart/values.yaml) for the full set of configuration options. Notable values:

| Parameter | Default | Description |
|-----------|---------|-------------|
| `tabminal.acceptTerms` | `false` | Accept upstream security warning |
| `tabminal.existingSecret` | `""` | Required pre-existing Secret name |
| `tabminal.existingSecretPasswordKey` | `password` | Password key in the Secret |
| `tabminal.host` | `0.0.0.0` | Listen address in the pod |
| `image.tag` | `sha-65e7af9` | Official commit tag containing Tabminal 3.0.40 |
| `persistence.enabled` | `true` | Enable PVC for `/root/.tabminal` |
| `persistence.size` | `1Gi` | PVC size |
| `ssh.enabled` | `false` | Mount SSH client files from an existing or generated Secret |
| `ssh.existingSecret` | `""` | Pre-existing SSH Secret name |
| `ssh.keyGeneration.enabled` | `false` | Generate a stable passphrase-protected Ed25519 key pair |
| `ssh.keyGeneration.outputSecretName` | `""` | Generated Secret name; defaults to `<fullname>-ssh` |
| `ssh.keyGeneration.passphraseSecret.name` | `""` | Pre-existing passphrase Secret read only by the Job |
| `ssh.keyGeneration.serviceAccountTokenExpirationSeconds` | `600` | Projected Job token lifetime, minimum 600 seconds |
| `ssh.keyGeneration.activeDeadlineSeconds` | `600` | Maximum time allowed for image pull and key generation |
| `ingress.enabled` | `false` | Enable ingress |
