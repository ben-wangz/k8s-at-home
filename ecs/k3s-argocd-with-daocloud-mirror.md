# 在 ECS 上安装 k3s + ArgoCD（m.daocloud.io 稳定版流程）

本文档是可复现流程，适用于“直连 Docker Hub / GHCR / Quay / ECR 不稳定”的 ECS 环境。

核心策略：

- 正常安装 k3s 与 ArgoCD
- 关键镜像通过 `m.daocloud.io` 预拉
- 预拉后重命名回原始镜像地址，确保 kubelet 命中本地缓存
- 对慢拉镜像使用后台任务 + 日志文件，避免 SSH 会话中断

## 1) 安装 k3s

```bash
curl -sfL https://rancher-mirror.rancher.cn/k3s/k3s-install.sh | INSTALL_K3S_MIRROR=cn sh -
```

验证：

```bash
kubectl get nodes -o wide
kubectl get pods -n kube-system -o wide
```

## 2) 预拉 k3s 基础镜像并重命名

```bash
pull_and_tag() {
  src="$1"
  dst="$2"
  k3s ctr -n k8s.io images pull "$src"
  k3s ctr -n k8s.io images tag "$src" "$dst"
}

pull_and_tag m.daocloud.io/docker.io/rancher/mirrored-pause:3.6 docker.io/rancher/mirrored-pause:3.6
pull_and_tag m.daocloud.io/docker.io/rancher/mirrored-coredns-coredns:1.14.2 docker.io/rancher/mirrored-coredns-coredns:1.14.2
pull_and_tag m.daocloud.io/docker.io/rancher/local-path-provisioner:v0.0.35 docker.io/rancher/local-path-provisioner:v0.0.35
pull_and_tag m.daocloud.io/docker.io/rancher/mirrored-metrics-server:v0.8.1 docker.io/rancher/mirrored-metrics-server:v0.8.1
pull_and_tag m.daocloud.io/docker.io/rancher/klipper-helm:v0.9.17-build20260422 docker.io/rancher/klipper-helm:v0.9.17-build20260422
pull_and_tag m.daocloud.io/docker.io/rancher/mirrored-library-traefik:3.6.13 docker.io/rancher/mirrored-library-traefik:3.6.13
pull_and_tag m.daocloud.io/docker.io/rancher/klipper-lb:v0.4.15 docker.io/rancher/klipper-lb:v0.4.15
pull_and_tag m.daocloud.io/docker.io/rancher/mirrored-library-busybox:1.37.0 docker.io/rancher/mirrored-library-busybox:1.37.0
```

重建 `kube-system` Pod 触发重新拉起：

```bash
kubectl delete pod -n kube-system --all
kubectl get pods -n kube-system -o wide
```

## 3) 安装 ArgoCD

```bash
kubectl create namespace argocd
kubectl apply -n argocd -f https://raw.githubusercontent.com/argoproj/argo-cd/v3.3.9/manifests/install.yaml
```

如果遇到 `applicationsets.argoproj.io` 注解过长（client-side apply 常见），改用 server-side apply：

```bash
kubectl apply --server-side -n argocd -f https://raw.githubusercontent.com/argoproj/argo-cd/v3.3.9/manifests/install.yaml
```

## 4) 预拉 ArgoCD 相关镜像并重命名

> `argocd:v3.3.9` 常较大，`dex` 和 `redis` 在网络抖动时也容易卡住。

```bash
pull_and_tag m.daocloud.io/quay.io/argoproj/argocd:v3.3.9 quay.io/argoproj/argocd:v3.3.9
pull_and_tag m.daocloud.io/ghcr.io/dexidp/dex:v2.43.0 ghcr.io/dexidp/dex:v2.43.0
pull_and_tag m.daocloud.io/public.ecr.aws/docker/library/redis:8.2.3-alpine public.ecr.aws/docker/library/redis:8.2.3-alpine
```

然后重建 ArgoCD Pod：

```bash
kubectl delete pod -n argocd --all
kubectl get pods -n argocd -w
```

