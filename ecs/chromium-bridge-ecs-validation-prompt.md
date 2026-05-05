# Chromium Bridge ECS Validation Prompt

Use this prompt in a fresh OpenCode session to reproduce the full ECS validation flow end-to-end.

Before running, replace `<ECS_HOST>` with your current ECS address.

```text
你现在要在远端 ECS `<ECS_HOST>` 上，从零完成 `chromium-bridge` 的 Phase 1 验证。请严格执行以下目标和约束：

【目标】
1) 在 ECS 上安装并跑通 k3s。
2) 在 k3s 内安装一个 NodePort container registry（`31666`）。
3) 在 ECS 本机构建两个镜像并 push 到该 registry：
   - chromium-bridge-server
   - chromium-bridge-novnc
4) 使用 Helm 安装 `application/chromium-bridge/chart`。
5) 验证 CDP 与 noVNC 可用：
   - CDP: `curl /json/version` 返回正常
   - noVNC: HTTP 200
6) 再跑一次 Playwright `connectOverCDP` 验证并截图。
7) 将通过 `m.daocloud.io` 拉取的基础镜像缓存到 `/data/image-cache`，并保留使用文档 `/data/image-cache-usage.md`。

【关键约束】
- 只能在远端机器 `<ECS_HOST>` 上执行验证，不在本机做替代验证。
- 所有原本走 Docker Hub/Quay/GHCR 的关键镜像，优先替换为 `m.daocloud.io/...` 拉取。
- 不要依赖外网稳定访问 bitnami chart 仓库；若拉不动，使用可达镜像源或本地 vendored chart 方案。
- 优先把耗时操作放后台并写日志，避免 SSH 中断导致误判。
- 不要跳过最终验证步骤（CDP + noVNC + Playwright）。

【仓库路径】
- 项目根目录：`/root/code/k8s-at-home`
- chart 路径：`application/chromium-bridge/chart`
- 推荐 values：`application/chromium-bridge/chart/values-ecs.yaml`

【执行步骤（必须按顺序）】

Step 0. 连接和前置检查
- 通过 ssh 连接 `root@<ECS_HOST>`。
- 检查：`podman`、`helm`、`k3s` 是否存在；记录当前状态。

Step 1. 安装或重装 k3s
- 若未安装，执行：
  `curl -sfL https://rancher-mirror.rancher.cn/k3s/k3s-install.sh | INSTALL_K3S_MIRROR=cn sh -`
- 验证：
  - `kubectl get nodes -o wide`
  - `kubectl get pods -n kube-system -o wide`

Step 2. 预拉并重命名 k3s 关键镜像（使用 m.daocloud.io）
- 需要处理以下镜像（mirror -> original）：
  - `m.daocloud.io/docker.io/rancher/mirrored-pause:3.6` -> `docker.io/rancher/mirrored-pause:3.6`
  - `m.daocloud.io/docker.io/rancher/mirrored-coredns-coredns:1.14.2` -> `docker.io/rancher/mirrored-coredns-coredns:1.14.2`
  - `m.daocloud.io/docker.io/rancher/local-path-provisioner:v0.0.35` -> `docker.io/rancher/local-path-provisioner:v0.0.35`
  - `m.daocloud.io/docker.io/rancher/mirrored-metrics-server:v0.8.1` -> `docker.io/rancher/mirrored-metrics-server:v0.8.1`
  - `m.daocloud.io/docker.io/rancher/klipper-helm:v0.9.17-build20260422` -> `docker.io/rancher/klipper-helm:v0.9.17-build20260422`
  - `m.daocloud.io/docker.io/rancher/mirrored-library-traefik:3.6.13` -> `docker.io/rancher/mirrored-library-traefik:3.6.13`
  - `m.daocloud.io/docker.io/rancher/klipper-lb:v0.4.15` -> `docker.io/rancher/klipper-lb:v0.4.15`
  - `m.daocloud.io/docker.io/rancher/mirrored-library-busybox:1.37.0` -> `docker.io/rancher/mirrored-library-busybox:1.37.0`
- 完成后必要时重建 kube-system pod。

Step 3. 将 mirror 镜像缓存到 `/data/image-cache`
- 确保目录存在：`/data/image-cache`
- 对上述 mirror 镜像执行：
  - `k3s ctr -n k8s.io images pull <mirror-image>`
  - `k3s ctr -n k8s.io images export /data/image-cache/<sanitized-name>.tar <mirror-image>`
- 如未存在，创建 `/data/image-cache-usage.md`（包含 import all / single / retag 示例）。

Step 4. 部署集群内 registry（NodePort 31666）
- namespace: `registry`
- deployment image: `m.daocloud.io/docker.io/library/registry:2`
- service: NodePort `31666` -> `5000`
- 验证：`kubectl get pods -n registry` 和 `kubectl get svc -n registry`

Step 5. 准备 chromium-bridge 源码与 chart 依赖
- 将本地仓库 `application/chromium-bridge` 同步到远端工作目录（例如 `/root/workspace/chromium-bridge`）。
- 处理 chart dependency `bitnami/common`：
  - 优先尝试 `oci://m.daocloud.io/registry-1.docker.io/bitnamicharts/common`
  - 若可拉取，放入 `chart/charts/common-*.tgz` 或 `helm dependency update`
- 先执行 `helm template` 验证模板可渲染。

Step 6. 在 ECS 本机构建并推送镜像
- 构建：
  - `127.0.0.1:31666/chromium-bridge-server:0.1.0`
  - `127.0.0.1:31666/chromium-bridge-novnc:0.1.0`
- push 到本机 registry（可关闭 tls verify）。
- 验证 `podman images` 与 push 输出。

Step 7. 缓存 chromium-bridge 镜像到 `/data`
- 保存 tar 到：
  - `/data/image-cache/chromium-bridge/chromium-bridge-server_0.1.0.tar`
  - `/data/image-cache/chromium-bridge/chromium-bridge-novnc_0.1.0.tar`
- 然后 `k3s ctr -n k8s.io images import` 一次，确保 containerd 可直接命中。

Step 8. Helm 安装 chromium-bridge
- namespace: `chromium-bridge`
- 使用 values 文件：`application/chromium-bridge/chart/values-ecs.yaml`
- 若直接在远端执行 helm，确保设置：
  - `export KUBECONFIG=/etc/rancher/k3s/k3s.yaml`
- 验证：
  - `kubectl get pods -n chromium-bridge -o wide`
  - `kubectl get svc -n chromium-bridge -o wide`

Step 9. 功能验证（必须）
- CDP:
  - `curl -sS http://127.0.0.1:30222/json/version`
  - 必须出现 `webSocketDebuggerUrl`
- noVNC:
  - `curl -I http://127.0.0.1:30080/`
  - 必须返回 `HTTP/1.1 200`
- 检查 server/novnc 日志，确认无致命错误。

Step 10. Playwright connectOverCDP 再验证
- 在 ECS 安装 node/npm（若缺失）并安装 `playwright`。
- 脚本连接 `http://127.0.0.1:30222`，访问 `https://example.com`，输出 title，保存截图。
- 截图文件例如：`/root/chromium-bridge-e2e/cdp-e2e.png`

【最终输出格式要求】
最后只输出以下内容（不要长篇解释）：
1) 执行结果总览（成功/失败）
2) 关键资源状态（k3s、registry、pods、services）
3) CDP/noVNC/Playwright 验证结果
4) 产物路径（镜像 tar、截图、usage 文档）
5) 若失败，给出下一步最小修复动作（最多 3 条）
```
