# frp

该应用由两个相互独立的部分组成：

- `chart/` 在 Kubernetes 中部署单副本 frps。
- `client/` 提供 `frpcctl`，在被代理机器上使用 rootful Podman Quadlet 运行 frpc。

frpc 不会部署到 Kubernetes 服务端。客户端的独立监控使用 systemd timer 做公网 SSH 主机密钥探测，不读取 frpc 或 Podman 状态，也不会自动修改或重启隧道。

## 部署 frps

先创建认证 Secret。token 不应写入 values、Git 或命令行参数：

```bash
umask 077
openssl rand -hex 32 > /secure/path/frp-token
kubectl --namespace develop create secret generic frps-auth \
  --from-file=token=/secure/path/frp-token
```

服务端和客户端必须使用同一个 token。在客户端安装完成前保留该 root-only 文件，完成后再安全删除。

`chart/values.yaml` 中的 `config.content` 只接受非敏感 frps TOML。`secretMounts` 只保存既有 Secret 的名称、key 和挂载路径；chart 不创建凭据 Secret。默认端口与已验证方案一致：

| 用途 | frps 容器端口 | NodePort |
| --- | ---: | ---: |
| frpc 控制连接 | 7000 | 32700 |
| cloud01 SSH | 6000 | 32699 |

安装 chart：

```bash
helm dependency build application/frp/chart
helm upgrade --install frp application/frp/chart \
  --namespace develop \
  --create-namespace
```

frps 客户端会话保存在单个进程中，因此 chart 强制 `replicas: 1` 和 `Recreate`。修改 ConfigMap 会触发 Pod 重建；轮换外部 Secret 后需要显式重启 Deployment。

## 构建 frpcctl

```bash
mkdir -p build
go -C application/frp/client build -o ../../../build/frpcctl ./cmd/frpcctl
```

将二进制、非敏感配置和本地创建的凭据文件传到客户端机器。目标机需要 rootful Podman、Quadlet、systemd 和 `ssh-keyscan`。

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

Quadlet 使用 `Pull=missing`。如果客户端不能直连 Docker Hub，应先从可拉取镜像的管理机流式导入，不必在客户端保存归档文件：

```bash
podman pull docker.io/fatedier/frpc:v0.70.1
podman save docker.io/fatedier/frpc:v0.70.1 | \
  ssh root@192.168.1.11 podman load
ssh root@192.168.1.11 \
  podman image exists docker.io/fatedier/frpc:v0.70.1
```

在客户端以 root 安装：

```bash
/root/frpcctl install \
  --frpc-config /root/frpc.toml \
  --token-file /root/frp-token \
  --monitor-config /root/monitor.json \
  --webhook-config /root/dingtalk.json

rm -f /root/frp-token /root/dingtalk.json
```

确认服务端和客户端 Secret 都已创建后，再删除操作机上的 `/secure/path/frp-token`。

安装过程会完成以下操作：

- 将 frpc 配置安装到 `/etc/frp/frpc.toml`。
- 将 token 通过 stdin 创建为 Podman Secret `frp-auth`，不把 token 保存到 Quadlet。
- 生成 `/etc/containers/systemd/frpc.container`，由 Quadlet generator 启动 `frpc.service`。
- 将 Webhook 凭据保存为 `/etc/frp-monitor/webhook.json`，目录权限为 `0700`，文件权限为 `0600`。
- 生成并启用 `frp-monitor.timer`，启动后每 15 分钟进行一次独立端到端探测。

Quadlet 的 `[Install]` 指向 `multi-user.target`，generator 会把服务挂到启动目标，`systemctl is-enabled frpc.service` 应显示 `generated`。因此节点重启后 frpc 会自动恢复，无需对生成单元执行 `systemctl enable`。frpc 运行在 host network 中，容器内的 `127.0.0.1:22` 指向客户端主机 SSH 服务。

## 运维

```bash
systemctl status frpc.service frp-monitor.timer
journalctl -u frpc.service -u frp-monitor.service --since today
/usr/local/sbin/frpcctl monitor
/usr/local/sbin/frpcctl webhook-test
```

重新执行 `install` 默认保留现有 Podman Secret。轮换 token 时明确增加 `--replace-token`，安装流程会替换 Secret 并重启 frpc：

```bash
/usr/local/sbin/frpcctl install \
  --frpc-config /etc/frp/frpc.toml \
  --token-file /secure/path/new-token \
  --monitor-config /etc/frp-monitor/config.json \
  --webhook-config /etc/frp-monitor/webhook.json \
  --replace-token
```

默认卸载会保留配置、状态和 Secret；只有显式使用 `--purge` 才会删除这些数据：

```bash
# Choose this command to preserve data.
/usr/local/sbin/frpcctl uninstall

# Choose this command to remove data and the Secret.
/usr/local/sbin/frpcctl uninstall --purge
```

## 监控边界

监控通过公网地址获取 ED25519 SSH 主机密钥，并与客户端本机 `/etc/ssh/ssh_host_ed25519_key.pub` 比较。这覆盖 DNS、公网转发、Kubernetes frps、frpc 隧道和本机 SSH 服务。

该监控和 frpc 位于同一台客户端机器。整机断电、系统卡死或完全断网时，本机无法发送 Webhook；监控整机可用性需要把探测器部署到另一个故障域。