## 5) 验证状态

```bash
kubectl get pods -n argocd -o wide
kubectl get crd | grep argoproj.io
```

期望：

- `argocd-application-controller` / `argocd-repo-server` / `argocd-server` / `argocd-applicationset-controller` / `argocd-notifications-controller` / `argocd-redis` / `argocd-dex-server` 全部 `Running`
- CRD 包含：
  - `applications.argoproj.io`
  - `applicationsets.argoproj.io`
  - `appprojects.argoproj.io`

## 6) 后台拉取（推荐，避免“看起来卡住”）

大镜像（例如 `argocd:v3.3.9`、`ghcr.io/berriai/litellm:*`）在某些网络下会长时间拉取。建议用后台脚本执行并写日志。

示例（按需修改镜像列表）：

```bash
cat >/root/prepull-images.sh <<'EOF'
#!/usr/bin/env bash
set -euo pipefail
pull_and_tag(){
  src="$1"
  dst="$2"
  echo "[$(date '+%F %T')] START $src -> $dst"
  k3s ctr -n k8s.io images pull "$src"
  k3s ctr -n k8s.io images tag "$src" "$dst"
  echo "[$(date '+%F %T')] DONE  $dst"
}
pull_and_tag m.daocloud.io/quay.io/argoproj/argocd:v3.3.9 quay.io/argoproj/argocd:v3.3.9
pull_and_tag m.daocloud.io/ghcr.io/dexidp/dex:v2.43.0 ghcr.io/dexidp/dex:v2.43.0
pull_and_tag m.daocloud.io/public.ecr.aws/docker/library/redis:8.2.3-alpine public.ecr.aws/docker/library/redis:8.2.3-alpine
EOF
chmod +x /root/prepull-images.sh
nohup /root/prepull-images.sh >/root/prepull-images.log 2>&1 &
echo $! >/root/prepull-images.pid
```

查看进度：

```bash
ps -fp "$(cat /root/prepull-images.pid)"
tail -n 80 /root/prepull-images.log
```

## 7) （可选）批量测试镜像是否可经 m.daocloud.io 拉取

```bash
python3 - <<'PY'
import subprocess
images = [
    'quay.io/argoproj/argocd:v3.3.9',
    'ghcr.io/dexidp/dex:v2.43.0',
    'public.ecr.aws/docker/library/redis:8.2.3-alpine',
    'ghcr.io/berriai/litellm:main-v1.83.14-stable',
    'docker.io/bitnamilegacy/postgresql:17.5.0-debian-12-r4',
    'docker.io/bitnamilegacy/redis:8.2.1-debian-12-r0',
]
for img in images:
    mirror = f'm.daocloud.io/{img}'
    p = subprocess.run(['k3s', 'ctr', '-n', 'k8s.io', 'images', 'pull', mirror], capture_output=True, text=True)
    print(f'{mirror}:', 'OK' if p.returncode == 0 else 'FAIL')
PY
```

## 常见问题

- `ErrImagePull` / `ImagePullBackOff`
  - 先确认 `m.daocloud.io/<原镜像地址>` 能拉
  - 成功后执行重命名到原始镜像地址
  - 删除对应 Pod 触发重建

- `local-path` PVC 一直 Pending
  - 常见原因是 helper pod 拉 `rancher/mirrored-library-busybox:1.37.0` 失败
  - 先执行 busybox 的预拉 + 重命名，再删除 helper pod 重试

- `FailedCreatePodSandBox` 且提示 `mirrored-pause` 拉取失败
  - 优先处理 `rancher/mirrored-pause:3.6` 的预拉与重命名

- ArgoCD apply 报 CRD annotation 太长
  - 使用 `kubectl apply --server-side ...`

- 通过公网地址 push OCI chart 超时
  - 优先在 ECS 机器本机执行 `helm push` 到本地 NodePort（例如 `127.0.0.1:31666`）
  - 避免从外部网络直连 NodePort 推送
