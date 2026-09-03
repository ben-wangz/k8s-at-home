# frp

The application consists of two independent parts:

- `chart/` deploys a single-replica frps in Kubernetes.
- `client/` provides `frpcctl`, which uses the rootful Podman Quadlet to run frpc on the proxied machine.

frpc is not deployed to the Kubernetes server. The client's independent monitor uses a systemd timer to check the public SSH host key. It does not read frpc or Podman status and never modifies or restarts the tunnel automatically.

## Deploy frps

Create an authentication Secret first. The token must not be written to values, Git, or command-line arguments:

```bash
umask 077
openssl rand -hex 32 > /secure/path/frp-token
kubectl --namespace develop create secret generic frps-auth \
  --from-file=token=/secure/path/frp-token
```

The server and client must use the same token. Keep this root-only file until client installation is complete, then delete it securely.

`config.content` in `chart/values.yaml` accepts only non-sensitive frps TOML. `secretMounts` stores only the name, key, and mount path of an existing Secret; the chart does not create a credential Secret. The default ports match the verified setup:

| Purpose | frps container port | NodePort |
| --- | ---: | ---: |
| frpc control connection | 7000 | 32700 |
| cloud01 SSH | 6000 | 32699 |

Install chart:

```bash
helm dependency build application/frp/chart
helm upgrade --install frp application/frp/chart \
  --namespace develop \
  --create-namespace
```

frps client sessions are held by a single process, so the chart enforces `replicas: 1` and `Recreate`. Modifying the ConfigMap triggers a Pod restart; after rotating an external Secret, explicitly restart the Deployment.

## Build frpcctl

```bash
mkdir -p build
go -C application/frp/client build -o ../../../build/frpcctl ./cmd/frpcctl
```

Copy the binary, non-sensitive configuration, and locally created credentials file to the client machine. The target machine requires rootful Podman, Quadlet, systemd, and `ssh-keyscan`.

```bash
install -m 0600 application/frp/client/examples/dingtalk.json.example \
  /secure/path/dingtalk.json
# Fill in the real DingTalk access_token and SEC signing secret.

scp build/frpcctl \
  application/frp/client/examples/frpc.toml \
  application/frp/client/examples/monitor.json \
  root@192.168.1.11:/root/
scp /secure/path/frp-token \
  /secure/path/dingtalk.json \
  root@192.168.1.11:/root/
```

Quadlet uses `Pull=missing`. If the client cannot directly connect to Docker Hub, you should first stream the import from a management machine that can pull the image without saving the archive file on the client:

```bash
podman pull docker.io/fatedier/frpc:v0.70.1
podman save docker.io/fatedier/frpc:v0.70.1 | \
  ssh root@192.168.1.11 podman load
ssh root@192.168.1.11 \
  podman image exists docker.io/fatedier/frpc:v0.70.1
```

Install as root on the client:

```bash
/root/frpcctl install \
  --frpc-config /root/frpc.toml \
  --token-file /root/frp-token \
  --monitor-config /root/monitor.json \
  --webhook-config /root/dingtalk.json

rm -f /root/frp-token /root/dingtalk.json
```

After confirming that both server and client Secrets exist, delete `/secure/path/frp-token` from the operator workstation.

The installation process will complete the following operations:

- Install frpc configuration into `/etc/frp/frpc.toml`.
- Create token as Podman Secret `frp-auth` via stdin and do not save token to Quadlet.
- Generate `/etc/containers/systemd/frpc.container` and start `frpc.service` by Quadlet generator.
- Save the webhook credentials as `/etc/frp-monitor/webhook.json` with directory permissions `0700` and file permissions `0600`.
- Generate and enable `frp-monitor.timer` to perform independent end-to-end probing every 15 minutes after startup.

Quadlet's `[Install]` points to `multi-user.target`, so the generator attaches the service to the boot target. `systemctl is-enabled frpc.service` should display `generated`. Therefore, frpc automatically recovers after a node restart, and there is no need to run `systemctl enable` on the generated unit. frpc runs in the host network, so `127.0.0.1:22` inside the container points to the client host's SSH service.

## Operation and maintenance

```bash
systemctl status frpc.service frp-monitor.timer
journalctl -u frpc.service -u frp-monitor.service --since today
/usr/local/sbin/frpcctl monitor
/usr/local/sbin/frpcctl webhook-test
```

Re-executing `install` will retain the existing Podman Secret by default. If you explicitly add `--replace-token` when rotating tokens, the installation process will replace the Secret and restart frpc:

```bash
/usr/local/sbin/frpcctl install \
  --frpc-config /etc/frp/frpc.toml \
  --token-file /secure/path/new-token \
  --monitor-config /etc/frp-monitor/config.json \
  --webhook-config /etc/frp-monitor/webhook.json \
  --replace-token
```

The default uninstall preserves configuration, state, and secrets; only explicit use of `--purge` will delete this data:

```bash
# Choose this command to preserve data.
/usr/local/sbin/frpcctl uninstall

# Choose this command to remove data and the Secret.
/usr/local/sbin/frpcctl uninstall --purge
```

## Monitoring boundaries

Monitor to obtain the ED25519 SSH host key through the public network address and compare it with the client's local `/etc/ssh/ssh_host_ed25519_key.pub`. This covers DNS, public forwarding, Kubernetes frps, frpc tunnels, and native SSH services.

The monitor and frpc are on the same client machine. When the entire machine is powered off, the system is stuck, or the network is completely disconnected, the machine cannot send Webhook; monitoring the availability of the entire machine requires deploying the detector to another fault domain.
